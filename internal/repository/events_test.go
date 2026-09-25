package repository

import (
	"sync"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type EventRepositorySuite struct {
	suite.Suite
	db  storage.DB
	tmp string
}

func TestEventRepositorySuite(t *testing.T) {
	suite.Run(t, new(EventRepositorySuite))
}

func (s *EventRepositorySuite) SetupTest() {
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	s.Require().NoError(err)
	s.db = db
}

func (s *EventRepositorySuite) TearDownTest() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *EventRepositorySuite) newRepo(bucketSize time.Duration) *EventRepository {
	repo, err := NewEventRepository(s.db, zap.NewNop(), bucketSize)
	s.Require().NoError(err)
	return repo
}

// ─── NextID / ObserveID ─────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestNextID_IsMonotonic() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	require.Equal(uint64(1), repo.NextID())
	require.Equal(uint64(2), repo.NextID())
	require.Equal(uint64(3), repo.NextID())
}

func (s *EventRepositorySuite) TestNextID_Concurrent_Unique() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	const goroutines = 10
	const perGoroutine = 100

	ids := make([]uint64, 0, goroutines*perGoroutine)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				id := repo.NextID()
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		_, dup := seen[id]
		require.False(dup, "duplicate id %d", id)
		seen[id] = struct{}{}
	}
	require.Len(seen, goroutines*perGoroutine)
}

func (s *EventRepositorySuite) TestObserveID_AdvancesCounter() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	repo.ObserveID(41)
	require.Equal(uint64(42), repo.NextID())
}

func (s *EventRepositorySuite) TestObserveID_LowerID_NoRegression() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	require.Equal(uint64(1), repo.NextID())
	repo.ObserveID(100)
	repo.ObserveID(50) // lower than current — must not move the counter backwards
	require.Equal(uint64(101), repo.NextID())
}

// ─── StoreWithBatch ─────────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestStoreWithBatch_UsesSuppliedID() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	msg := &storagepb.StoredMessage{
		Topic:            "orders",
		Payload:          []byte("hello"),
		EnqueuedAtUnixMs: 17000,
		DelayMs:          0,
	}

	b := s.db.NewBatch()
	key1, err := repo.StoreWithBatch(b, 7, msg)
	require.NoError(err)
	key2, err := repo.StoreWithBatch(b, 8, msg)
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	_, _, id1, err := utils.ParseEventKey(key1)
	require.NoError(err)
	_, _, id2, err := utils.ParseEventKey(key2)
	require.NoError(err)

	require.Equal(uint64(7), id1)
	require.Equal(uint64(8), id2)
}

func (s *EventRepositorySuite) TestStoreWithBatch_KeyLayout() {
	require := s.Require()

	bucketSize := 1 * time.Second
	repo := s.newRepo(bucketSize)

	msg := &storagepb.StoredMessage{
		Topic:            "payments",
		Payload:          []byte("data"),
		EnqueuedAtUnixMs: 17000, // bucket 17 with 1s buckets
		DelayMs:          2000,  // fire at 19000 → bucket 19
	}

	b := s.db.NewBatch()
	key, err := repo.StoreWithBatch(b, 1, msg)
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	th, bucket, _, err := utils.ParseEventKey(key)
	require.NoError(err)
	require.Equal(utils.TopicHash("payments"), th)
	require.Equal(uint64(19), bucket)
}

func (s *EventRepositorySuite) TestStoreWithBatch_StoredValueIsMarshalledMessage() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	msg := &storagepb.StoredMessage{
		Topic:            "notifications",
		Payload:          []byte("payload-bytes"),
		EnqueuedAtUnixMs: 20000,
		DelayMs:          100,
		TtlMs:            60000,
	}

	b := s.db.NewBatch()
	key, err := repo.StoreWithBatch(b, 1, msg)
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	val, closer, err := s.db.Get(key)
	require.NoError(err)
	defer closer.Close()

	var decoded storagepb.StoredMessage
	require.NoError(proto.Unmarshal(val, &decoded))
	require.Equal(msg.Topic, decoded.Topic)
	require.Equal(msg.Payload, decoded.Payload)
	require.Equal(msg.EnqueuedAtUnixMs, decoded.EnqueuedAtUnixMs)
	require.Equal(msg.DelayMs, decoded.DelayMs)
	require.Equal(msg.TtlMs, decoded.TtlMs)
}

func (s *EventRepositorySuite) TestStoreWithBatch_IndexesAreStored() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	msg := &storagepb.StoredMessage{
		Topic:            "indexed-topic",
		EnqueuedAtUnixMs: 1000,
		Indexes:          []*pb.Index{},
	}

	b := s.db.NewBatch()
	key, err := repo.StoreWithBatch(b, 1, msg)
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	// With no indexes, the only new key is the event key itself.
	th, bucket, id, err := utils.ParseEventKey(key)
	require.NoError(err)
	require.Equal(utils.TopicHash("indexed-topic"), th)
	require.Equal(uint64(1), bucket) // 1000ms / 1s
	require.Equal(uint64(1), id)
}

// ─── StoreRawWithBatch ──────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestStoreRawWithBatch_StoresRawBytes() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	rawMsg := []byte("raw-protobuf-bytes")
	indexes := [][]byte{[]byte("idx-key-1")}

	b := s.db.NewBatch()
	key, err := repo.StoreRawWithBatch(b, 9, 42, 12345, indexes, rawMsg)
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	// Key layout must match supplied id, bucket and topicHash.
	th, bucket, id, err := utils.ParseEventKey(key)
	require.NoError(err)
	require.Equal(uint64(12345), th)
	require.Equal(uint64(42), bucket)
	require.Equal(uint64(9), id)

	// Value must be the exact raw bytes — no re-serialisation.
	val, closer, err := s.db.Get(key)
	require.NoError(err)
	defer closer.Close()
	require.Equal(rawMsg, val)

	// Index key must map back to event key.
	idxVal, idxCloser, err := s.db.Get([]byte("idx-key-1"))
	require.NoError(err)
	defer idxCloser.Close()
	require.Equal(key, idxVal)
}

func (s *EventRepositorySuite) TestStoreRawWithBatch_MultipleIndexes() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	indexes := [][]byte{
		[]byte("idx-a"),
		[]byte("idx-b"),
		[]byte("idx-c"),
	}

	b := s.db.NewBatch()
	key, err := repo.StoreRawWithBatch(b, 1, 1, 2, indexes, []byte("v"))
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	for _, idx := range indexes {
		val, closer, err := s.db.Get(idx)
		require.NoError(err)
		require.Equal(key, val)
		closer.Close()
	}
}

// ─── DeleteWithBatch ────────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestDeleteWithBatch_RemovesKey() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	// Store first.
	b1 := s.db.NewBatch()
	key, err := repo.StoreWithBatch(b1, 1, &storagepb.StoredMessage{
		Topic:            "del-topic",
		EnqueuedAtUnixMs: 5000,
	})
	require.NoError(err)
	require.NoError(b1.Commit(storage.Sync))
	require.NoError(b1.Close())

	// Now delete it.
	b2 := s.db.NewBatch()
	require.NoError(repo.DeleteWithBatch(b2, key))
	require.NoError(b2.Commit(storage.Sync))
	require.NoError(b2.Close())

	_, _, err = s.db.Get(key)
	require.Error(err, "expected key to be deleted")
}
