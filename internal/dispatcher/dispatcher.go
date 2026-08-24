package dispatcher

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/internal/storage"

	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
)

// inFlightEntry tracks a single message that has been sent to a consumer but
// not yet acknowledged.
type inFlightEntry struct {
	dispatchedAt time.Time
	topic        string
}

// Dispatcher scans the storage engine for messages that are due for delivery
// and dispatches them to connected consumers via the Hub.
//
// Key design choices:
//   - Per-topic range scans using the topic-first key layout
//   - Only scans topics with connected consumers (active-topic set from Hub)
//   - Tracks in-flight messages per key; cleans up on consumer disconnect
//   - Performs TTL checks at dispatch time; expired messages are batched for deletion
//   - Snapshot-based iteration (never blocks concurrent writes)
type Dispatcher struct {
	db              storage.DB
	hub             *Hub
	deleter         *Deleter
	logger          *zap.Logger
	interval        time.Duration
	inFlightTimeout time.Duration
	wakeCh          chan struct{}
	inFlight        sync.Map // key: string(pebbleKey) → *inFlightEntry
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(
	db storage.DB,
	hub *Hub,
	deleter *Deleter,
	interval time.Duration,
	inFlightTimeout time.Duration,
	wakeCh chan struct{},
	logger *zap.Logger,
) *Dispatcher {
	return &Dispatcher{
		db:              db,
		hub:             hub,
		deleter:         deleter,
		logger:          logger.Named("dispatcher"),
		interval:        interval,
		inFlightTimeout: inFlightTimeout,
		wakeCh:          wakeCh,
	}
}

// RemoveInFlight removes a message from the in-flight tracker by key, making
// it eligible for re-dispatch if it still exists in storage.
func (d *Dispatcher) RemoveInFlight(key []byte) {
	d.inFlight.Delete(string(key))
}

// RemoveInFlightBatch removes multiple keys from the in-flight tracker.
// Called by the state machine's OnDeleteKeys callback after Raft applies a
// DeleteBatchCmd — at that point the keys are gone from all replicas.
func (d *Dispatcher) RemoveInFlightBatch(keys [][]byte) {
	for _, k := range keys {
		d.inFlight.Delete(string(k))
	}
}

// Run is the dispatcher event loop. It blocks until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	timer := time.NewTimer(d.interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wakeCh:
			// A consumer connected — scan immediately.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			d.dispatchAll()
			timer.Reset(d.interval)
		case <-timer.C:
			dispatched := d.dispatchAll()
			if dispatched > 0 {
				// More messages may be ready — re-scan without delay.
				timer.Reset(0)
			} else {
				timer.Reset(d.interval)
			}
		}
	}
}

// dispatchAll performs one full dispatch pass across all active topics.
// Returns the total number of messages dispatched.
func (d *Dispatcher) dispatchAll() int {
	if !d.hub.HasConsumers() {
		return 0
	}

	// In Raft mode, only the leader dispatches messages.
	if !d.isLeader() {
		return 0
	}

	activeTopics := d.hub.ActiveTopics()
	if len(activeTopics) == 0 {
		return 0
	}

	start := time.Now()
	nowMs := start.UnixMilli()
	totalDispatched := 0

	for _, topic := range activeTopics {
		dispatched := d.dispatchTopic(topic, nowMs)
		totalDispatched += dispatched
	}

	metrics.DispatchPassDurationMs.Observe(float64(time.Since(start).Milliseconds()))

	return totalDispatched
}

// isLeader returns true if this node should dispatch messages.
// In standalone mode (no Raft), always returns true.
func (d *Dispatcher) isLeader() bool {
	if app.A.NodeHost == nil {
		return true
	}
	shardID := app.A.Config().Raft.ClusterID
	leaderID, _, valid, err := app.A.NodeHost.GetLeaderID(shardID)
	if err != nil || !valid {
		return false
	}
	return leaderID == app.A.Config().Raft.NodeID
}

