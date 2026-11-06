package handlers

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func openPublishTest(t *testing.T, engine string) (*app.App, *dispatcher.Hub, *ProducerHandler, func()) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		Storage: config.Storage{Engine: engine,
			Pebble: config.Pebble{Mode: "disk", DataDir: filepath.Join(root, "pebble"), WALEnabled: true},
			Bolt:   config.Bolt{File: filepath.Join(root, "bolt.db")}},
		Delivery: config.DefaultDelivery(), Publish: config.Publish{MinAckLevel: config.Quorum},
	}
	previous := app.A
	a, err := app.Init(cfg, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, a.WithRepositories())
	hub := dispatcher.NewHub(dispatcher.NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	producer := NewProducerHandler(zap.NewNop())
	reopen := func() {
		require.NoError(t, a.DB.Close())
		a, err = app.Init(cfg, zap.NewNop())
		require.NoError(t, err)
		require.NoError(t, a.WithRepositories())
	}
	t.Cleanup(func() { require.NoError(t, a.DB.Close()); app.A = previous })
	return a, hub, producer, reopen
}

func publishedKeys(t *testing.T, db storage.DB) [][]byte {
	t.Helper()
	var keys [][]byte
	require.NoError(t, db.Scan(nil, func(k, _ []byte) error {
		if len(k) == 24 {
			keys = append(keys, append([]byte(nil), k...))
		}
		return nil
	}))
	return keys
}

func TestPublishingRejectsUnregisteredTopicsAtomically(t *testing.T) {
	_, _, producer, _ := openPublishTest(t, "pebble")
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	ack, err := producer.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{
		{Topic: "orders", Payload: []byte("valid")}, {Topic: "unregistered", Payload: []byte("unroutable")},
	}})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.False(t, ack.Success)
	require.Empty(t, publishedKeys(t, app.A.DB), "the whole batch must be rejected")
}

func TestPublishedDestinationsSurviveOfflineWorkersAndRestart(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			_, hub, producer, reopen := openPublishTest(t, engine)
			for _, name := range []string{"a", "b"} {
				require.NoError(t, createTestInterest(context.Background(), "orders", name))
				hub.Register(name, "orders", name, make(chan *pb.QueueMessage, 1))
			}
			originalNames, err := raft.ReadSubscriptionRecipients(app.A.DB, "orders")
			require.NoError(t, err)
			hub.Unregister("b")
			batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", Payload: []byte("payload"), DelayMs: 60_000}}}
			ack, err := producer.processBatch(context.Background(), batch)
			require.NoError(t, err)
			require.True(t, ack.Success)
			keys := publishedKeys(t, app.A.DB)
			require.Len(t, keys, 1)
			state, err := raft.ReadDeliveryState(app.A.DB, keys[0])
			require.NoError(t, err)
			require.True(t, state.PublishBound)
			require.ElementsMatch(t, originalNames, state.Recipients)
			require.NoError(t, createTestInterest(context.Background(), "orders", "later"))
			allNames, err := raft.ReadSubscriptionRecipients(app.A.DB, "orders")
			require.NoError(t, err)
			reopen()
			state, err = raft.ReadDeliveryState(app.A.DB, keys[0])
			require.NoError(t, err)
			require.ElementsMatch(t, originalNames, state.Recipients, "later groups cannot join a frozen roster")
			ack, err = producer.processBatch(context.Background(), batch)
			require.NoError(t, err)
			require.True(t, ack.Success)
			keys = publishedKeys(t, app.A.DB)
			require.Len(t, keys, 2)
			state, err = raft.ReadDeliveryState(app.A.DB, keys[1])
			require.NoError(t, err)
			require.ElementsMatch(t, allNames, state.Recipients)
		})
	}
}

type publishFailureDB struct {
	storage.DB
	commits int
}

func (db *publishFailureDB) NewBatch() storage.Batch {
	return &publishFailureBatch{Batch: db.DB.NewBatch(), db: db}
}

type publishFailureBatch struct {
	storage.Batch
	db *publishFailureDB
}

func (b *publishFailureBatch) Set(k, v []byte) error {
	if strings.HasPrefix(string(k), "metadata/delivery/") {
		return errors.New("receipt write failure")
	}
	return b.Batch.Set(k, v)
}

