package storage

import (
	"bytes"
	"fmt"
	"io"

	"github.com/futureq-io/futureq/internal/config"
	"go.etcd.io/bbolt"
	bolt "go.etcd.io/bbolt"
)

// ── bbolt adapter ─────────────────────────────────────────────────────────────
//
// bbolt stores data inside named buckets; we use a single bucket to preserve
// the flat, lexicographically ordered keyspace the dispatcher and key schema
// depend on.
//
// Concurrency model:
//   - Reads: each NewIter call opens a read-only transaction. The transaction
//     is committed (released) when the iterator is closed.
//   - Writes: each Batch.Commit call opens and commits a read-write transaction.

const defaultBucket = "futureq"

// BoltConfig holds the configuration needed to open a bbolt database.
type BoltConfig struct {
	// DataPath is the path to the bbolt database file (e.g. "/data/futureq.db").
	DataPath string
	// Bucket is the name of the bbolt bucket used to store all records.
	// Defaults to "futureq" if empty.
	Bucket string
}

// boltDB wraps *bolt.DB and implements storage.DB.
type boltDB struct {
	db     *bolt.DB
	bucket []byte
}

// NewBoltDB opens a bbolt database at cfg.DataPath and returns it as a
// storage.DB. The database file is created if it does not exist.
func NewBoltDB(cfg config.Bolt) (DB, error) {
	db, err := bolt.Open(cfg.DataPath, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("bbolt: failed to open %q: %w", cfg.DataPath, err)
	}

	bname := cfg.DefaultBucket
	if bname == "" {
		bname = defaultBucket
	}

	// Ensure the bucket exists before any caller tries to use it.
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bname))
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("bbolt: failed to create bucket %q: %w", bname, err)
	}

	return &boltDB{db: db, bucket: []byte(bname)}, nil
}

// Get retrieves the value for key using a short-lived read-only transaction.
func (b *boltDB) Get(key []byte) ([]byte, io.Closer, error) {
	var out []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(b.bucket)
		if bkt == nil {
			return fmt.Errorf("bbolt: bucket %q not found", b.bucket)
		}
		v := bkt.Get(key)
		if v == nil {
			return ErrNotFound
		}
		// Values are only valid for the duration of the transaction, so copy.
		out = make([]byte, len(v))
		copy(out, v)
		return nil
	})
	return out, io.NopCloser(nil), err
}

// NewBatch returns a boltBatch that accumulates Set/Delete operations and
// applies them in a single read-write transaction on Commit.
func (b *boltDB) NewBatch() Batch {
	return &boltBatch{db: b.db, bucket: b.bucket}
}

// NewIter opens a read-only transaction and returns an iterator over the bucket.
// The transaction is released when the iterator is closed.
func (b *boltDB) NewIter(opts *IterOptions) (Iterator, error) {
	tx, err := b.db.Begin(false)
	if err != nil {
		return nil, fmt.Errorf("bbolt: failed to begin read tx: %w", err)
	}

	bkt := tx.Bucket(b.bucket)
	if bkt == nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("bbolt: bucket %q not found", b.bucket)
	}

	var lower, upper []byte
	if opts != nil {
		lower = opts.LowerBound
		upper = opts.UpperBound
	}

	return &boltIterator{
		tx:     tx,
		cursor: bkt.Cursor(),
		lower:  lower,
		upper:  upper,
	}, nil
}

// Scan iterates over the bbolt database and yields keys and values to the provided function.
func (b *boltDB) Scan(opts *IterOptions, yield func(key, value []byte) error) error {
	// Open a read-only transaction.
	// This provides the same consistency guarantees as Pebble's Snapshot.
	return b.db.View(func(tx *bbolt.Tx) error {
		// bbolt stores data in buckets. Grab the default bucket for your KV store.
		bucket := tx.Bucket(b.bucket)
		if bucket == nil {
			// If the bucket doesn't exist, the database is effectively empty.
			return nil
		}

		c := bucket.Cursor()
		var k, v []byte

		// 1. Handle LowerBound
		if opts != nil && opts.LowerBound != nil {
			// Seek moves the cursor to the first key that is >= LowerBound
			k, v = c.Seek(opts.LowerBound)
		} else {
			// No LowerBound? Start at the very beginning of the database
			k, v = c.First()
		}

		// 2. The hot loop
		for k != nil {
			// Handle UpperBound (LevelDB/Pebble standard is that UpperBound is exclusive)
			// bytes.Compare returns >= 0 if k is equal to or greater than UpperBound
			if opts != nil && opts.UpperBound != nil && bytes.Compare(k, opts.UpperBound) >= 0 {
				break
			}

			// Yield to the caller. If they return false, break the loop early.
			if err := yield(k, v); err != nil {
				return fmt.Errorf("yield func: %w", err)
			}

			// Move to the next key in the B+Tree
			k, v = c.Next()
		}

		return nil
	})
}

