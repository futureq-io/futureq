package repository

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"google.golang.org/protobuf/proto"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
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
	db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
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

// ─── Constructor ────────────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestNewEventRepository_FreshDB_StartsAtZero() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	require.Equal(uint64(0), repo.lastID)
}

func (s *EventRepositorySuite) TestNewEventRepository_RestoresLastID() {
	require := s.Require()

	// Seed last-id directly into the DB.
	b := s.db.NewBatch()
	idBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(idBytes, 42)
	require.NoError(b.Set(eventsLastIDKey, idBytes))
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	repo := s.newRepo(1 * time.Second)
	require.Equal(uint64(42), repo.lastID)
}

// ─── StoreWithBatch ─────────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestStoreWithBatch_AssignsMonotonicIDs() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)
	msg := &storagepb.StoredMessage{
		Topic:            "orders",
		Payload:          []byte("hello"),
		EnqueuedAtUnixMs: 17000,
		DelayMs:          0,
	}

	b := s.db.NewBatch()
	key1, err := repo.StoreWithBatch(b, msg)
	require.NoError(err)
	key2, err := repo.StoreWithBatch(b, msg)
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	// Parse IDs from keys — must be 1 and 2.
	_, _, id1, err := utils.ParseEventKey(key1)
	require.NoError(err)
	_, _, id2, err := utils.ParseEventKey(key2)
	require.NoError(err)

	require.Equal(uint64(1), id1)
	require.Equal(uint64(2), id2)
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
	key, err := repo.StoreWithBatch(b, msg)
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
	key, err := repo.StoreWithBatch(b, msg)
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

func (s *EventRepositorySuite) TestStoreWithBatch_LastIDPersistedInBatch() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	b := s.db.NewBatch()
	_, err := repo.StoreWithBatch(b, &storagepb.StoredMessage{
		Topic:            "t",
		EnqueuedAtUnixMs: 1000,
	})
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	val, closer, err := s.db.Get(eventsLastIDKey)
	require.NoError(err)
	defer closer.Close()

	require.Equal(uint64(1), binary.BigEndian.Uint64(val))
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
	key, err := repo.StoreWithBatch(b, msg)
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
	key, err := repo.StoreRawWithBatch(b, 42, 12345, indexes, rawMsg)
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	// Key layout must match supplied bucket and topicHash.
	th, bucket, id, err := utils.ParseEventKey(key)
	require.NoError(err)
	require.Equal(uint64(12345), th)
	require.Equal(uint64(42), bucket)
	require.Equal(uint64(1), id)

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
	key, err := repo.StoreRawWithBatch(b, 1, 2, indexes, []byte("v"))
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

func (s *EventRepositorySuite) TestStoreRawWithBatch_UpdatesLastID() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	b := s.db.NewBatch()
	_, err := repo.StoreRawWithBatch(b, 1, 1, nil, []byte("x"))
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	val, closer, err := s.db.Get(eventsLastIDKey)
	require.NoError(err)
	defer closer.Close()
	require.Equal(uint64(1), binary.BigEndian.Uint64(val))
}

// ─── DeleteWithBatch ────────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestDeleteWithBatch_RemovesKey() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	// Store first.
	b1 := s.db.NewBatch()
	key, err := repo.StoreWithBatch(b1, &storagepb.StoredMessage{
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

// ─── EventBatch ─────────────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestEventBatch_Store_And_Commit() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	eb := repo.NewBatch()
	key, err := eb.Store(&storagepb.StoredMessage{
		Topic:            "batch-topic",
		Payload:          []byte("batched"),
		EnqueuedAtUnixMs: 9000,
	})
	require.NoError(err)
	require.NoError(eb.Commit(storage.Sync))
	require.NoError(eb.Close())

	val, closer, err := s.db.Get(key)
	require.NoError(err)
	defer closer.Close()

	var decoded storagepb.StoredMessage
	require.NoError(proto.Unmarshal(val, &decoded))
	require.Equal("batch-topic", decoded.Topic)
}

func (s *EventRepositorySuite) TestEventBatch_Store_IsMonotonicWithinBatch() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	eb := repo.NewBatch()
	key1, err := eb.Store(&storagepb.StoredMessage{Topic: "t", EnqueuedAtUnixMs: 1000})
	require.NoError(err)
	key2, err := eb.Store(&storagepb.StoredMessage{Topic: "t", EnqueuedAtUnixMs: 1000})
	require.NoError(err)
	require.NoError(eb.Commit(storage.Sync))
	require.NoError(eb.Close())

	_, _, id1, _ := utils.ParseEventKey(key1)
	_, _, id2, _ := utils.ParseEventKey(key2)
	require.Less(id1, id2)
}

func (s *EventRepositorySuite) TestEventBatch_StoreRaw() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	eb := repo.NewBatch()
	key, err := eb.StoreRaw(7, 99, nil, []byte("raw"))
	require.NoError(err)
	require.NoError(eb.Commit(storage.Sync))
	require.NoError(eb.Close())

	th, bucket, _, err := utils.ParseEventKey(key)
	require.NoError(err)
	require.Equal(uint64(99), th)
	require.Equal(uint64(7), bucket)
}

func (s *EventRepositorySuite) TestEventBatch_Delete() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	// Seed a key directly.
	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("to-delete"), []byte("v")))
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	eb := repo.NewBatch()
	require.NoError(eb.Delete([]byte("to-delete")))
	require.NoError(eb.Commit(storage.Sync))
	require.NoError(eb.Close())

	_, _, err := s.db.Get([]byte("to-delete"))
	require.Error(err)
}

// ─── Concurrency ────────────────────────────────────────────────────────────

func (s *EventRepositorySuite) TestStoreWithBatch_Concurrent_IDsAreUnique() {
	require := s.Require()

	repo := s.newRepo(1 * time.Second)

	const goroutines = 10
	const perGoroutine = 20

	batches := make([]storage.Batch, goroutines)
	for i := range batches {
		batches[i] = s.db.NewBatch()
	}

	var wg sync.WaitGroup
	keys := make([][]byte, goroutines*perGoroutine)
	errs := make([]error, goroutines*perGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				k, err := repo.StoreWithBatch(batches[g], &storagepb.StoredMessage{
					Topic:            "concurrent",
					EnqueuedAtUnixMs: 1000,
				})
				keys[g*perGoroutine+i] = k
				errs[g*perGoroutine+i] = err
			}
		}(g)
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(err)
	}

	// Commit all batches.
	for _, b := range batches {
		require.NoError(b.Commit(storage.Sync))
		require.NoError(b.Close())
	}

	// All event IDs must be unique.
	seen := make(map[uint64]struct{})
	for _, k := range keys {
		_, _, id, err := utils.ParseEventKey(k)
		require.NoError(err)
		_, exists := seen[id]
		require.False(exists, "duplicate event ID %d", id)
		seen[id] = struct{}{}
	}

	require.Len(seen, goroutines*perGoroutine)
}
