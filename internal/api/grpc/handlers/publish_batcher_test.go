package handlers

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
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

func testPublishBatcher(t *testing.T, cfg config.Publish, clustered bool, flush func(context.Context, pb.AckLevel, []*queuedPublish)) *publishBatcher {
	t.Helper()
	b := newPublishBatcher(cfg, clustered, flush)
	t.Cleanup(func() {
		b.cancel()
		b.workers.Wait()
		b.flushes.Wait()
	})
	return b
}

func handoffPublish(t *testing.T, b *publishBatcher, req *queuedPublish) {
	t.Helper()
	select {
	case b.lanes[0] <- req:
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
		{"bytes", 8, time.Second, []int{50, 55, 39}, [][]int{{50}, {55, 39}}},
		{"timer", 8, 10 * time.Millisecond, []int{11}, [][]int{{11}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultPublish()
			cfg.BatchMaxRequests, cfg.BatchMaxWait, cfg.BatchMaxBytes, cfg.BatchMaxInFlight = tc.count, tc.wait, "100B", 1
			flushed := make(chan []int, len(tc.groups))
			b := testPublishBatcher(t, cfg, true, func(_ context.Context, _ pb.AckLevel, group []*queuedPublish) {
				var sizes []int
				for _, req := range group {
					sizes = append(sizes, req.bytes)
				}
				flushed <- sizes
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

func TestPublishBatcherSingletonsCanOverlapAndRespectLimit(t *testing.T) {
	cfg := config.DefaultPublish()
	cfg.BatchMaxRequests, cfg.BatchMaxBytes, cfg.BatchMaxInFlight = 8, "100B", 2
	started := make(chan int, 3)
	finish := make(chan struct{}, 3)
	b := testPublishBatcher(t, cfg, true, func(ctx context.Context, _ pb.AckLevel, group []*queuedPublish) {
		started <- len(group)
		select {
		case <-finish:
		case <-ctx.Done():
		}
	})
	for range 3 {
		handoffPublish(t, b, &queuedPublish{ctx: context.Background(), bytes: 94, release: func() {}})
	}
	require.Equal(t, 1, awaitPublish(t, started))
	require.Equal(t, 1, awaitPublish(t, started))
	select {
	case <-started:
		t.Fatal("third flush exceeded the in-flight limit")
	case <-time.After(25 * time.Millisecond):
	}
	finish <- struct{}{}
	require.Equal(t, 1, awaitPublish(t, started))
}

func TestPublishBatcherDropsCanceledRequestBeforeSubmission(t *testing.T) {
	cfg := config.DefaultPublish()
	cfg.BatchMaxRequests, cfg.BatchMaxBytes, cfg.BatchMaxInFlight = 8, "100B", 1
	started := make(chan *queuedPublish, 3)
	finish := make(chan struct{}, 3)
	b := testPublishBatcher(t, cfg, true, func(ctx context.Context, _ pb.AckLevel, group []*queuedPublish) {
		started <- group[0]
		select {
		case <-finish:
		case <-ctx.Done():
		}
	})
	first := &queuedPublish{ctx: context.Background(), bytes: 94, release: func() {}}
	handoffPublish(t, b, first)
	require.Same(t, first, awaitPublish(t, started))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	handoffPublish(t, b, &queuedPublish{ctx: ctx, bytes: 94, release: func() { close(released) }})
	cancel()
	finish <- struct{}{}
	awaitPublishSignal(t, released)
	last := &queuedPublish{ctx: context.Background(), bytes: 94, release: func() {}}
	handoffPublish(t, b, last)
	require.Same(t, last, awaitPublish(t, started))
}

func TestRaftGroupedPublishReturnsIndependentResults(t *testing.T) {
	cfg := config.DefaultPublish()
	cfg.BatchMaxRequests, cfg.BatchMaxWait = 3, 500*time.Millisecond
	a, producer, sm, _ := openRaftPublishFixture(t, cfg, true)
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	require.NoError(t, a.Subscriptions.CreateTopic(context.Background(), "empty", nil))
	results := make([]chan error, 3)
	for i, topic := range []string{"orders", "missing", "empty"} {
		results[i] = make(chan error, 1)
		go func() {
			_, err := producer.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}, {Topic: topic}}})
			results[i] <- err
		}()
	}
	for i, code := range []codes.Code{codes.OK, codes.NotFound, codes.FailedPrecondition} {
		require.Equal(t, code, status.Code(awaitPublishError(t, results[i])))
	}
	require.Len(t, publishedKeys(t, a.DB), 2)
	require.Equal(t, int64(1), sm.proposals.Load())
	require.Equal(t, 3, publishFrameCount(t, sm.publishedCommands()[0]))
}

func TestRaftGroupedPublishCancellationAfterSubmissionDoesNotAbortPeers(t *testing.T) {
	cfg := config.DefaultPublish()
	cfg.BatchMaxRequests, cfg.BatchMaxWait, cfg.MaxPendingRequests = 2, 500*time.Millisecond, 2
	a, producer, sm, _ := openRaftPublishFixture(t, cfg, true)
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	release, applying := holdPublish(t, &sm.applyGate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := producer.processBatch(ctx, batch); first <- err }()
	go func() { _, err := producer.processBatch(context.Background(), batch); second <- err }()
	awaitPublishSignal(t, applying)
	cancel()
	require.Equal(t, codes.Canceled, status.Code(awaitPublishError(t, first)))

	// A canceled caller's accepted work still owns its slot until apply.
	early := &pb.PublishBatch{AckLevel: pb.AckLevel_ACK_LEVEL_NO_ACK, Messages: batch.Messages}
	overload, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_, err := producer.processBatch(overload, early)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Zero(t, sm.reads.Load(), "overloaded requests should never reach receipt lookup")
	release()
	require.NoError(t, awaitPublishError(t, second))
	require.Len(t, publishedKeys(t, a.DB), 2)
	require.Equal(t, 2, publishFrameCount(t, sm.publishedCommands()[0]))
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

func batchWithIndex(key string) *pb.PublishBatch {
	index := &pb.Index{}
	if key != "" {
		index.Value = &pb.Index_StringValue{StringValue: key}
	}
	return &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", Indexes: []*pb.Index{index}}}}
}

func BenchmarkRaftPublishBatching(b *testing.B) {
	const clients = 16
	for _, workload := range []struct {
		name         string
		payloadBytes int
		byteCap      config.Size
	}{
		{"groupable", 256, "8KiB"},
		{"forced_singletons", 2500, "4KiB"},
	} {
		for _, requests := range []int{1, 16} {
			b.Run(fmt.Sprintf("%s/max_requests_%d", workload.name, requests), func(b *testing.B) {
				cfg := config.DefaultPublish()
				cfg.BatchMaxRequests, cfg.BatchMaxBytes = requests, workload.byteCap
				_, producer, sm, _ := openRaftPublishFixture(b, cfg, false)
				require.NoError(b, createTestInterest(context.Background(), "orders", "workers"))
				batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", Payload: make([]byte, workload.payloadBytes)}}}
				latencies := make([]time.Duration, b.N)
				failures := make(chan error, clients)
				var workers sync.WaitGroup
				b.ResetTimer()
				for worker := range clients {
					workers.Go(func() {
						for i := worker; i < b.N; i += clients {
							start := time.Now()
							_, err := producer.processBatch(context.Background(), batch)
							latencies[i] = time.Since(start)
							if err != nil {
								failures <- err
								return
							}
						}
					})
				}
				workers.Wait()
				b.StopTimer()
				close(failures)
				for err := range failures {
					require.NoError(b, err)
				}
				proposals := sm.proposals.Load()
				if workload.name == "forced_singletons" || requests == 1 {
					require.Equal(b, int64(b.N), proposals)
				} else if b.N >= 2*clients {
					require.Less(b, proposals, int64(b.N))
				}
				slices.Sort(latencies)
				b.ReportMetric(float64(latencies[(95*b.N+99)/100-1])/float64(time.Millisecond), "p95-ms")
				b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "publishes/s")
				b.ReportMetric(float64(b.N)/float64(proposals), "publishes/proposal")
			})
		}
	}
}