// Flush is a no-op for bbolt: every committed transaction is fsync'd by default.
func (b *boltDB) Flush() error { return nil }

// Close closes the underlying bbolt database.
func (b *boltDB) Close() error { return b.db.Close() }

// ── boltBatch ────────────────────────────────────────────────────────────────

type boltBatch struct {
	db     *bolt.DB
	bucket []byte
	ops    []boltOp
}

type boltOp struct {
	del   bool
	key   []byte
	value []byte
}

func (bb *boltBatch) Set(key, value []byte) error {
	k := make([]byte, len(key))
	copy(k, key)
	v := make([]byte, len(value))
	copy(v, value)
	bb.ops = append(bb.ops, boltOp{key: k, value: v})
	return nil
}

func (bb *boltBatch) Delete(key []byte) error {
	k := make([]byte, len(key))
	copy(k, key)
	bb.ops = append(bb.ops, boltOp{del: true, key: k})
	return nil
}

// Commit applies all accumulated operations in a single read-write transaction.
// SyncMode is accepted for interface compatibility; bbolt always syncs on
// commit unless bolt.DB.NoSync is set globally.
func (bb *boltBatch) Commit(_ SyncMode) error {
	return bb.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(bb.bucket)
		if bkt == nil {
			return fmt.Errorf("bbolt: bucket %q not found", bb.bucket)
		}
		for _, op := range bb.ops {
			var err error
			if op.del {
				err = bkt.Delete(op.key)
			} else {
				err = bkt.Put(op.key, op.value)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Close discards the pending operations. Safe to call after Commit.
func (bb *boltBatch) Close() error {
	bb.ops = bb.ops[:0]
	return nil
}

// ── boltIterator ─────────────────────────────────────────────────────────────

// boltIterator adapts a *bolt.Cursor to the storage.Iterator interface.
// It owns the read-only transaction it was created from; Close() rolls it back.
type boltIterator struct {
	tx     *bolt.Tx
	cursor *bolt.Cursor
	lower  []byte
	upper  []byte

	curKey []byte
	curVal []byte
	done   bool
}

func (bi *boltIterator) First() bool {
	var k, v []byte
	if bi.lower != nil {
		k, v = bi.cursor.Seek(bi.lower)
	} else {
		k, v = bi.cursor.First()
	}
	return bi.set(k, v)
}

func (bi *boltIterator) Next() bool {
	if bi.done {
		return false
	}
	k, v := bi.cursor.Next()
	return bi.set(k, v)
}

func (bi *boltIterator) set(k, v []byte) bool {
	if k == nil || (bi.upper != nil && bytes.Compare(k, bi.upper) >= 0) {
		bi.done = true
		bi.curKey = nil
		bi.curVal = nil
		return false
	}
	bi.curKey = k
	bi.curVal = v
	return true
}

func (bi *boltIterator) Valid() bool   { return !bi.done && bi.curKey != nil }
func (bi *boltIterator) Key() []byte   { return bi.curKey }
func (bi *boltIterator) Value() []byte { return bi.curVal }

// Error always returns nil for bbolt — range exhaustion surfaces as nil keys
// from Next()/First(), not as deferred error values.
func (bi *boltIterator) Error() error { return nil }

// Close rolls back the read-only transaction, releasing it back to bbolt.
func (bi *boltIterator) Close() error { return bi.tx.Rollback() }

// ── compile-time interface checks ─────────────────────────────────────────────

var (
	_ DB       = (*boltDB)(nil)
	_ Batch    = (*boltBatch)(nil)
	_ Iterator = (*boltIterator)(nil)
)
