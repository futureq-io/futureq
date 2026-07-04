package dispatcher

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/storage"

	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
)

// inFlightEntry tracks a single message that has been sent to a consumer but
// not yet acknowledged.
type inFlightEntry struct {
	dispatchedAt time.Time
	consumerID   string
	topic        string
	groupID      string
}

// Dispatcher scans the Pebble database for messages that are due for delivery
// and dispatches them to connected consumers via the Hub.
//
// Key design choices:
//   - Uses Pebble snapshot-based iteration (never blocks concurrent writes)
//   - Only scans topics with connected consumers (active-topic set from Hub)
//   - Tracks in-flight messages per consumer; cleans up on consumer disconnect
//   - Performs TTL checks at dispatch time; expired messages are batched for deletion
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
// it eligible for re-dispatch if it still exists in Pebble.
func (d *Dispatcher) RemoveInFlight(key []byte) {
	d.inFlight.Delete(key)
}

// RemoveInFlightBatch removes multiple keys from the in-flight tracker.
// Called by the state machine's OnDeleteKeys callback after Raft applies a
// DeleteBatchCmd — at that point the keys are gone from all replicas.
func (d *Dispatcher) RemoveInFlightBatch(keys [][]byte) {
	for _, k := range keys {
		d.inFlight.Delete(k)
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
			d.doPass()
			timer.Reset(d.interval)
		case <-timer.C:
			dispatched := d.doPass()
			if dispatched > 0 {
				// More messages may be ready — re-scan without delay.
				timer.Reset(0)
			} else {
				timer.Reset(d.interval)
			}
		}
	}
}

// doPass performs one scan of the Pebble database for due messages and
// dispatches them to consumers. Returns the number of messages dispatched.
func (d *Dispatcher) doPass() int {
	if !d.hub.HasConsumers() {
		return 0
	}

	// In Raft mode, only the leader dispatches messages.
	if app.A.NodeHost != nil {
		shardID := app.A.Config().Raft.ClusterID
		leaderID, _, valid, err := app.A.NodeHost.GetLeaderID(shardID)
		if err != nil || !valid || leaderID != app.A.Config().Raft.NodeID {
			return 0
		}
	}

	// Get active (topic, group) pairs from the Hub.
	activeTopics := d.hub.ActiveTopics()
	if len(activeTopics) == 0 {
		return 0
	}

	// Compute topic hashes (Hub doesn't import utils to avoid circular deps).
	for i := range activeTopics {
		activeTopics[i].TopicHash = utils.TopicHash(activeTopics[i].Topic)
	}

	nowMs := time.Now().UnixMilli()
	nowBucket := utils.CalculateBucket(nowMs, app.A.Config().Storage.TimeBucketSize)
	upperBound := utils.BucketUpperBound(nowBucket)

	iter, err := d.db.NewIter(&storage.IterOptions{
		UpperBound: upperBound,
	})

	if err != nil {
		d.logger.Error("failed to create iterator", zap.Error(err))
		return 0
	}
	defer iter.Close() //nolint:errcheck

	// Build a set of active topic hashes for O(1) lookup during iteration.
	type topicGroupKey struct {
		topicHash uint64
		groupID   string
	}
	activeSet := make(map[topicGroupKey]string, len(activeTopics)) // → topic name
	for _, at := range activeTopics {
		activeSet[topicGroupKey{at.TopicHash, at.GroupID}] = at.Topic
	}

	dispatched := 0
	var expiredKeys [][]byte

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		_, topicHash, _, err := utils.ParseEventKey(key)
		if err != nil {
			d.logger.Error(
				"failed to parse event key",
				zap.ByteString("key", key),
				zap.Error(err),
			)
			continue
		}

		// Check in-flight status.
		if entry, exists := d.inFlight.Load(key); exists {
			e := entry.(*inFlightEntry)
			if time.Since(e.dispatchedAt) < d.inFlightTimeout {
				continue
			}
			// Timed out — allow re-dispatch.
			d.inFlight.Delete(key)
		}

		// Deserialize the stored message.
		val := iter.Value()
		var msg storagepb.StoredMessage
		if err := proto.Unmarshal(val, &msg); err != nil {
			d.logger.Error("failed to unmarshal stored message", zap.Error(err))
			continue
		}

		// TTL check: skip and collect for deletion if expired.
		if msg.TtlMs > 0 {
			expiresAt := msg.EnqueuedAtUnixMs + msg.TtlMs
			if nowMs >= expiresAt {
				keyCopy := make([]byte, len(key))
				copy(keyCopy, key)
				expiredKeys = append(expiredKeys, keyCopy)
				continue
			}
		}

		// Dispatch to each active group that subscribes to this topic.
		sentAny := false
		for _, at := range activeTopics {
			if at.TopicHash != topicHash {
				continue
			}

			keyCopy := make([]byte, len(key))
			copy(keyCopy, key)

			qMsg := &pb.QueueMessage{
				Topic:            msg.Topic,
				Payload:          msg.Payload,
				DeliveryTag:      keyCopy,
				EnqueuedAtUnixMs: msg.EnqueuedAtUnixMs,
				DelayMs:          msg.DelayMs,
			}

			consumerID := d.hub.DispatchToGroup(at.Topic, at.GroupID, qMsg, key)
			if consumerID == "" {
				// No available consumer in this group right now.
				continue
			}

			sentAny = true

			// Record in-flight.
			d.inFlight.Store(key, &inFlightEntry{
				dispatchedAt: time.Now(),
				consumerID:   consumerID,
				topic:        at.Topic,
				groupID:      at.GroupID,
			})
		}

		if sentAny {
			dispatched++
		}
	}

	// Batch-delete expired messages.
	if len(expiredKeys) > 0 {
		for _, k := range expiredKeys {
			d.deleter.MarkDeleted(k)
		}
		d.logger.Debug("queued expired messages for deletion", zap.Int("count", len(expiredKeys)))
	}

	return dispatched
}
