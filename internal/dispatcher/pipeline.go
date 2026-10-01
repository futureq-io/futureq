package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type ScanLimits struct {
	Candidates     int
	Examined       int
	Bytes          int
	WorkTime       time.Duration
	PrepareTimeout time.Duration
	Workers        int
}

func DefaultScanLimits() ScanLimits {
	return ScanLimits{Candidates: 64, Examined: 256, Bytes: raft.MaxDeliveryPrepareBytes, WorkTime: 5 * time.Millisecond, PrepareTimeout: 250 * time.Millisecond, Workers: 2}
}

type scanCursor struct {
	after []byte
	epoch uint64
}
type deliveryChunk struct {
	topic       string
	items       []raft.DeliveryPrepare
	scanStarted int64
	maintenance bool
	started     time.Time
	oldestDueMs int64
}
type preparedChunk struct {
	chunk  deliveryChunk
	states map[string]*raft.DeliveryState
	err    error
}

const maintenanceTopic = "\x00delivery-maintenance"

// runPipeline owns the cursors. At most one job per topic and Workers jobs
// overall can exist. A slow proposal occupies one worker, while another topic
// can scan and prepare. Results and candidate keys are bounded by those same
// slots; no goroutine or retained payload is allocated per event.
func (d *Dispatcher) runPipeline(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	jobs := make(chan deliveryChunk, d.Limits.Workers)
	results := make(chan preparedChunk, d.Limits.Workers)
	var workers sync.WaitGroup
	for i := 0; i < d.Limits.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case chunk := <-jobs:
					if !d.hub.DeliveryPermitted() {
						select {
						case results <- preparedChunk{chunk: chunk, err: errors.New("delivery permission expired before preparation")}:
						case <-ctx.Done():
							return
						}
						continue
					}
					callCtx, stop := context.WithTimeout(ctx, d.Limits.PrepareTimeout)
					states, err := d.prepareChunk(callCtx, chunk)
					stop()
					select {
					case results <- preparedChunk{chunk, states, err}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer func() { cancel(); workers.Wait(); metrics.DeliveryQueueDepth.WithLabelValues("prepare").Set(0) }()
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	busy := make(map[string]bool)
	busyDue := make(map[string]int64)
	rotation := 0
	nextMaintenance := time.Now()
	schedule := func() {
		d.hub.ExpireInFlight(time.Now())
		d.hub.ObserveQueues()
		oldest := int64(0)
		for _, due := range busyDue {
			if due > 0 {
				oldest = max(oldest, time.Now().UnixMilli()-due)
			}
		}
		metrics.DeliveryOldestPreparationLatenessMs.Set(float64(oldest))
		if len(busy) >= d.Limits.Workers {
			return
		}
		if d.hub.PermitNeedsRenewal(d.Limits.PrepareTimeout+d.Limits.WorkTime) && !d.refreshPermit(ctx) {
			return
		}
		if !d.hub.DeliveryPermitted() {
			return
		}
		topics := d.hub.ActiveTopics()
		sort.Strings(topics)
		live := make(map[string]bool, len(topics))
		for _, topic := range topics {
			live[topic] = true
		}
		for topic := range d.cursors {
			if topic != maintenanceTopic && !live[topic] && !busy[topic] {
				delete(d.cursors, topic)
			}
		}
		if d.MaintenanceRecipients != nil && !time.Now().Before(nextMaintenance) {
			topics = append(topics, maintenanceTopic)
		}
		if len(topics) == 0 {
			return
		}
		start := rotation % len(topics)
		for offset := 0; offset < len(topics) && len(busy) < d.Limits.Workers; offset++ {
			topic := topics[(start+offset)%len(topics)]
			if busy[topic] {
				continue
			}
			var chunk deliveryChunk
			if topic == maintenanceTopic {
				chunk = d.collectMaintenance()
				nextMaintenance = time.Now().Add(d.interval)
			} else {
				chunk = d.collectTopic(topic, time.Now().UnixMilli())
			}
			rotation = (start + offset + 1) % len(topics)
			if len(chunk.items) == 0 {
				continue
			}
			busy[topic] = true
			busyDue[topic] = chunk.oldestDueMs
			jobs <- chunk
		}
		metrics.DeliveryQueueDepth.WithLabelValues("prepare").Set(float64(len(busy)))
	}
	schedule()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			schedule()
		case <-d.wakeCh:
			schedule()
		case result := <-results:
			delete(busy, result.chunk.topic)
			delete(busyDue, result.chunk.topic)
			if result.err != nil {
				d.logger.Warn("delivery preparation failed; cyclic scan will retry", zap.Error(result.err))
			} else if d.refreshPermit(ctx) {
				// SyncRead catches a follower up after a remotely committed
				// preparation and revalidates metadata before accessing payloads.
				d.deliverChunk(result.chunk, result.states)
			}
			schedule()
		}
	}
}