// dispatchTopic scans a single topic's key range and dispatches due messages.
// Returns the number of messages dispatched for this topic.
//
// The scan is bounded to buckets ≤ nowBucket so future (not-yet-due) messages
// are never iterated. The key layout is [topicHash][bucket][eventID], so the
// exclusive upper bound [topicHash][nowBucket+1][0...] covers all and only
// the due messages for this topic.
func (d *Dispatcher) dispatchTopic(topic string, nowMs int64) int {
	topicHash := utils.TopicHash(topic)
	nowBucket := utils.CalculateBucket(nowMs, app.A.Config().Storage.TimeBucketSize)

	dispatched := 0
	var expiredKeys [][]byte

	// Per-topic range scan over due buckets only: [topicHash, topicHash | nowBucket+1).
	// Scan manages the snapshot/iterator lifecycle internally — lower overhead
	// than NewIter since there's no manual resource management.
	err := d.db.Scan(&storage.IterOptions{
		LowerBound: utils.TopicLowerBound(topicHash),
		UpperBound: utils.DueUpperBound(topicHash, nowBucket),
	}, func(key, val []byte) error {
		// Check in-flight status.
		if d.isInFlight(key) {
			return nil
		}

		// Deserialize the stored message.
		var msg storagepb.StoredMessage
		if err := proto.Unmarshal(val, &msg); err != nil {
			d.logger.Error("failed to unmarshal stored message", zap.Error(err))
			return nil
		}

		// TTL check: skip and collect for deletion if expired.
		if d.isExpired(&msg, nowMs) {
			keyCopy := make([]byte, len(key))
			copy(keyCopy, key)
			expiredKeys = append(expiredKeys, keyCopy)
			metrics.MessagesExpiredTotal.WithLabelValues(topic, "dispatcher").Inc()
			return nil
		}

		// Build the queue message. Must copy key — it's only valid during yield.
		keyCopy := make([]byte, len(key))
		copy(keyCopy, key)

		qMsg := &pb.QueueMessage{
			Topic:            msg.Topic,
			Payload:          msg.Payload,
			DeliveryTag:      keyCopy,
			EnqueuedAtUnixMs: msg.EnqueuedAtUnixMs,
			DelayMs:          msg.DelayMs,
		}

		// Dispatch to all eligible consumers on this topic.
		sentTo := d.hub.DispatchToTopic(topic, qMsg, keyCopy)
		if len(sentTo) > 0 {
			// Track in-flight for timeout-based redelivery.
			d.inFlight.Store(string(keyCopy), &inFlightEntry{
				dispatchedAt: time.Now(),
				topic:        topic,
			})
			dispatched++

			// Delivery latency: total time from enqueue to dispatch.
			latencyMs := float64(nowMs - msg.EnqueuedAtUnixMs)
			if latencyMs < 0 {
				latencyMs = 0 // clock skew guard
			}
			metrics.DeliveryLatencyMs.WithLabelValues(topic).Observe(latencyMs)

			// Overhead: how late we are relative to the scheduled time.
			scheduledMs := msg.EnqueuedAtUnixMs + msg.DelayMs
			overheadMs := float64(nowMs - scheduledMs)
			if overheadMs < 0 {
				overheadMs = 0 // dispatched early (shouldn't happen, but guard)
			}
			metrics.DeliveryOverheadMs.WithLabelValues(topic).Observe(overheadMs)

			for _, gid := range sentTo {
				metrics.MessagesDispatchedTotal.WithLabelValues(topic, gid).Inc()
				metrics.MessagesInFlight.WithLabelValues(topic, gid).Inc()
			}
		}

		return nil
	})

	if err != nil {
		d.logger.Error("scan error",
			zap.String("topic", topic),
			zap.Error(err),
		)
	}

	// Batch-delete expired messages.
	if len(expiredKeys) > 0 {
		for _, k := range expiredKeys {
			d.deleter.MarkDeleted(k)
		}
		d.logger.Debug("queued expired messages for deletion",
			zap.String("topic", topic),
			zap.Int("count", len(expiredKeys)),
		)
	}

	return dispatched
}

// isInFlight checks if a key is currently in-flight and not yet timed out.
// If the entry has timed out, it is removed and the key is eligible for
// re-dispatch.
func (d *Dispatcher) isInFlight(key []byte) bool {
	entry, exists := d.inFlight.Load(string(key))
	if !exists {
		return false
	}

	e := entry.(*inFlightEntry)
	if time.Since(e.dispatchedAt) < d.inFlightTimeout {
		return true
	}

	// Timed out — allow re-dispatch.
	d.inFlight.Delete(string(key))
	return false
}

// isExpired returns true if the message's TTL has elapsed.
func (d *Dispatcher) isExpired(msg *storagepb.StoredMessage, nowMs int64) bool {
	if msg.TtlMs <= 0 {
		return false
	}
	return nowMs >= msg.EnqueuedAtUnixMs+msg.TtlMs
}
