package storage

import (
	"errors"
	"io"
)

// ErrNotFound is returned by DB.Get when the requested key does not exist.
// Implementations must map their own not-found sentinel to this error so that
// callers don't need to import engine-specific packages.
var ErrNotFound = errors.New("key not found")

// SyncMode controls fsync behaviour on a Batch commit.
type SyncMode uint8

const (
	// NoSync commits without an fsync. Faster, but data may be lost on crash.
	NoSync SyncMode = iota
	// Sync commits with an fsync. Slower, durable on power loss.
	Sync
)

// IterOptions configures an Iterator's key range.
// Both bounds are optional; a nil bound means the iterator is unbounded in
// that direction.
type IterOptions struct {
	// LowerBound is the inclusive lower bound of the iteration range.
	// The iterator will not visit keys less than this value.
	LowerBound []byte

	// UpperBound is the exclusive upper bound of the iteration range.
	// The iterator will not visit keys greater than or equal to this value.
	UpperBound []byte
}

// Iterator is a forward cursor over a sorted key-value range.
// Callers must call Close when done to release underlying resources.
//
// The byte slices returned by Key and Value are only valid until the next
// call to any iterator method or until Close is called. Copy them if you
// need them to outlive the current iteration step.
type Iterator interface {
	// First positions the iterator at the first key (respecting LowerBound).
	// Returns true if a key is found.
	First() bool

	// Next advances the iterator to the next key.
	// Returns true if a key is found.
	Next() bool

	// Valid reports whether the iterator is positioned at a valid key.
	Valid() bool

	// Key returns the key at the current position.
	// The slice is only valid until the next iterator method call.
	Key() []byte

	// Value returns the value at the current position.
	// The slice is only valid until the next iterator method call.
	Value() []byte

	// Error returns any accumulated error from iteration.
	// Always check this after the loop exits.
	Error() error

	// Close releases the iterator's resources.
	Close() error
}

// Batch is a collection of mutations (Set and Delete) that are applied to the
// database atomically on Commit. Batches are not safe for concurrent use.
//
// Callers must call Close when done, regardless of whether Commit was called,
// to release pooled resources. Closing after a successful Commit is a no-op.
type Batch interface {
	// Set adds a key-value pair to the batch, overwriting any existing value.
	Set(key, value []byte) error

	// Delete removes a key from the batch.
	Delete(key []byte) error

	// Commit applies all mutations in the batch to the database atomically.
	// mode controls whether the commit is durably synced to disk.
	Commit(mode SyncMode) error

	// Close releases the batch's resources. Safe to call after Commit.
	Close() error
}

// DB is the engine-agnostic storage interface.
//
// Implementations are free to be backed by Pebble, BoltDB, or any other
// ordered key-value engine.
//
// Key ordering guarantee: implementations MUST maintain keys in
// lexicographic (byte-wise) order so that the dispatcher's range scan and the
// bucket-based key scheme work correctly.
type DB interface {
	// Get retrieves the value stored for key.
	// Returns ErrNotFound if the key does not exist.
	Get(key []byte) (value []byte, closer io.Closer, err error)

	// NewBatch returns a new empty Batch.
	// The caller must call Batch.Close when done.
	NewBatch() Batch

	// NewIter returns a consistent, point-in-time iterator over the database.
	// opts may be nil for an unbounded scan.
	// The caller must call Iterator.Close when done; this also releases any
	// underlying transaction or snapshot held by the iterator.
	NewIter(opts *IterOptions) (Iterator, error)

	// Flush forces any in-memory data to be written to durable storage.
	Flush() error

	// Close shuts down the storage engine, flushing all pending writes.
	// No other methods may be called after Close returns.
	Close() error
}