func (d *Dispatcher) refreshPermit(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	started := time.Now()
	defer func() { metrics.ReadBarrierDurationMs.Observe(float64(time.Since(started).Microseconds()) / 1000) }()
	if d.ReadBarrier != nil {
		return d.ReadBarrier() == nil
	}
	if app.A != nil && app.A.NodeHost != nil {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := app.A.NodeHost.SyncRead(callCtx, app.A.Config().Cluster.ShardID, nil)
		return err == nil
	}
	return true
}

// A cursor is strictly a scan position, never a completion watermark. Exhausting
// the current due range wraps it to the beginning, revisiting insertions behind
// it, failed proposals/sends, NACKs, and timeout retries. Epoch changes reset it.
// Every examined key consumes work, including remote owners and in-flight keys.
func (d *Dispatcher) collectTopic(topic string, nowMs int64) deliveryChunk {
	started := time.Now()
	defer func() { metrics.DeliveryScanDurationMs.Observe(float64(time.Since(started).Microseconds()) / 1000) }()
	chunk := deliveryChunk{topic: topic, scanStarted: nowMs, started: started}
	view := d.hub.InterestView(topic)
	if !view.Active {
		return chunk
	}
	cursor := d.cursors[topic]
	if cursor == nil || cursor.epoch != view.Epoch {
		cursor = &scanCursor{epoch: view.Epoch}
		d.cursors[topic] = cursor
	}
	hash := utils.TopicHash(topic)
	lower := utils.TopicLowerBound(hash)
	if len(cursor.after) > 0 {
		lower = cursor.after
	}
	iter, err := d.db.NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: utils.DueUpperBound(hash, utils.CalculateBucket(nowMs, d.TimeBucket))})
	if err != nil {
		d.logger.Error("delivery iterator failed", zap.Error(err))
		return chunk
	}
	defer iter.Close() //nolint:errcheck
	examined, bytesRead, commandBytes := 0, 0, 3
	valid := iter.First()
	for valid {
		if examined >= d.Limits.Examined || len(chunk.items) >= d.Limits.Candidates || bytesRead >= d.Limits.Bytes || time.Since(started) >= d.Limits.WorkTime || !d.hub.DeliveryPermitted() {
			break
		}
		key, value := iter.Key(), iter.Value()
		examined++
		bytesRead += len(key) + len(value)
		// Appending zero is the inclusive lower bound strictly after this
		// fixed-length event key, without assuming IDs are consecutive.
		cursor.after = append(append([]byte(nil), key...), 0)
		if len(key) != 24 {
			valid = iter.Next()
			continue
		}
		metrics.DeliveryScannedKeysTotal.Inc()
		owners := view.localOwners(key)
		if len(owners) == 0 {
			metrics.DeliveryOwnershipFilteredTotal.Inc()
			valid = iter.Next()
			continue // receipt cleanup has its own bounded maintenance cursor
		}
		if !d.hub.hasReadyOwner(owners, key) {
			valid = iter.Next()
			continue
		}
		var msg storagepb.StoredMessage
		if err := proto.Unmarshal(value, &msg); err != nil {
			d.logger.Error("invalid stored message", zap.Error(err))
			valid = iter.Next()
			continue
		}
		now := time.Now().UnixMilli()
		if d.isExpired(&msg, now) {
			d.deleter.MarkDeleted(append([]byte(nil), key...))
			metrics.MessagesExpiredTotal.WithLabelValues(topic, "dispatcher").Inc()
			valid = iter.Next()
			continue
		}
		if now < msg.EnqueuedAtUnixMs+msg.DelayMs {
			valid = iter.Next()
			continue
		}
		item := raft.DeliveryPrepare{Key: append([]byte(nil), key...), Epoch: view.Epoch, Recipients: view.Recipients}
		encoded, err := json.Marshal(item)
		if err != nil || commandBytes+len(encoded)+1 > d.Limits.Bytes {
			// Revisit this item next chunk rather than advancing past a byte
			// boundary. A single oversized interest set is reported explicitly.
			cursor.after = append([]byte(nil), key...)
			if len(chunk.items) == 0 {
				d.logger.Error("delivery interests exceed preparation byte limit", zap.String("topic", topic))
			}
			break
		}
		commandBytes += len(encoded) + 1
		chunk.items = append(chunk.items, item)
		if due := msg.EnqueuedAtUnixMs + msg.DelayMs; chunk.oldestDueMs == 0 || due < chunk.oldestDueMs {
			chunk.oldestDueMs = due
		}
		valid = iter.Next()
	}
	if !valid {
		cursor.after = nil
	}
	if err := iter.Error(); err != nil {
		d.logger.Error("delivery scan failed", zap.Error(err))
		cursor.after = nil
	}
	return chunk
}

