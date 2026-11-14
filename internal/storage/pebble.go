package storage

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/futureq-io/futureq/internal/config"
	"go.uber.org/zap"
)

// Pebble holds an open pebble database and implements storage.DB.
type Pebble struct {
	revision atomic.Uint64
	empty    pebbleEmptyRanges
	db       *pebble.DB
	logger   *zap.Logger
}

// NewPebble opens a Pebble database using the given config.
func NewPebble(cfg config.Pebble, logger *zap.Logger) (*Pebble, error) {
	concurrency := cfg.CompactionConcurrency
	if concurrency < 0 {
		return nil, fmt.Errorf("pebble compaction concurrency must be positive")
	}
	if concurrency == 0 {
		concurrency = 1 // Preserve direct callers that omit this configuration field.
	}
	pebbleLogger := logger.Named("storage").With(
		zap.String("engine", "pebble"),
		zap.Int("compaction_concurrency", concurrency),
	)

	cacheSize, err := cfg.CacheSize.Bytes()
	if err != nil {
		cacheSize = 64 * 1024 * 1024
	}
	memtableSize, err := cfg.MemtableSize.Bytes()
	if err != nil {
		memtableSize = 64 * 1024 * 1024
	}

	cache := pebble.NewCache(cacheSize)
	// this somehow prevents memory leaks in the opts
	defer cache.Unref()

	eventListener := pebble.MakeLoggingEventListener(pebbleLogger.Sugar())
	dbOpts := &pebble.Options{
		DisableWAL:                 cfg.Mode != "memory" && !cfg.WALEnabled,
		Logger:                     pebbleLogger.Sugar(),
		Cache:                      cache,
		MemTableSize:               uint64(memtableSize),
		EventListener:              &eventListener,
		CompactionConcurrencyRange: func() (int, int) { return concurrency, concurrency },
	}

	path := cfg.DataDir
	if cfg.Mode == "memory" || path == "" {
		path = ""
		dbOpts.FS = vfs.NewMem()
		pebbleLogger.Info("Initializing Pebble DB in memory", zap.Bool("persist", false))
	} else {
		pebbleLogger.Info("Initializing Pebble DB", zap.Bool("persist", true))
	}

	db, err := pebble.Open(path, dbOpts)
	if err != nil {
		return nil, err
	}

	return &Pebble{db: db, logger: pebbleLogger}, nil
}

// ── storage.DB implementation ─────────────────────────────────────────────────

func (p *Pebble) Get(key []byte) ([]byte, io.Closer, error) {
	val, closer, err := p.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return val, closer, ErrNotFound
	}

	return val, closer, err
}

func (p *Pebble) NewBatch() Batch {
	return &pebbleBatch{b: p.db.NewBatch(), revision: &p.revision, empty: &p.empty}
}

// NewIter opens a snapshot internally and returns an iterator scoped to it.
// Close() on the returned iterator releases both the iterator and the snapshot.
func (p *Pebble) NewIter(opts *IterOptions) (Iterator, error) {
	// Serialize snapshot capture with gap invalidation, never with commit I/O.
	p.empty.mu.Lock()
	snap := p.db.NewSnapshot()
	probe, lower := p.empty.capture(opts)
	p.empty.mu.Unlock()

	var po *pebble.IterOptions
	if opts != nil {
		po = &pebble.IterOptions{
			LowerBound: opts.LowerBound,
			UpperBound: opts.UpperBound,
		}
		if lower != nil {
			po.LowerBound = lower
		}
	}

	iter, err := snap.NewIter(po)
	if err != nil {
		p.empty.finish(probe, nil)
		_ = snap.Close()
		return nil, err
	}

	return &pebbleIterator{iter: iter, snap: snap, empty: &p.empty, probe: probe}, nil
}

func (p *Pebble) Scan(opts *IterOptions, yield func(key, value []byte) error) error {
	snap := p.db.NewSnapshot()

	defer snap.Close() //nolint:errcheck

	var po *pebble.IterOptions
	if opts != nil {
		po = &pebble.IterOptions{
			LowerBound: opts.LowerBound,
			UpperBound: opts.UpperBound,
		}
	}

	iter, err := snap.NewIter(po)
	if err != nil {
		return err
	}

	defer iter.Close() //nolint:errcheck

	for iter.First(); iter.Valid(); iter.Next() {
		if err := yield(iter.Key(), iter.Value()); err != nil {
			return fmt.Errorf("yield func: %w", err)
		}
	}

	return iter.Error()
}

func (p *Pebble) Flush() error { return p.db.Flush() }
func (p *Pebble) Close() error { return p.db.Close() }

// ── pebbleBatch ───────────────────────────────────────────────────────────────

type pebbleBatch struct {
	empty      *pebbleEmptyRanges
	sets       pebbleKeySpan
	revision   *atomic.Uint64
	payloadSet bool
	b          *pebble.Batch
}

func (pb *pebbleBatch) Set(key, value []byte) error {
	pb.sets.include(key)
	err := pb.b.Set(key, value, nil)
	if err == nil && len(key) == 24 {
		pb.payloadSet = true
	}
	return err
}
func (pb *pebbleBatch) Delete(key []byte) error {
	return pb.b.Delete(key, nil)
}
func (pb *pebbleBatch) Close() error { return pb.b.Close() }

func (pb *pebbleBatch) Commit(mode SyncMode) error {
	if pb.empty != nil {
		done := pb.empty.begin(&pb.sets)
		defer done()
	}
	opts := pebble.Sync
	if mode == NoSync {
		opts = pebble.NoSync
	}
	if err := pb.b.Commit(opts); err != nil {
		return err
	}
	if pb.payloadSet {
		pb.revision.Add(1)
	}
	return nil
}

// ── pebbleIterator ────────────────────────────────────────────────────────────

// pebbleIterator owns both the pebble.Iterator and the pebble.Snapshot it was
// created from. Close() releases both so callers only manage one resource.
type pebbleIterator struct {
	empty *pebbleEmptyRanges
	probe *pebbleEmptyProbe
	iter  *pebble.Iterator
	snap  *pebble.Snapshot
}

func (pi *pebbleIterator) First() bool {
	valid := pi.iter.First()
	if pi.probe != nil {
		var end []byte
		if valid {
			end = pi.iter.Key()
		} else if pi.iter.Error() == nil {
			end = pi.probe.upper
		}
		pi.empty.finish(pi.probe, end)
		pi.probe = nil
	}
	return valid
}
func (pi *pebbleIterator) Last() bool    { return pi.iter.Last() }
func (pi *pebbleIterator) Next() bool    { return pi.iter.Next() }
func (pi *pebbleIterator) Valid() bool   { return pi.iter.Valid() }
func (pi *pebbleIterator) Key() []byte   { return pi.iter.Key() }
func (pi *pebbleIterator) Value() []byte { return pi.iter.Value() }
func (pi *pebbleIterator) Error() error  { return pi.iter.Error() }
func (pi *pebbleIterator) Close() error {
	pi.empty.finish(pi.probe, nil)
	pi.probe = nil
	iterErr := pi.iter.Close()
	snapErr := pi.snap.Close()
	if iterErr != nil {
		return iterErr
	}

	return snapErr
}

// ── compile-time interface checks ─────────────────────────────────────────────

var (
	_ DB       = (*Pebble)(nil)
	_ Batch    = (*pebbleBatch)(nil)
	_ Iterator = (*pebbleIterator)(nil)
)

func (p *Pebble) CacheRevision() uint64 { return p.revision.Load() }

var _ CacheRevisioner = (*Pebble)(nil)
