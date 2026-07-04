package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/raft"
	"github.com/futureq-io/futureq/internal/storage"
	"go.uber.org/zap"
)

// Deleter accumulates acknowledged-message keys and periodically flushes them
// as a single Raft-replicated DeleteBatchCmd. In single-node (non-Raft) mode
// it falls back to writing deletions directly to Pebble.
//
// Routing deletions through Raft ensures that all replicas remove acknowledged
// messages atomically, preventing a new leader from re-dispatching a message
// that was already acknowledged before a failover.
type Deleter struct {
	db       storage.DB
	logger   *zap.Logger
	interval time.Duration
	pending  [][]byte
	mu       sync.Mutex

	// propose is called to submit a DeleteBatchCmd to the Raft cluster.
	// If nil, deletions are written directly to Pebble (single-node mode).
	propose func(cmd []byte) error

	// OnDelete is called after keys are successfully deleted, with copies of
	// each key. Used to remove entries from the dispatcher's in-flight map.
	OnDelete func(key []byte)
}

// NewDeleter constructs a Deleter.
// propose should be set to a function that calls NodeHost.SyncPropose with a
// DeleteBatchCmd payload. Pass nil for single-node (non-Raft) mode.
func NewDeleter(db storage.DB, interval time.Duration, propose func(cmd []byte) error, logger *zap.Logger) *Deleter {
	return &Deleter{
		db:       db,
		logger:   logger.Named("deleter"),
		interval: interval,
		pending:  make([][]byte, 0, 1024),
		propose:  propose,
	}
}

// MarkDeleted enqueues a key for batched deletion. The key is the 24-byte
// Pebble key received as the delivery_tag from the consumer's AckRequest.
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

// flush drains the pending queue and either proposes a Raft DeleteBatchCmd
// or writes directly to Pebble (single-node fallback).
func (d *Deleter) flush() {
	d.mu.Lock()
	if len(d.pending) == 0 {
		d.mu.Unlock()
		return
	}
	keysToFlush := d.pending
	d.pending = make([][]byte, 0, 1024)
	d.mu.Unlock()

	if d.propose != nil {
		// Raft path: replicate the deletion to all nodes atomically.
		cmd, err := raft.MarshalDeleteBatchCmd(keysToFlush)
		if err != nil {
			d.logger.Error("failed to marshal DeleteBatchCmd", zap.Error(err))
			return
		}

		if err := d.propose(cmd); err != nil {
			d.logger.Error("failed to propose DeleteBatchCmd via Raft", zap.Error(err),
				zap.Int("count", len(keysToFlush)))
			return
		}

		d.logger.Debug("flushed delete batch via Raft", zap.Int("count", len(keysToFlush)))
	} else {
		// Single-node path: write deletions directly to Pebble.
		batch := d.db.NewBatch()
		defer batch.Close()

		for _, key := range keysToFlush {
			if err := batch.Delete(key); err != nil {
				d.logger.Error("failed to mark key for deletion", zap.Error(err))
			}
		}

		if err := batch.Commit(storage.Sync); err != nil {
			d.logger.Error("failed to commit delete batch", zap.Error(err))
			return
		}

		d.logger.Debug("flushed delete batch directly", zap.Int("count", len(keysToFlush)))
	}

	if d.OnDelete != nil {
		for _, key := range keysToFlush {
			d.OnDelete(key)
		}
	}
}