// Maintenance is independent of local ownership and active topics. It only
// updates existing receipts, so disconnected universal-only topics eventually
// release their payloads without freezing new recipients for remote messages.
func (d *Dispatcher) collectMaintenance() deliveryChunk {
	started := time.Now()
	defer func() { metrics.DeliveryScanDurationMs.Observe(float64(time.Since(started).Microseconds()) / 1000) }()
	chunk := deliveryChunk{topic: maintenanceTopic, maintenance: true, scanStarted: started.UnixMilli(), started: started}
	epochNow, activeNow := d.hub.DeliveryEpoch()
	if !activeNow {
		return chunk
	}
	prefix := []byte("metadata/delivery/")
	cursor := d.cursors[maintenanceTopic]
	if cursor == nil {
		cursor = &scanCursor{}
		d.cursors[maintenanceTopic] = cursor
	}
	lower := prefix
	if len(cursor.after) > 0 {
		lower = cursor.after
	}
	upper := append([]byte(nil), prefix...)
	upper[len(upper)-1]++
	iter, err := d.db.NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return chunk
	}
	defer iter.Close() //nolint:errcheck
	examined, bytesRead, commandBytes := 0, 0, 3
	valid := iter.First()
	for valid {
		if examined >= d.Limits.Examined || len(chunk.items) >= d.Limits.Candidates || bytesRead >= d.Limits.Bytes || time.Since(started) >= d.Limits.WorkTime || !d.hub.DeliveryPermitted() {
			break
		}
		key, value := iter.Key(), iter.Value()
		examined++
		bytesRead += len(key) + len(value)
		cursor.after = append(append([]byte(nil), key...), 0)
		if len(key) != len(prefix)+24 {
			valid = iter.Next()
			continue
		}
		var state raft.DeliveryState
		if json.Unmarshal(value, &state) != nil {
			valid = iter.Next()
			continue
		}
		if state.Epoch >= epochNow {
			valid = iter.Next()
			continue
		}
		eventKey := key[len(prefix):]
		payload, closer, err := d.db.Get(eventKey)
		if err != nil {
			valid = iter.Next()
			continue
		}
		var msg storagepb.StoredMessage
		err = proto.Unmarshal(payload, &msg)
		_ = closer.Close()
		if err != nil {
			valid = iter.Next()
			continue
		}
		epoch, recipients, active := d.MaintenanceRecipients(msg.Topic)
		if active && epoch > state.Epoch {
			item := raft.DeliveryPrepare{Key: append([]byte(nil), eventKey...), Epoch: epoch, Recipients: recipients}
			encoded, err := json.Marshal(item)
			if err != nil || commandBytes+len(encoded)+1 > d.Limits.Bytes {
				cursor.after = append([]byte(nil), key...)
				break
			}
			commandBytes += len(encoded) + 1
			chunk.items = append(chunk.items, item)
		}
		valid = iter.Next()
	}
	if !valid || iter.Error() != nil {
		cursor.after = nil
	}
	return chunk
}

