package dispatcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

type DeleterSuite struct {
	suite.Suite
}

func TestDeleterSuite(t *testing.T) {
	suite.Run(t, new(DeleterSuite))
}

// ─── DirectDeleteBackend ─────────────────────────────────────────────────────

func (s *DeleterSuite) TestDirectDeleteBackend_RemovesKeys() {
	require := s.Require()

	db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	require.NoError(err)
	defer db.Close()

	// Seed a key.
	b := db.NewBatch()
	require.NoError(b.Set([]byte("to-delete"), []byte("v")))
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	before, closer, err := db.Get([]byte("to-delete"))
	require.NoError(err)
	require.Equal([]byte("v"), before)
	closer.Close()

	backend := NewDirectDeleteBackend(db, zap.NewNop())
	require.NoError(backend.DeleteKeys([][]byte{[]byte("to-delete")}))

	_, _, err = db.Get([]byte("to-delete"))
	require.Error(err, "expected key to be deleted")
}

func (s *DeleterSuite) TestDirectDeleteBackend_MultipleKeys() {
	require := s.Require()

	db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	require.NoError(err)
	defer db.Close()

	b := db.NewBatch()
	for _, k := range []string{"k1", "k2", "k3"} {
		require.NoError(b.Set([]byte(k), []byte("v")))
	}
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	backend := NewDirectDeleteBackend(db, zap.NewNop())
	require.NoError(backend.DeleteKeys([][]byte{
		[]byte("k1"), []byte("k2"), []byte("k3"),
	}))

	for _, k := range []string{"k1", "k2", "k3"} {
		_, _, err := db.Get([]byte(k))
		require.Error(err)
	}
}

func (s *DeleterSuite) TestDirectDeleteBackend_EmptyList_Succeeds() {
	require := s.Require()

	db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	require.NoError(err)
	defer db.Close()

	backend := NewDirectDeleteBackend(db, zap.NewNop())
	require.NoError(backend.DeleteKeys([][]byte{}))
}

// ─── RaftDeleteBackend ──────────────────────────────────────────────────────

func (s *DeleterSuite) TestRaftDeleteBackend_ProposalsMarshalledCommand() {
	require := s.Require()

	var captured []byte
	proposeFn := func(cmd []byte) error {
		captured = append([]byte(nil), cmd...)
		return nil
	}

	backend := NewRaftDeleteBackend(proposeFn, zap.NewNop())

	key := make([]byte, 24) // required key length for MarshalDeleteBatchCmd
	require.NoError(backend.DeleteKeys([][]byte{key}))

	require.NotNil(captured, "propose must be called")
	require.Equal(byte(1), captured[0], "first byte should be DeleteBatchCmd type (1)")
}

func (s *DeleterSuite) TestRaftDeleteBackend_ProposalsErrorPropagates() {
	require := s.Require()

	sentinel := errors.New("propose failed")
	proposeFn := func(cmd []byte) error { return sentinel }

	backend := NewRaftDeleteBackend(proposeFn, zap.NewNop())
	key := make([]byte, 24)

	err := backend.DeleteKeys([][]byte{key})
	require.Error(err)
	require.Contains(err.Error(), sentinel.Error())
}

func (s *DeleterSuite) TestRaftDeleteBackend_InvalidKeyLength_Fails() {
	require := s.Require()

	proposeFn := func(cmd []byte) error { return nil }
	backend := NewRaftDeleteBackend(proposeFn, zap.NewNop())

	err := backend.DeleteKeys([][]byte{[]byte("short-key")})
	require.Error(err, "a non-24-byte key must be rejected")
}

// ─── Deleter ─────────────────────────────────────────────────────────────────

// mockBackend records every DeleteKeys call and can be set to fail.
type mockBackend struct {
	calls atomic.Int32
	err   error
	keys  [][]byte
}

func (m *mockBackend) DeleteKeys(keys [][]byte) error {
	m.calls.Add(1)
	m.keys = keys
	return m.err
}

func (s *DeleterSuite) TestDeleter_MarkDeleted_AccumulatesKeys() {
	require := s.Require()

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())

	d.MarkDeleted([]byte("a"))
	d.MarkDeleted([]byte("b"))

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Len(d.pending, 2)
}

func (s *DeleterSuite) TestDeleter_Run_FlushesOnInterval() {
	require := s.Require()

	backend := &mockBackend{}
	d := NewDeleter(backend, 10*time.Millisecond, zap.NewNop())

	d.MarkDeleted([]byte("x"))
	d.MarkDeleted([]byte("y"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	// Wait for at least one flush.
	require.Eventually(func() bool {
		return backend.calls.Load() >= 1
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-done
}

func (s *DeleterSuite) TestDeleter_RetriesFailedFlush() {
	require := s.Require()

	backend := &mockBackend{err: errors.New("boom")}
	d := NewDeleter(backend, time.Hour, zap.NewNop()) // long interval — only ctx cancel triggers flush

	d.MarkDeleted([]byte("retry-me"))

	// Trigger a flush manually via Run + Cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	cancel()
	<-done

	// After a failed flush the key must be re-enqueued for retry.
	d.mu.Lock()
	defer d.mu.Unlock()
	require.Len(d.pending, 1, "failed keys must be re-enqueued")
	require.Equal([]byte("retry-me"), d.pending[0])
}

func (s *DeleterSuite) TestDeleter_OnDelete_CalledForEachKey() {
	require := s.Require()

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())

	var deleted []string
	d.OnDelete = func(key []byte) { deleted = append(deleted, string(key)) }

	d.MarkDeleted([]byte("k1"))
	d.MarkDeleted([]byte("k2"))

	// Trigger flush via ctx cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	cancel()
	<-done

	require.Len(deleted, 2)
	require.Contains(deleted, "k1")
	require.Contains(deleted, "k2")
}

func (s *DeleterSuite) TestDeleter_MarkDeleted_CopiesKey() {
	require := s.Require()

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())

	original := []byte("original-key")
	d.MarkDeleted(original)

	// Mutating the original slice must not affect the pending copy.
	original[0] = 'X'

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Equal([]byte("original-key"), d.pending[0])
}
