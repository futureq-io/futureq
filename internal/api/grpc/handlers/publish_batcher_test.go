package handlers

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func awaitPublish[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("publish did not advance")
		var zero T
		return zero
	}
}

func awaitPublishSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	awaitPublish(t, ch)
}

func awaitPublishError(t *testing.T, ch <-chan error) error {
	t.Helper()
	return awaitPublish(t, ch)
}

func assertPublishWaiting(t *testing.T, returned <-chan error) {
	t.Helper()
	select {
	case err := <-returned:
		t.Fatalf("publish returned before the requested ACK guarantee: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
}

func testPublishBatcher(t *testing.T, cfg config.Publish, write func(context.Context, []*queuedPublish) error) *publishBatcher {
	t.Helper()
	b := newPublishBatcher(cfg, write)
	t.Cleanup(b.close)
	return b
}

func handoffPublish(t *testing.T, b *publishBatcher, req *queuedPublish) {
	t.Helper()
	req.done = make(chan error, 1)
	select {
	case b.queueIn <- req:
	case <-time.After(3 * time.Second):
		t.Fatal("publish worker did not accept request")
	}
}

func TestPublishBatcherFlushThresholds(t *testing.T) {
	tests := []struct {
		name   string
		count  int
		wait   time.Duration
		sizes  []int
		groups [][]int
	}{
		{"count", 2, time.Second, []int{11, 12}, [][]int{{11, 12}}},
		{"bytes", 8, time.Second, []int{50, 55, 45}, [][]int{{50}, {55, 45}}},
		{"timer", 8, 10 * time.Millisecond, []int{11}, [][]int{{11}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultPublish()
			cfg.BatchMaxRequests, cfg.BatchMaxWait, cfg.BatchMaxBytes = tc.count, tc.wait, "100B"
			flushed := make(chan []int, len(tc.groups))
			b := testPublishBatcher(t, cfg, func(_ context.Context, group []*queuedPublish) error {
				var sizes []int
				for _, req := range group {
					sizes = append(sizes, req.bytes)
				}
				flushed <- sizes
				return nil
			})
			for _, size := range tc.sizes {
				handoffPublish(t, b, &queuedPublish{ctx: context.Background(), bytes: size, release: func() {}})
			}
			for _, want := range tc.groups {
				require.Equal(t, want, awaitPublish(t, flushed))
			}
		})
	}
}

func TestPublishBatcherDropsCanceledRequestBeforeWrite(t *testing.T) {
	cfg := config.DefaultPublish()
	cfg.BatchMaxRequests, cfg.BatchMaxWait = 2, 100*time.Millisecond
	written := make(chan *queuedPublish, 2)
	b := testPublishBatcher(t, cfg, func(_ context.Context, group []*queuedPublish) error {
		for _, req := range group {
			written <- req
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	handoffPublish(t, b, &queuedPublish{ctx: ctx, bytes: 1, release: func() { close(released) }})
	cancel()
	survivor := &queuedPublish{ctx: context.Background(), bytes: 1}
	handoffPublish(t, b, survivor)
	awaitPublishSignal(t, released)
	require.Same(t, survivor, awaitPublish(t, written))
}

func TestStandaloneBatchingSharesCommitAndBoltValidation(t *testing.T) {
	for _, engine := range []string{"bolt", "pebble"} {
		t.Run(engine, func(t *testing.T) {
			a, _, direct, _ := openPublishTest(t, engine)
			require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
			a.Config().Publish.BatchMaxRequests, a.Config().Publish.BatchMaxWait = 2, 500*time.Millisecond
			batched := NewProducerHandler(zap.NewNop())
			t.Cleanup(batched.Close)

			if engine == "bolt" {
				for _, producer := range []*ProducerHandler{direct, batched} {
					for _, key := range []string{"", string(make([]byte, bolt.MaxKeySize+1))} {
						_, err := producer.processBatch(context.Background(), batchWithIndex(key))
						require.Equal(t, codes.InvalidArgument, status.Code(err))
					}
				}
			}
			db := &publishCommitDB{DB: a.DB}
			a.DB = db
			results := make(chan error, 2)
			for range 2 {
				go func() {
					_, err := batched.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}})
					results <- err
				}()
			}
			require.NoError(t, awaitPublishError(t, results))
			require.NoError(t, awaitPublishError(t, results))
			require.Equal(t, []storage.SyncMode{storage.Sync}, db.modes)
			require.Len(t, publishedKeys(t, db), 2)
		})
	}
}

type blockedPublishCommitDB struct {
	storage.DB
	calls   atomic.Int64
	entered chan struct{}
	finish  chan struct{}
}

func (db *blockedPublishCommitDB) NewBatch() storage.Batch {
	return &blockedPublishCommitBatch{Batch: db.DB.NewBatch(), db: db}
}

type blockedPublishCommitBatch struct {
	storage.Batch
	db *blockedPublishCommitDB
}

func (b *blockedPublishCommitBatch) Commit(mode storage.SyncMode) error {
	if b.db.calls.Add(1) == 1 {
		close(b.db.entered)
		<-b.db.finish
	}
	return b.Batch.Commit(mode)
}

func TestStandaloneConcurrentCommitsPreserveWatermark(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			a, _, producer, reopen := openPublishTest(t, engine)
			require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
			db := &blockedPublishCommitDB{DB: a.DB, entered: make(chan struct{}), finish: make(chan struct{})}
			a.DB = db
			batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}}
			first, second := make(chan error, 1), make(chan error, 1)
			var publishers sync.WaitGroup
			unblock := sync.OnceFunc(func() { close(db.finish) })
			defer func() { unblock(); publishers.Wait() }()

			publishers.Go(func() {
				_, err := producer.processBatch(context.Background(), batch)
				first <- err
			})
			awaitPublishSignal(t, db.entered)
			publishers.Go(func() {
				_, err := producer.processBatch(context.Background(), batch)
				second <- err
			})
			require.NoError(t, awaitPublishError(t, second), "a blocked publisher must not hold up another commit")
			unblock()
			require.NoError(t, awaitPublishError(t, first))

			watermark, closer, err := db.Get(repository.LastIDKey)
			require.NoError(t, err)
			require.Len(t, watermark, 8)
			require.EqualValues(t, 2, binary.BigEndian.Uint64(watermark), "late commit must not replace the higher ID")
			require.NoError(t, closer.Close())
			reopen()
			_, err = producer.processBatch(context.Background(), batch)
			require.NoError(t, err)
			var ids []uint64
			for _, key := range publishedKeys(t, app.A.DB) {
				_, _, id, err := utils.ParseEventKey(key)
				require.NoError(t, err)
				ids = append(ids, id)
			}
			require.ElementsMatch(t, []uint64{1, 2, 3}, ids)
		})
	}
}

func TestStandaloneFlushTargetDoesNotLimitIndividualPublish(t *testing.T) {
	a, _, _, _ := openPublishTest(t, "pebble")
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	cfg := &a.Config().Publish
	cfg.BatchMaxRequests, cfg.BatchMaxBytes, cfg.MaxRequestBytes = 2, "64B", "480B"
	producer := NewProducerHandler(zap.NewNop())
	t.Cleanup(producer.Close)

	batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", Payload: make([]byte, 256)}}}
	ack, err := producer.processBatch(context.Background(), batch)
	require.NoError(t, err)
	require.True(t, ack.Success)
	batch.Messages[0].Payload = make([]byte, 512)
	_, err = producer.processBatch(context.Background(), batch)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Len(t, publishedKeys(t, a.DB), 1)
}

func batchWithIndex(key string) *pb.PublishBatch {
	index := &pb.Index{}
	if key != "" {
		index.Value = &pb.Index_StringValue{StringValue: key}
	}
	return &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", Indexes: []*pb.Index{index}}}}
}