func (b *publishFailureBatch) Commit(mode storage.SyncMode) error {
	b.db.commits++
	return b.Batch.Commit(mode)
}

func TestPublishReceiptFailureNeverCommitsPayload(t *testing.T) {
	_, _, producer, _ := openPublishTest(t, "pebble")
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	wrapped := &publishFailureDB{DB: app.A.DB}
	app.A.DB = wrapped
	_, err := producer.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}})
	require.ErrorContains(t, err, "receipt write failure")
	require.Zero(t, wrapped.commits)
	require.Empty(t, publishedKeys(t, wrapped))
}

func TestPublishCapturesUniversalSubscribersOnlyAtPublication(t *testing.T) {
	_, _, producer, _ := openPublishTest(t, "pebble")
	original, err := app.A.Subscriptions.Create(context.Background(), "orders", "", "stable")
	require.NoError(t, err)
	_, err = producer.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", DelayMs: time.Hour.Milliseconds()}}})
	require.NoError(t, err)
	_, err = app.A.Subscriptions.Create(context.Background(), "orders", "", "later")
	require.NoError(t, err)
	keys := publishedKeys(t, app.A.DB)
	state, err := raft.ReadDeliveryState(app.A.DB, keys[0])
	require.NoError(t, err)
	require.Equal(t, []string{original.Recipient}, state.Recipients)
}

func createTestInterest(ctx context.Context, topic, group string) error {
	_, err := app.A.Subscriptions.Create(ctx, topic, group, "")
	return err
}

type subscribeRegistrationStream struct{ ackTestStream }

func (s *subscribeRegistrationStream) SendHeader(grpcmetadata.MD) error { return nil }

func (s *subscribeRegistrationStream) Context() context.Context { return context.Background() }

func TestSubscribeRegistersGroupBeyondStreamLifetime(t *testing.T) {
	_, hub, producer, _ := openPublishTest(t, "pebble")
	created, err := app.A.Subscriptions.Create(context.Background(), "orders", "workers", "")
	require.NoError(t, err)
	stream := &subscribeRegistrationStream{ackTestStream{frames: []*pb.ConsumerFrame{{
		Body: &pb.ConsumerFrame_Init{Init: &pb.SubscribeInit{Topic: "orders", GroupId: "workers"}},
	}}}}
	handler := NewConsumerHandler(zap.NewNop(), hub, dispatcher.NewDeleter(nil, time.Hour, zap.NewNop()))
	require.NoError(t, handler.Subscribe(stream))
	require.False(t, hub.HasConsumers())
	names, err := raft.ReadSubscriptionRecipients(app.A.DB, "orders")
	require.NoError(t, err)
	require.Equal(t, []string{created.Recipient}, names)
	ack, err := producer.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}})
	require.NoError(t, err)
	require.True(t, ack.Success, "offline registered groups must accept publications")
}

type publishCommitDB struct {
	storage.DB
	modes []storage.SyncMode
	fail  bool
}

func (db *publishCommitDB) NewBatch() storage.Batch {
	return &publishCommitBatch{Batch: db.DB.NewBatch(), db: db}
}

type publishCommitBatch struct {
	storage.Batch
	db *publishCommitDB
}

func (b *publishCommitBatch) Commit(mode storage.SyncMode) error {
	b.db.modes = append(b.db.modes, mode)
	if b.db.fail {
		return errors.New("commit failure")
	}
	return b.Batch.Commit(mode)
}

func TestStandalonePublishUsesOneSyncCommitAndFailsAtomically(t *testing.T) {
	_, _, producer, _ := openPublishTest(t, "pebble")
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	wrapped := &publishCommitDB{DB: app.A.DB}
	app.A.DB = wrapped
	batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}}
	_, err := producer.processBatch(context.Background(), batch)
	require.NoError(t, err)
	require.Equal(t, []storage.SyncMode{storage.Sync}, wrapped.modes)
	wrapped.fail = true
	_, err = producer.processBatch(context.Background(), batch)
	require.ErrorIs(t, err, errBatchSave)
	require.Len(t, publishedKeys(t, wrapped), 1, "a failed commit cannot publish another payload")
}
