package dispatcher

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// Entries own decoded protobuf data. Default proto.Unmarshal copies byte fields;
// no iterator-owned memory or recipient decisions survive discovery.
type preloadCandidate struct {
	key     [utils.EventKeySize]byte
	ready   bool // charged to the independent overdue admission allowance
	due     int64
	payload cachedPayload
}

// cachedPayload owns immutable message data, never cached delivery eligibility.
type cachedPayload struct {
	message *deliverypb.StoredDeliveryMessage
}

func cachePayload(value []byte) (cachedPayload, error) {
	var msg deliverypb.StoredDeliveryMessage
	metrics.DeliveryPayloadDecodesTotal.WithLabelValues("discovery").Inc()
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(value, &msg); err != nil {
		return cachedPayload{}, err
	}
	return cachedPayload{message: &msg}, nil
}

// readPayload loads a message when discovery supplied no cached payload.
// Event keys identify immutable message bytes for their entire lifetime,
// including replay and snapshot recovery. Receipts determine eligibility.
// Malformed values return an empty payload; storage failures return an error.
func (d *Dispatcher) readPayload(key []byte, stage string) (cachedPayload, error) {
	value, closer, err := d.db.Get(key)
	if err != nil {
		return cachedPayload{}, err
	}
	defer closer.Close() //nolint:errcheck
	msg := &deliverypb.StoredDeliveryMessage{}
	metrics.DeliveryPayloadDecodesTotal.WithLabelValues(stage).Inc()
	if (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(value, msg) != nil {
		return cachedPayload{}, nil
	}
	return cachedPayload{message: msg}, nil
}

type candidateHeap []preloadCandidate

type topicPreload struct {
	heapShift        uint // 0/1: typed binary; 2/3: typed 4-ary/8-ary
	adaptiveHeap     bool // only the scheduler's retained heap adapts
	heapWork         candidateHeapWork
	readyCount       int          // scheduler-owned due credits; future entries are charged separately
	repairGeneration uint64       // fences cursor resets against older scan results
	valid            *atomic.Bool // optional generation fence, set by the production owner
	epoch            uint64
	assignmentEpoch  uint64
	after            []byte // independent of the live due cursor; never a watermark
	items            candidateHeap
	keyMu            sync.RWMutex // protects keys only; never held across storage or Hub calls
	keys             map[[utils.EventKeySize]byte]bool
	scanNext         bool
	futureScanAfter  time.Time // scheduler-owned speculative scan deadline
	futureScanIdle   time.Duration
	shardScans       []shardScanState // scheduler-owned cursors for parallel discovery
	nextShardScan    int
	nextDueShardScan int
	sharded          bool
	layouts          int
	layoutRevision   uint64
	layoutProbeAt    time.Time
	revision         uint64
	revisionWatch    *storage.TopicRevision
	shardRevisions   *storage.TopicRevisionSnapshot
	dueBeforeRefresh bool
}

// Discovery reads this small synchronized index without copying the entire
// preload. Heaps and accounting remain exclusively scheduler-owned.
func (p *topicPreload) hasKey(key [utils.EventKeySize]byte) bool {
	p.keyMu.RLock()
	defer p.keyMu.RUnlock()
	return p.keys[key]
}
func (p *topicPreload) addKey(key [utils.EventKeySize]byte) {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if p.keys == nil {
		p.keys = make(map[[utils.EventKeySize]byte]bool)
	}
	p.keys[key] = true
}
func (p *topicPreload) removeKey(key [utils.EventKeySize]byte) {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	delete(p.keys, key)
}
func (p *topicPreload) clearKeys() {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	p.keys = nil
}

// collectScheduledTopic alternates cached chunks with live due scans. Even a
// continuously full cache cannot hide a newly inserted overdue message, a NACK,
// or a failed preparation behind a future scan position. The ordinary cyclic
// scan remains the recovery path for every discarded or failed candidate.
func (d *Dispatcher) collectScheduledTopic(topic string, nowMs int64) deliveryChunk {
	view := d.hub.InterestView(topic)
	if !view.Active {
		d.dropPreload(topic)
		return deliveryChunk{topic: topic}
	}
	if d.preloads == nil {
		d.preloads = make(map[string]*topicPreload)
	}
	p := d.preloads[topic]
	if p == nil || p.epoch != view.Epoch {
		d.dropPreload(topic)
		p = &topicPreload{epoch: view.Epoch, heapShift: candidateHeapShift(d.HeapType), adaptiveHeap: d.HeapType == config.HeapAuto}
		d.preloads[topic] = p
	}
	if !p.scanNext {
		chunk := d.collectPreloaded(topic, p, view, nowMs)
		if len(chunk.items) > 0 {
			p.scanNext = true
			return chunk
		}
	}
	p.scanNext = false
	scanned := d.collectTopic(topic, nowMs)
	d.admitChunk(p, scanned, nil)
	if len(scanned.items) == 0 && d.PreloadLookahead > 0 && d.PreloadMaxMessages > 0 {
		d.preloadTopic(topic, p, view, nowMs)
	}
	return d.collectPreloaded(topic, p, view, nowMs)
}

func (d *Dispatcher) collectPreloaded(topic string, p *topicPreload, view InterestView, nowMs int64) deliveryChunk {
	started := time.Now()
	chunk := deliveryChunk{topic: topic, scanStarted: nowMs, started: started, payloads: make(map[string]cachedPayload), generation: p.valid}
	defer func() {
		observeCollection("preload_ready", started)
		d.observePreload()
	}()
	examined := 0
	var commandSize raft.DeliveryPrepareSizer
	for len(p.items) > 0 && p.items[0].due <= nowMs {
		if examined >= d.Limits.Examined || len(chunk.items) >= d.Limits.Candidates || (examined > 0 && time.Since(started) >= d.Limits.WorkTime) || !d.hub.DeliveryPermitted() {
			break
		}
		candidate := p.items[0]
		item := raft.DeliveryPrepare{Key: append([]byte(nil), candidate.key[:]...), Epoch: view.Epoch, Recipients: view.Recipients}
		if commandSize.SizeWith(item) > d.Limits.Bytes && len(chunk.items) > 0 {
			break // retain this candidate for a subsequent bounded chunk
		}
		p.removeCandidateHead()
		p.removeKey(candidate.key)
		if candidate.ready {
			p.readyCount--
			d.readyCount--
		} else {
			d.preloadCount--
		}
		examined++
		if commandSize.SizeWith(item) > d.Limits.Bytes {
			chunk.unfit = true
			d.logger.Error("delivery interests exceed preparation byte limit", zap.String("topic", topic), zap.Int("limit", d.Limits.Bytes))
			metrics.DeliveryRejectedTotal.WithLabelValues("prepare_bytes").Inc()
			break // retry on bounded backoff with a fresh recipient view
		}
		if _, ready := d.hub.hasReadyOwner(view, item.Key); !ready {
			continue
		}
		commandSize.Add(item)
		source := "preload"
		if candidate.ready {
			source = "scan"
		}
		metrics.DeliveryCandidatesTotal.WithLabelValues(source).Inc()
		chunk.items = append(chunk.items, item)
		chunk.payloads[string(item.Key)] = candidate.payload
		if chunk.oldestDueMs == 0 || candidate.due < chunk.oldestDueMs {
			chunk.oldestDueMs = candidate.due
		}
	}
	p.compact()
	return chunk
}

// preloadTopic reads only the moving lookahead range, with its own cyclic
// cursor and the existing key/byte/time budgets. Full buffers stop speculative
// reads, while live due scans continue independently. Coarse buckets require
// checking the exact message due time before retaining a candidate. It returns
// true only when the range was exhausted without an iterator error.
func (d *Dispatcher) preloadTopic(topic string, p *topicPreload, view InterestView, nowMs int64) bool {
	defer d.observePreload()
	if !view.Active || !d.shardScan.matches(view) || d.preloadCount >= d.PreloadMaxMessages {
		return false
	}
	started := time.Now()
	loaded, scanned, filtered := 0, 0, 0
	defer func() {
		observeCollection("preload_scan", started)
		metrics.DeliveryPreloadLoadedTotal.Add(float64(loaded))
		metrics.DeliveryPreloadScannedKeysTotal.Add(float64(scanned))
		metrics.DeliveryScannedKeysTotal.Add(float64(scanned))
		metrics.DeliveryOwnershipFilteredTotal.Add(float64(filtered))
	}()
	horizon := nowMs + d.PreloadLookahead.Milliseconds()
	if horizon < nowMs { // saturate rather than wrap a very large configured duration
		horizon = int64(^uint64(0) >> 1)
	}
	hash := utils.TopicHash(topic)
	earliest := utils.CalculateBucket(nowMs, d.TimeBucket)
	last := utils.CalculateBucket(horizon, d.TimeBucket)
	lower := utils.EventKey(earliest, hash, 0)
	if bytes.Compare(p.after, lower) > 0 {
		lower = p.after
	}
	var upper []byte
	if !view.sharded {
		upper = utils.DueUpperBound(hash, last)
	}
	iter, err := openDiscoveryIterator(d.discoverySource(), view, hash, lower, upper, earliest, last, started.Add(d.Limits.WorkTime), d.shardScan)
	if err != nil {
		d.logger.Warn("preload iterator failed", zap.Error(err))
		return false
	}
	defer iter.Close() //nolint:errcheck
	examined, bytesRead := 0, 0
	valid := iter.First()
	for valid {
		if d.scanPayload.full() || d.preloadCount >= d.PreloadMaxMessages || examined >= d.Limits.Examined || bytesRead >= d.Limits.Bytes ||
			(examined > 0 && time.Since(started) >= d.Limits.WorkTime) || !d.hub.DeliveryPermitted() {
			break
		}
		key := iter.Key()
		examined++
		bytesRead += len(key)
		metrics.DeliveryDiscoveryBytesTotal.Add(float64(len(key)))
		p.after = append(append([]byte(nil), key...), 0)
		if len(key) == utils.EventKeySize {
			scanned++
			if p.hasKey([utils.EventKeySize]byte(key)) || (d.skipKeys != nil && d.skipKeys([utils.EventKeySize]byte(key))) {
				valid = iter.Next()
				continue
			}
			if !view.hasLocalOwner(key) {
				filtered++
				valid = iter.Next()
				continue
			}
			candidate := preloadCandidate{key: [utils.EventKeySize]byte(key)}
			value := iter.Value()
			bytesRead += len(value)
			metrics.DeliveryDiscoveryBytesTotal.Add(float64(len(value)))
			// Reject out-of-window entries without copying payloads or decoding indexes.
			var header deliverypb.StoredMessageMetadata
			if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(value, &header); err != nil {
				valid = iter.Next()
				continue
			}
			due := header.EnqueuedAtUnixMs + header.DelayMs
			if due <= nowMs || due > horizon || d.isExpired(&header, nowMs) {
				valid = iter.Next()
				continue
			}
			if !d.scanPayload.reserve(len(value)) {
				p.after = append(p.after[:0], key...)
				break
			}
			payload, err := cachePayload(value)
			if err == nil {
				msg := payload.message
				candidate.payload = payload
				candidate.due = msg.EnqueuedAtUnixMs + msg.DelayMs
				if candidate.due > nowMs && candidate.due <= horizon && !d.isExpired(msg, nowMs) {
					p.pushCandidate(candidate)
					p.addKey(candidate.key)
					d.preloadCount++
					loaded++
				}
			}
		}
		valid = iter.Next()
	}
	if iter.resumeAt != nil {
		p.after = iter.resumeAt
	} else if !valid || iter.Error() != nil {
		p.after = nil
	}
	return !valid && iter.resumeAt == nil && iter.Error() == nil
}

func (d *Dispatcher) dropPreload(topic string) {
	if p := d.preloads[topic]; p != nil {
		if p.revisionWatch != nil {
			p.revisionWatch.Close()
		}
		if p.valid != nil {
			p.valid.Store(false)
		}
		for _, candidate := range p.items {
			if candidate.ready {
				d.readyCount--
			} else {
				d.preloadCount--
			}
		}
		p.items = nil
		p.clearKeys()
		p.readyCount = 0
		delete(d.preloads, topic)
	}
	d.observePreload()
}

func (d *Dispatcher) prunePreloads(live map[string]bool) {
	epoch, active := d.hub.DeliveryEpoch()
	for topic, p := range d.preloads {
		if !live[topic] || !active || p.epoch != epoch {
			d.dropPreload(topic)
		}
	}
}

// Each collection path calls this once, after closing its iterator. Keeping the
// aggregate and path samples together prevents preload work disappearing from
// the existing scan-cost panels. Cache residency time is deliberately excluded.
func observeCollection(path string, started time.Time) {
	elapsed := float64(time.Since(started).Microseconds()) / 1000
	metrics.DeliveryScanDurationMs.Observe(elapsed)
	metrics.DeliveryCollectionDurationMs.WithLabelValues(path).Observe(elapsed)
}

// Only the scheduler accesses preload state. Prometheus scrapes read the gauges,
// never the heap or maps, so collection cannot race with scheduler mutations.
func (d *Dispatcher) observePreload() {
	if d.scanOnly {
		return
	}
	metrics.DeliveryQueueDepth.WithLabelValues("preload").Set(float64(d.preloadCount))
	capacity := 0
	if d.PreloadLookahead > 0 && d.PreloadMaxMessages > 0 {
		capacity = d.PreloadMaxMessages
	}
	metrics.DeliveryPreloadCapacity.Set(float64(capacity))
}

// compact bounds backing capacity and clears drained maps without keeping
// payload references in unused heap slots.
func (p *topicPreload) compact() {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if len(p.items) == 0 {
		p.items, p.keys = nil, nil // release capacity when a topic drains
	} else if len(p.items) <= cap(p.items)/4 {
		items := make(candidateHeap, len(p.items))
		copy(items, p.items)
		p.items = items
		p.keys = make(map[[utils.EventKeySize]byte]bool, len(items))
		for _, candidate := range items {
			p.keys[candidate.key] = true
		}
	}
}

// releaseReady frees shared due credits when a topic cannot currently assign
// another job. Future entries keep their separate cache allowance. Rewind the
// repair cursor so evicted payloads remain recoverable without waiting for an
// entire forward pass, and fence older cursor results against that rewind.
func (d *Dispatcher) releaseReady(topic string, p *topicPreload) {
	if p.readyCount == 0 {
		return
	}
	kept := p.items[:0]
	for _, candidate := range p.items {
		if candidate.ready {
			p.removeKey(candidate.key)
			d.readyCount--
			p.readyCount--
		} else {
			kept = append(kept, candidate)
		}
	}
	clear(p.items[len(kept):])
	p.items = kept
	p.heapWork = candidateHeapWork{}
	if len(p.items) == 0 && p.adaptiveHeap {
		p.setHeapShift(binaryHeapShift)
	}
	p.items.initCandidates(p.heapShift)
	p.compact()
	p.repairGeneration++
	d.forgetCursor(topic)
}
