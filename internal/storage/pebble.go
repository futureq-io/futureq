package storage

import (
	"io"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/futureq-io/futureq/internal/config"
	"go.uber.org/zap"
)

// Pebble holds an open pebble database and implements storage.DB.
type Pebble struct {
	db     *pebble.DB
	logger *zap.Logger
}

// NewPebble opens a Pebble database using the given config.
func NewPebble(cfg config.Pebble, logger *zap.Logger) (*Pebble, error) {
	pebbleLogger := logger.Named("storage").With(
		zap.String("engine", "pebble"),
	)

	cacheSize := cfg.CacheSizeMB * 1024 * 1024
	if cacheSize <= 0 {
		cacheSize = 64 * 1024 * 1024
	}

	cache := pebble.NewCache(cacheSize)
	// this somehow prevents memory leaks in the opts
	defer cache.Unref()

	eventListener := pebble.MakeLoggingEventListener(pebbleLogger.Sugar())
	dbOpts := &pebble.Options{
		DisableWAL:    cfg.DisableWAL,
		Logger:        pebbleLogger.Sugar(),
		Cache:         cache,
		MemTableSize:  cfg.InMemTableSizeMB * 1024 * 1024,
		EventListener: &eventListener,
	}

	if cfg.DataPath == "" {
		dbOpts.FS = vfs.NewMem()
		pebbleLogger.Info("Initializing Pebble DB in memory", zap.Bool("persist", false))
	} else {
		pebbleLogger.Info("Initializing Pebble DB", zap.Bool("persist", true))
	}

	db, err := pebble.Open(cfg.DataPath, dbOpts)
	if err != nil {
		return nil, err
	}

	return &Pebble{db: db, logger: pebbleLogger}, nil
}

// ── storage.DB implementation ─────────────────────────────────────────────────

func (p *Pebble) Get(key []byte) ([]byte, io.Closer, error) {
	return p.db.Get(key)
}

func (p *Pebble) NewBatch() Batch {
	return &pebbleBatch{b: p.db.NewBatch()}
}

// NewIter opens a snapshot internally and returns an iterator scoped to it.
// Close() on the returned iterator releases both the iterator and the snapshot.
func (p *Pebble) NewIter(opts *IterOptions) (Iterator, error) {
	snap := p.db.NewSnapshot()

	var po *pebble.IterOptions
	if opts != nil {
		po = &pebble.IterOptions{
			LowerBound: opts.LowerBound,
			UpperBound: opts.UpperBound,
		}
	}

	iter, err := snap.NewIter(po)
	if err != nil {
		_ = snap.Close()
		return nil, err
	}

	return &pebbleIterator{iter: iter, snap: snap}, nil
}

func (p *Pebble) Scan(opts *IterOptions, yield func(key, value []byte) bool) error {
	snap := p.db.NewSnapshot()
	defer snap.Close()

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

	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		if !yield(iter.Key(), iter.Value()) {
			break
		}
	}

	return iter.Error()
}

func (p *Pebble) Flush() error { return p.db.Flush() }
func (p *Pebble) Close() error { return p.db.Close() }

// ── pebbleBatch ───────────────────────────────────────────────────────────────

type pebbleBatch struct {
	b *pebble.Batch
}

func (pb *pebbleBatch) Set(key, value []byte) error { return pb.b.Set(key, value, nil) }
func (pb *pebbleBatch) Delete(key []byte) error     { return pb.b.Delete(key, nil) }
func (pb *pebbleBatch) Close() error                { return pb.b.Close() }

func (pb *pebbleBatch) Commit(mode SyncMode) error {
	switch mode {
	case NoSync:
		return pb.b.Commit(pebble.NoSync)
	default:
		return pb.b.Commit(pebble.Sync)
	}
}

// ── pebbleIterator ────────────────────────────────────────────────────────────

// pebbleIterator owns both the pebble.Iterator and the pebble.Snapshot it was
// created from. Close() releases both so callers only manage one resource.
type pebbleIterator struct {
	iter *pebble.Iterator
	snap *pebble.Snapshot
}

func (pi *pebbleIterator) First() bool   { return pi.iter.First() }
func (pi *pebbleIterator) Next() bool    { return pi.iter.Next() }
func (pi *pebbleIterator) Valid() bool   { return pi.iter.Valid() }
func (pi *pebbleIterator) Key() []byte   { return pi.iter.Key() }
func (pi *pebbleIterator) Value() []byte { return pi.iter.Value() }
func (pi *pebbleIterator) Error() error  { return pi.iter.Error() }
func (pi *pebbleIterator) Close() error {
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