func (d *Dispatcher) prepareChunk(ctx context.Context, chunk deliveryChunk) (map[string]*raft.DeliveryState, error) {
	if d.PrepareBatch != nil {
		return d.PrepareBatch(ctx, chunk.items)
	}
	states := make(map[string]*raft.DeliveryState, len(chunk.items))
	if d.PrepareDelivery != nil {
		for _, item := range chunk.items {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			state, err := d.PrepareDelivery(chunk.topic, item.Key)
			if err != nil {
				return nil, err
			}
			states[string(item.Key)] = state
		}
	}
	return states, nil
}

func (d *Dispatcher) deliverChunk(chunk deliveryChunk, states map[string]*raft.DeliveryState) int {
	if chunk.maintenance {
		return 0
	}
	defer func() {
		metrics.DispatchPassDurationMs.Observe(float64(time.Since(chunk.started).Microseconds()) / 1000)
	}()
	delivered := 0
	for _, item := range chunk.items {
		if !d.hub.DeliveryPermitted() {
			metrics.DeliveryRejectedTotal.WithLabelValues("permit").Inc()
			break
		}
		view := d.hub.InterestView(chunk.topic)
		if !view.Active || view.Epoch != item.Epoch {
			break
		}
		manifest := states[string(item.Key)]
		if (d.PrepareBatch != nil || d.PrepareDelivery != nil) && manifest == nil {
			continue
		}
		value, closer, err := d.db.Get(item.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			d.logger.Warn("prepared payload read failed", zap.Error(err))
			continue
		}
		var msg storagepb.StoredMessage
		err = proto.Unmarshal(value, &msg)
		_ = closer.Close()
		if err != nil {
			continue
		}
		now := time.Now().UnixMilli()
		if d.isExpired(&msg, now) {
			d.deleter.MarkDeleted(item.Key)
			continue
		}
		if now < msg.EnqueuedAtUnixMs+msg.DelayMs {
			continue
		}
		queued := &pb.QueueMessage{Topic: msg.Topic, Payload: msg.Payload, DeliveryTag: item.Key, EnqueuedAtUnixMs: msg.EnqueuedAtUnixMs, DelayMs: msg.DelayMs}
		sentTo := d.hub.DispatchPrepared(chunk.topic, queued, item.Key, manifest)
		if len(sentTo) == 0 {
			continue
		}
		delivered++
		// Preserve the legacy scan-start measurement's explicit semantics.
		metrics.DeliveryLatencyMs.WithLabelValues(chunk.topic).Observe(float64(max(0, chunk.scanStarted-msg.EnqueuedAtUnixMs)))
		metrics.DeliveryOverheadMs.WithLabelValues(chunk.topic).Observe(float64(max(0, chunk.scanStarted-msg.EnqueuedAtUnixMs-msg.DelayMs)))
		for _, group := range sentTo {
			metrics.MessagesDispatchedTotal.WithLabelValues(chunk.topic, group).Inc()
			metrics.MessagesInFlight.WithLabelValues(chunk.topic, group).Inc()
		}
	}
	return delivered
}
