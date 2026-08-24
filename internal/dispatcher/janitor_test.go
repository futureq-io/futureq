package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type JanitorSuite struct {
	suite.Suite
	db storage.DB
}

func TestJanitorSuite(t *testing.T) {
	suite.Run(t, new(JanitorSuite))
}

func (s *JanitorSuite) SetupTest() {
	db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	s.Require().NoError(err)
	s.db = db
}

func (s *JanitorSuite) TearDownTest() {
	if s.db != nil {
		s.db.Close()
	}
}

// makeEventKey constructs a 24-byte event key.
func makeEventKey(topic string, bucket, eventID uint64) []byte {
	return utils.EventKey(bucket, utils.TopicHash(topic), eventID)
}

// storeMessage writes a marshalled StoredMessage under the given key.
func (s *JanitorSuite) storeMessage(key []byte, msg *storagepb.StoredMessage) {
	data, err := proto.Marshal(msg)
	s.Require().NoError(err)

	b := s.db.NewBatch()
	s.Require().NoError(b.Set(key, data))
	s.Require().NoError(b.Commit(storage.Sync))
	s.Require().NoError(b.Close())
}

// ─── TTLJanitor.sweep ───────────────────────────────────────────────────────

func (s *JanitorSuite) TestSweep_MarksExpiredMessages() {
	require := s.Require()

	nowMs := time.Now().UnixMilli()

	expiredKey := makeEventKey("orders", 1, 1)
	s.storeMessage(expiredKey, &storagepb.StoredMessage{
		Topic:            "orders",
		EnqueuedAtUnixMs: nowMs - 10_000,
		TtlMs:            5_000, // expired 5s ago
	})

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	janitor := NewTTLJanitor(s.db, d, time.Hour, zap.NewNop())

	janitor.sweep()

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Len(d.pending, 1)
	require.Equal(expiredKey, d.pending[0])
}

func (s *JanitorSuite) TestSweep_SkipsNonExpiredMessages() {
	require := s.Require()

	nowMs := time.Now().UnixMilli()

	liveKey := makeEventKey("orders", 1, 1)
	s.storeMessage(liveKey, &storagepb.StoredMessage{
		Topic:            "orders",
		EnqueuedAtUnixMs: nowMs,
		TtlMs:            60_000, // live for another minute
	})

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	janitor := NewTTLJanitor(s.db, d, time.Hour, zap.NewNop())

	janitor.sweep()

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Empty(d.pending)
}

func (s *JanitorSuite) TestSweep_SkipsNoTTL() {
	require := s.Require()

	nowMs := time.Now().UnixMilli()

	noTTLKey := makeEventKey("orders", 1, 1)
	s.storeMessage(noTTLKey, &storagepb.StoredMessage{
		Topic:            "orders",
		EnqueuedAtUnixMs: nowMs - 1_000_000, // very old, but no TTL
		TtlMs:            0,
	})

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	janitor := NewTTLJanitor(s.db, d, time.Hour, zap.NewNop())

	janitor.sweep()

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Empty(d.pending)
}

func (s *JanitorSuite) TestSweep_SkipsNonEventKeys() {
	require := s.Require()

	// A metadata-style key (not 24 bytes) should be silently skipped.
	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("metadata/some-other-key"), []byte("not-proto")))
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	janitor := NewTTLJanitor(s.db, d, time.Hour, zap.NewNop())

	janitor.sweep()

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Empty(d.pending)
}

func (s *JanitorSuite) TestSweep_SkipsUnparseableValue() {
	require := s.Require()

	// Valid 24-byte key but value is not a valid protobuf.
	key := makeEventKey("orders", 1, 1)
	b := s.db.NewBatch()
	require.NoError(b.Set(key, []byte{0xFF, 0xFE, 0xFD}))
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	janitor := NewTTLJanitor(s.db, d, time.Hour, zap.NewNop())

	janitor.sweep()

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Empty(d.pending)
}

func (s *JanitorSuite) TestSweep_MultipleExpired_AllMarked() {
	require := s.Require()

	nowMs := time.Now().UnixMilli()

	keys := [][]byte{
		makeEventKey("t1", 1, 1),
		makeEventKey("t2", 2, 2),
		makeEventKey("t3", 3, 3),
	}
	for _, k := range keys {
		s.storeMessage(k, &storagepb.StoredMessage{
			Topic:            "t",
			EnqueuedAtUnixMs: nowMs - 100_000,
			TtlMs:            1_000,
		})
	}

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	janitor := NewTTLJanitor(s.db, d, time.Hour, zap.NewNop())

	janitor.sweep()

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Len(d.pending, 3)
}

// ─── TTLJanitor.Run ─────────────────────────────────────────────────────────

func (s *JanitorSuite) TestRun_SweepsOnInterval() {
	require := s.Require()

	nowMs := time.Now().UnixMilli()
	expiredKey := makeEventKey("t", 1, 1)
	s.storeMessage(expiredKey, &storagepb.StoredMessage{
		Topic:            "t",
		EnqueuedAtUnixMs: nowMs - 60_000,
		TtlMs:            1_000,
	})

	backend := &mockBackend{}
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	janitor := NewTTLJanitor(s.db, d, 10*time.Millisecond, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { janitor.Run(ctx); close(done) }()

	require.Eventually(func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.pending) > 0
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-done
}
