package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"go.uber.org/zap"
)

// DeleteBackend abstracts how deletions are persisted.
// In Raft mode, deletions are proposed to the cluster. In standalone mode,
// they are written directly to the local storage engine.
type DeleteBackend interface {
	// DeleteKeys atomically removes the given keys from storage.
	// Returns nil on success, or an error if the deletion could not be
	// completed. On error, the keys are NOT removed and should be retried.
	DeleteKeys(keys [][]byte) error
}

// raftDeleteBackend routes deletions through Raft consensus.
type raftDeleteBackend struct {
	propose func(cmd []byte) error
	logger  *zap.Logger
}

// NewRaftDeleteBackend returns a DeleteBackend that replicates deletions
// via Raft DeleteBatchCmd.
func NewRaftDeleteBackend(propose func(cmd []byte) error, logger *zap.Logger) DeleteBackend {
	return &raftDeleteBackend{
		propose: propose,
		logger:  logger.Named("raft_delete"),
	}
}

func (b *raftDeleteBackend) DeleteKeys(keys [][]byte) error {
	cmd, err := raft.MarshalDeleteBatchCmd(keys)
	if err != nil {
		return err
	}
	return b.propose(cmd)
}

// directDeleteBackend writes deletions directly to the local storage engine.
type directDeleteBackend struct {
	db     storage.DB
	logger *zap.Logger
}

// NewDirectDeleteBackend returns a DeleteBackend that deletes keys directly
// from the local DB (single-node mode).
func NewDirectDeleteBackend(db storage.DB, logger *zap.Logger) DeleteBackend {
	return &directDeleteBackend{
		db:     db,
		logger: logger.Named("direct_delete"),
	}
}

func (b *directDeleteBackend) DeleteKeys(keys [][]byte) error {
	batch := b.db.NewBatch()
	defer batch.Close() //nolint:errcheck

	for _, key := range keys {
		if err := batch.Delete(key); err != nil {
			return err
		}
		if err := batch.Delete(raft.DeliveryStateKey(key)); err != nil {
			return err
		}
	}

	return batch.Commit(storage.Sync)
}

// ─── Deleter ─────────────────────────────────────────────────────────────────

// Deleter batches independent recipient ACKs and explicit/TTL deletions.
// Recipient ACKs retain the shared payload until all required interests finish.
// Failed proposals are retried; a crash before commit may cause redelivery.
type Deleter struct {
	backend  DeleteBackend
	logger   *zap.Logger
	interval time.Duration

	mu      sync.Mutex
	pending [][]byte
	acks    []raft.DeliveryAck
	// AcknowledgeBatch persists independent recipient completions. It deletes
	// a shared payload only after every required recipient has ACKed.
	AcknowledgeBatch func([]raft.DeliveryAck) error

	// OnDelete is called after keys are successfully deleted, with copies of
	// each key. Used to remove entries from the dispatcher's in-flight map.
	OnDelete func(key []byte)
}

// NewDeleter constructs a Deleter with the given backend and flush interval.
func NewDeleter(backend DeleteBackend, interval time.Duration, logger *zap.Logger) *Deleter {
	return &Deleter{
		backend:  backend,
		logger:   logger.Named("deleter"),
		interval: interval,
		pending:  make([][]byte, 0, 1024),
	}
}

// MarkDeleted enqueues a key for batched deletion. The key is the 24-byte
// storage key received as the delivery_tag from the consumer's AckRequest.
func (d *Deleter) MarkDeleted(key []byte) {
	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)

	d.mu.Lock()
	d.pending = append(d.pending, keyCopy)
	d.mu.Unlock()
}

// MarkAcknowledged releases local consumer tracking immediately; persistence
// and retries happen asynchronously, independently of the rebalance drain.
func (d *Deleter) MarkAcknowledged(key []byte, recipient string) {
	if d.AcknowledgeBatch == nil {
		d.MarkDeleted(key)
		return
	}
	d.mu.Lock()
	d.acks = append(d.acks, raft.DeliveryAck{Key: append([]byte(nil), key...), Recipient: recipient})
	d.mu.Unlock()
}

// Run starts the batched delete loop. It blocks until ctx is cancelled.
func (d *Deleter) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.flush()
			return
		case <-ticker.C:
			d.flush()
		}
	}
}

// flush drains the pending queue and deletes the accumulated keys via the
// configured backend. On success, invokes the OnDelete callback for each key.
func (d *Deleter) flush() {
	d.mu.Lock()
	acks := d.acks
	d.acks = nil
	d.mu.Unlock()
	if len(acks) > 0 {
		if err := d.AcknowledgeBatch(acks); err != nil {
			d.logger.Error("failed to persist recipient ACKs", zap.Error(err))
			metrics.DeleteFailuresTotal.Inc()
			d.mu.Lock()
			d.acks = append(acks, d.acks...)
			d.mu.Unlock()
		}
	}
	d.mu.Lock()
	if len(d.pending) == 0 {
		d.mu.Unlock()
		return
	}
	keysToFlush := d.pending
	d.pending = make([][]byte, 0, 1024)
	d.mu.Unlock()

	if err := d.backend.DeleteKeys(keysToFlush); err != nil {
		d.logger.Error("failed to delete batch",
			zap.Error(err),
			zap.Int("count", len(keysToFlush)),
		)
		metrics.DeleteFailuresTotal.Inc()
		// Re-enqueue failed keys for retry on next flush.
		d.mu.Lock()
		d.pending = append(keysToFlush, d.pending...)
		d.mu.Unlock()
		return
	}

	metrics.DeleteBatchSize.Observe(float64(len(keysToFlush)))
	d.logger.Debug("flushed delete batch", zap.Int("count", len(keysToFlush)))

	if d.OnDelete != nil {
		for _, key := range keysToFlush {
			d.OnDelete(key)
		}
	}
}
