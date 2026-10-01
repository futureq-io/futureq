package dispatcher

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/futureq-io/futureq/internal/app"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"

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
	// Installed before Run. Cluster reads also refresh the bounded metadata permit.
	ReadBarrier           func() error
	PrepareDelivery       func(string, []byte) (*raft.DeliveryState, error) // legacy single-key hook
	PrepareBatch          func(context.Context, []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error)
	MaintenanceRecipients func(string) (uint64, []string, bool)
	Limits                ScanLimits
	TimeBucket            time.Duration
	cursors               map[string]*scanCursor // owned by the scheduler
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
	hub.SetDeliveryTimeout(inFlightTimeout)
	bucket := time.Millisecond
	if app.A != nil {
		bucket = app.A.Config().Delivery.TimeBucket
	}
	return &Dispatcher{
		db:              db,
		hub:             hub,
		deleter:         deleter,
		logger:          logger.Named("dispatcher"),
		interval:        interval,
		inFlightTimeout: inFlightTimeout,
		wakeCh:          wakeCh,
		Limits:          DefaultScanLimits(),
		TimeBucket:      bucket,
		cursors:         make(map[string]*scanCursor),
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
	d.runPipeline(ctx)
}

// dispatchAll is also used by synchronous correctness tests. Each topic gets
// one bounded chunk; production Run submits those chunks to a fixed worker pool.
func (d *Dispatcher) dispatchAll() int {
	d.hub.ExpireInFlight(time.Now())
	if !d.hub.HasConsumers() || !d.refreshPermit(context.Background()) {
		return 0
	}
	total := 0
	for _, topic := range d.hub.ActiveTopics() {
		total += d.dispatchTopic(topic, time.Now().UnixMilli())
	}
	return total
}

func (d *Dispatcher) dispatchTopic(topic string, nowMs int64) int {
	chunk := d.collectTopic(topic, nowMs)
	if len(chunk.items) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.Limits.PrepareTimeout)
	defer cancel()
	states, err := d.prepareChunk(ctx, chunk)
	if err != nil {
		d.logger.Warn("failed to prepare delivery batch", zap.Error(err))
		return 0
	}
	if !d.refreshPermit(ctx) {
		return 0
	}
	return d.deliverChunk(chunk, states)
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
	if d.inFlight.CompareAndDelete(string(key), entry) {
		d.hub.RemoveDeletedBatch([][]byte{key})
	}
	return false
}

// isExpired returns true if the message's TTL has elapsed.
func (d *Dispatcher) isExpired(msg *storagepb.StoredMessage, nowMs int64) bool {
	if msg.TtlMs <= 0 {
		return false
	}
	return nowMs >= msg.EnqueuedAtUnixMs+msg.TtlMs
}
