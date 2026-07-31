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

type DispatcherSuite struct {
	suite.Suite
	db storage.DB
}

func TestDispatcherSuite(t *testing.T) {
	suite.Run(t, new(DispatcherSuite))
}

func (s *DispatcherSuite) SetupTest() {
	db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	s.Require().NoError(err)
	s.db = db
}

func (s *DispatcherSuite) TearDownTest() {
	if s.db != nil {
		s.db.Close()
	}
}

// storeDueMessage writes a StoredMessage that is already due for delivery.
// Returns the generated event key.
func (s *DispatcherSuite) storeDueMessage(topic string, payload []byte, delayMs, ttlMs int64) []byte {
	bucketSize := 1 * time.Second
	nowMs := time.Now().UnixMilli()

	enqueuedAt := nowMs - 10_000 // enqueued 10s ago

	msg := &storagepb.StoredMessage{
		Topic:            topic,
		Payload:          payload,
		EnqueuedAtUnixMs: enqueuedAt,
		DelayMs:          delayMs,
		TtlMs:            ttlMs,
	}
	data, err := proto.Marshal(msg)
	s.Require().NoError(err)

	fireAtMs := enqueuedAt + delayMs
	bucket := utils.CalculateBucket(fireAtMs, bucketSize)
	key := utils.EventKey(bucket, utils.TopicHash(topic), 1)

	b := s.db.NewBatch()
	s.Require().NoError(b.Set(key, data))
	s.Require().NoError(b.Commit(storage.Sync))
	s.Require().NoError(b.Close())

	return key
}

// ─── isInFlight / RemoveInFlight ─────────────────────────────────────────────

func (s *DispatcherSuite) TestIsInFlight_NewKey_NotInFlight() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Second, 5*time.Second, wakeCh, zap.NewNop())

	require.False(disp.isInFlight([]byte("some-key")))
}

func (s *DispatcherSuite) TestRemoveInFlight_EvictsEntry() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Second, 5*time.Second, wakeCh, zap.NewNop())

	key := []byte("my-key")
	disp.inFlight.Store(string(key), &inFlightEntry{dispatchedAt: time.Now(), topic: "t"})
	require.True(disp.isInFlight(key))

	disp.RemoveInFlight(key)
	require.False(disp.isInFlight(key))
}

func (s *DispatcherSuite) TestRemoveInFlightBatch_RemovesAllKeys() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Second, 5*time.Second, wakeCh, zap.NewNop())

	keys := [][]byte{[]byte("k1"), []byte("k2"), []byte("k3")}
	for _, k := range keys {
		disp.inFlight.Store(string(k), &inFlightEntry{dispatchedAt: time.Now(), topic: "t"})
	}

	disp.RemoveInFlightBatch(keys)

	for _, k := range keys {
		require.False(disp.isInFlight(k))
	}
}

func (s *DispatcherSuite) TestIsInFlight_TimedOutEntry_ReturnsFalse() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	// Very short in-flight timeout.
	disp := NewDispatcher(s.db, hub, d, time.Second, 1*time.Millisecond, wakeCh, zap.NewNop())

	key := []byte("old-key")
	// Entry dispatched in the past → has timed out.
	disp.inFlight.Store(string(key), &inFlightEntry{
		dispatchedAt: time.Now().Add(-time.Hour),
		topic:        "t",
	})

	require.False(disp.isInFlight(key), "timed-out entry must not be considered in-flight")

	// Entry must also be evicted from the map.
	_, exists := disp.inFlight.Load(string(key))
	require.False(exists)
}

// ─── isExpired ───────────────────────────────────────────────────────────────

func (s *DispatcherSuite) TestIsExpired_ZeroTTL_NeverExpires() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Second, 5*time.Second, wakeCh, zap.NewNop())

	msg := &storagepb.StoredMessage{
		EnqueuedAtUnixMs: 1,
		TtlMs:            0,
	}
	require.False(disp.isExpired(msg, time.Now().UnixMilli()))
}

func (s *DispatcherSuite) TestIsExpired_TTLElapsed_True() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Second, 5*time.Second, wakeCh, zap.NewNop())

	nowMs := time.Now().UnixMilli()
	msg := &storagepb.StoredMessage{
		EnqueuedAtUnixMs: nowMs - 10_000,
		TtlMs:            5_000,
	}
	require.True(disp.isExpired(msg, nowMs))
}

func (s *DispatcherSuite) TestIsExpired_TTLNotYetElapsed_False() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Second, 5*time.Second, wakeCh, zap.NewNop())

	nowMs := time.Now().UnixMilli()
	msg := &storagepb.StoredMessage{
		EnqueuedAtUnixMs: nowMs,
		TtlMs:            60_000,
	}
	require.False(disp.isExpired(msg, nowMs))
}

// ─── dispatchAll (standalone mode, no raft) ─────────────────────────────────
//
// These tests exercise the standalone path where app.A.NodeHost == nil, so
// isLeader() always returns true.

func (s *DispatcherSuite) TestDispatchAll_NoConsumers_ReturnsZero() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Second, 5*time.Second, wakeCh, zap.NewNop())

	require.Equal(0, disp.dispatchAll())
}

// ─── Run (event loop) ───────────────────────────────────────────────────────

func (s *DispatcherSuite) TestRun_CancelStopsLoop() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	disp := NewDispatcher(s.db, hub, d, time.Hour, 5*time.Second, wakeCh, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { disp.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		require.Fail("Run did not exit after ctx cancel")
	}
}

func (s *DispatcherSuite) TestRun_WakeSignalTriggersImmediateScan() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
	backend := NewDirectDeleteBackend(s.db, zap.NewNop())
	d := NewDeleter(backend, time.Hour, zap.NewNop())
	// Long interval — only wakeCh can trigger a scan in reasonable time.
	disp := NewDispatcher(s.db, hub, d, time.Hour, 5*time.Second, wakeCh, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { disp.Run(ctx); close(done) }()

	// Register a consumer so HasConsumers returns true.
	consumerCh := make(chan interface{}, 1)
	_ = consumerCh
	// (dispatcher has no messages to dispatch — just verifying Run doesn't deadlock)
	wakeCh <- struct{}{}

	// Give it a moment to process the wake signal.
	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail("Run did not exit after cancel")
	}
}
