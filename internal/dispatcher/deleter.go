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
			b.logger.Error("failed to mark key for deletion", zap.Error(err))
		}
	}

	return batch.Commit(storage.Sync)
}

// ─── Deleter ─────────────────────────────────────────────────────────────────

// Deleter accumulates acknowledged-message keys and periodically flushes them
// as a single batch through the configured DeleteBackend.
//
// Routing deletions through Raft ensures that all replicas remove acknowledged
// messages atomically, preventing a new leader from re-dispatching a message
// that was already acknowledged before a failover.
type Deleter struct {
	backend  DeleteBackend
	logger   *zap.Logger
	interval time.Duration

	mu      sync.Mutex
	pending [][]byte

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
