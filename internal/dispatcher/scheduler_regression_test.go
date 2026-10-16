package dispatcher

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type schedulerScanDB struct {
	storage.DB
	calls  atomic.Int64
	before func(*storage.IterOptions)
}

func (db *schedulerScanDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	db.calls.Add(1)
	if db.before != nil {
		db.before(opts)
	}
	return db.DB.NewIter(opts)
}

func TestSchedulerBusyTopicCannotExhaustDiscoveryAdmission(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, workerCount := range []int{2, 4} {
			t.Run(fmt.Sprintf("%s/workers-%d", engine, workerCount), func(t *testing.T) {
				d, ledger := newPreloadTest(t, engine)
				d.PreloadLookahead, d.PreloadMaxMessages = 0, 0
				d.Limits.Candidates = 1
				d.Limits.Workers = workerCount
				d.Limits.PrepareTimeout = time.Second
				slow, fast := make(chan *pb.QueueMessage, 8), make(chan *pb.QueueMessage, 1)
				d.hub.Register("slow", "a-slow", "workers", slow)
				d.hub.Register("fast", "b-fast", "workers", fast)
				var slowKeys [][]byte
				for id := uint64(1); id <= 4; id++ {
					slowKeys = append(slowKeys, putPipelineEvent(t, d, "a-slow", id, time.Now().Add(-time.Minute).UnixMilli()))
				}
				// Seed two independently admitted scan results within the two-entry due
				// allowance. Assigning one must not leave credits trapped by its busy topic.
				view := d.hub.InterestView("a-slow")
				p := &topicPreload{epoch: view.Epoch}
				d.preloads = map[string]*topicPreload{"a-slow": p}
				for range 2 {
					d.admitChunk(p, d.collectTopic("a-slow", time.Now().UnixMilli()), nil)
				}
				require.Equal(t, 2, d.readyCount)
				fastScan := make(chan struct{})
				var scanOnce sync.Once
				d.db = &schedulerScanDB{DB: d.db, before: func(opts *storage.IterOptions) {
					if opts != nil && len(opts.LowerBound) >= 8 && bytes.Equal(opts.LowerBound[:8], utils.TopicLowerBound(utils.TopicHash("b-fast"))) {
						scanOnce.Do(func() { close(fastScan) })
					}
				}}
				entered, release := make(chan struct{}), make(chan struct{})
				var first atomic.Bool
				d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
					if bytes.Equal(items[0].Key[:8], utils.TopicLowerBound(utils.TopicHash("a-slow"))) && first.CompareAndSwap(false, true) {
						close(entered)
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-release:
						}
					}
					return ledger.PrepareBatch(ctx, items)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { d.Run(ctx); close(done) }()
				var releaseOnce sync.Once
				t.Cleanup(func() { cancel(); releaseOnce.Do(func() { close(release) }); <-done })
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("slow preparation did not start")
				}
				// On the regression the ready allowance fills; on the fix another topic
				// can scan. Gate the late insert on either observable outcome.
				require.Eventually(t, func() bool {
					select {
					case <-fastScan:
						return true
					default:
					}
					var m dto.Metric
					require.NoError(t, metrics.DeliveryQueueDepth.WithLabelValues("ready").Write(&m))
					return m.GetGauge().GetValue() == 2
				}, 500*time.Millisecond, time.Millisecond)
				key := putPipelineEvent(t, d, "b-fast", 99, time.Now().Add(-time.Minute).UnixMilli())
				select {
				case d.wakeCh <- struct{}{}:
				default:
				}
				select {
				case msg := <-fast:
					require.Equal(t, key, msg.DeliveryTag)
				case <-time.After(250 * time.Millisecond):
					t.Fatal("busy topic exhausted admission while another preparation worker was idle")
				}
				releaseOnce.Do(func() { close(release) })
				seen := make(map[string]bool)
				for range slowKeys {
					select {
					case msg := <-slow:
						require.False(t, seen[string(msg.DeliveryTag)])
						seen[string(msg.DeliveryTag)] = true
					case <-time.After(time.Second):
						t.Fatal("evicted ready candidates were not rediscovered after preparation resumed")
					}
				}
				for _, key := range slowKeys {
					require.True(t, seen[string(key)])
				}
			})
		}
	}
}

func TestSchedulerUnfitBatchBacksOffDiscovery(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, ledger := newPreloadTest(t, engine)
			d.PreloadLookahead, d.PreloadMaxMessages = 0, 0
			d.interval = time.Second
			d.Limits.Bytes = 1024
			queues := make([]chan *pb.QueueMessage, 30)
			for i := range queues {
				queues[i] = make(chan *pb.QueueMessage, 1)
				d.hub.Register(fmt.Sprintf("%02d%s", i, strings.Repeat("u", 80)), "orders", "", queues[i])
			}
			key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Minute).UnixMilli())
			counted := &schedulerScanDB{DB: d.db}
			d.db = counted
			var logs bytes.Buffer
			core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.Lock(zapcore.AddSync(&logs)), zap.ErrorLevel)
			d.logger = zap.New(core)
			var preparations atomic.Int64
			d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
				preparations.Add(1)
				return ledger.PrepareBatch(ctx, items)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			d.Run(ctx)
			t.Logf("iterator calls in 150ms: %d", counted.calls.Load())
			require.LessOrEqual(t, counted.calls.Load(), int64(4), "an unfit recipient set must wait for backoff instead of immediate rediscovery")
			require.Zero(t, preparations.Load())
			require.Zero(t, d.readyCount)
			require.Contains(t, logs.String(), "delivery interests exceed preparation byte limit")
			for _, queue := range queues {
				require.Empty(t, queue)
			}
			// Rejection releases cache references, not durable data.
			value, closer, err := d.db.Get(key)
			require.NoError(t, err)
			require.NotEmpty(t, value)
			require.NoError(t, closer.Close())
		})
	}
}

func TestSchedulerLateScanCannotRetainBusyTopicAdmission(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, ledger := newPreloadTest(t, engine)
			d.Limits.Candidates = 1
			d.Limits.PrepareTimeout = time.Second
			slow, fast := make(chan *pb.QueueMessage, 8), make(chan *pb.QueueMessage, 1)
			d.hub.Register("slow", "a-slow", "workers", slow)
			d.hub.Register("fast", "b-fast", "workers", fast)
			due := time.Now().Add(100 * time.Millisecond).UnixMilli()
			keys := [][]byte{putPipelineEvent(t, d, "a-slow", 1, due)}
			require.Zero(t, d.dispatchAll())
			require.Equal(t, 1, d.preloadCount)
			for id := uint64(2); id <= 4; id++ {
				keys = append(keys, putPipelineEvent(t, d, "a-slow", id, time.Now().Add(-time.Minute).UnixMilli()))
			}
			scanEntered, releaseScan := make(chan struct{}), make(chan struct{})
			var scanFirst atomic.Bool
			ctx, cancel := context.WithCancel(context.Background())
			d.db = &schedulerScanDB{DB: d.db, before: func(opts *storage.IterOptions) {
				if opts != nil && len(opts.LowerBound) >= 8 && bytes.Equal(opts.LowerBound[:8], utils.TopicLowerBound(utils.TopicHash("a-slow"))) && scanFirst.CompareAndSwap(false, true) {
					close(scanEntered)
					select {
					case <-releaseScan:
					case <-ctx.Done():
					}
				}
			}}
			prepareEntered, releasePrepare := make(chan struct{}), make(chan struct{})
			var prepareFirst atomic.Bool
			d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
				if bytes.Equal(items[0].Key[:8], utils.TopicLowerBound(utils.TopicHash("a-slow"))) && prepareFirst.CompareAndSwap(false, true) {
					close(prepareEntered)
					select {
					case <-releasePrepare:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return ledger.PrepareBatch(ctx, items)
			}
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			var stopScan, stopPrepare sync.Once
			t.Cleanup(func() {
				cancel()
				stopScan.Do(func() { close(releaseScan) })
				stopPrepare.Do(func() { close(releasePrepare) })
				<-done
			})
			select {
			case <-scanEntered:
			case <-time.After(time.Second):
				t.Fatal("scan did not start before cached deadline")
			}
			select {
			case <-prepareEntered:
			case <-time.After(time.Second):
				t.Fatal("blocked scan prevented cached preparation")
			}
			stopScan.Do(func() { close(releaseScan) })
			// Before release, the blocked scan always occupies one discovery slot.
			// Zero confirms its result has been consumed, even on the broken version.
			require.Eventually(t, func() bool {
				var m dto.Metric
				require.NoError(t, metrics.DeliveryQueueDepth.WithLabelValues("discovery").Write(&m))
				return m.GetGauge().GetValue() == 0
			}, 500*time.Millisecond, time.Millisecond)
			fastKey := putPipelineEvent(t, d, "b-fast", 99, time.Now().Add(-time.Minute).UnixMilli())
			select {
			case d.wakeCh <- struct{}{}:
			default:
			}
			select {
			case msg := <-fast:
				require.Equal(t, fastKey, msg.DeliveryTag)
			case <-time.After(250 * time.Millisecond):
				t.Fatal("late scan result retained global due credits for a busy topic")
			}
			stopPrepare.Do(func() { close(releasePrepare) })
			seen := make(map[string]bool)
			for range keys {
				select {
				case msg := <-slow:
					require.False(t, seen[string(msg.DeliveryTag)])
					seen[string(msg.DeliveryTag)] = true
				case <-time.After(time.Second):
					t.Fatal("late-scan candidates did not recover after busy job completed")
				}
			}
			for _, key := range keys {
				require.True(t, seen[string(key)])
			}
		})
	}
}

func TestReleaseReadyPreservesFuturePayloadsAndRewindsRepair(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.Limits.Candidates = 3
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 8))
	now := time.Now().UnixMilli()
	for id := uint64(1); id <= 3; id++ {
		putPipelineEvent(t, d, "orders", id, now-1000)
	}
	future := putPipelineEvent(t, d, "orders", 4, now+5000)
	p := &topicPreload{epoch: d.hub.InterestView("orders").Epoch}
	scanned := d.collectTopic("orders", now)
	require.Len(t, scanned.items, 3)
	d.admitChunk(p, scanned, nil)
	d.preloadTopic("orders", p, d.hub.InterestView("orders"), now)
	require.Equal(t, 3, p.readyCount)
	require.Equal(t, 1, d.preloadCount)
	backing := p.items[:cap(p.items)]
	d.releaseReady("orders", p)
	require.Zero(t, d.readyCount)
	require.Zero(t, p.readyCount)
	require.Len(t, p.items, 1)
	require.Len(t, p.keys, 1)
	require.Equal(t, [24]byte(future), p.items[0].key)
	require.Equal(t, []byte("payload"), p.items[0].payload.message.Payload)
	for _, slot := range backing {
		require.False(t, slot.ready, "removed due references must be cleared from the old array")
		if slot.payload.message != nil {
			require.Equal(t, [24]byte(future), slot.key, "the old backing array must not retain evicted payloads")
		}
	}
	require.Equal(t, uint64(1), p.repairGeneration)
	require.Nil(t, d.cursors["orders"])
	repair := d.collectTopic("orders", now)
	require.Len(t, repair.items, 3, "eviction must preserve discovery of every unsent durable message")
	require.Equal(t, 3, d.admitChunk(p, repair, nil))
	require.Equal(t, 3, p.readyCount)
	require.Equal(t, 3, d.readyCount)
}

func TestFutureOnlyDiscoveryDoesNotScanOrAdvanceDueCursor(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 4))
	now := time.Now().UnixMilli()
	due := putPipelineEvent(t, d, "orders", 1, now-1000)
	future := putPipelineEvent(t, d, "orders", 2, now+5000)
	counted := &schedulerScanDB{DB: d.db}
	d.db = counted
	cursor := scanCursor{epoch: d.hub.InterestView("orders").Epoch, after: append(append([]byte(nil), due...), 0)}
	result := d.discover(discoveryJob{topic: "orders", owner: &topicPreload{epoch: cursor.epoch}, cursor: cursor, dueSlots: 0, futureSlots: 2})
	require.EqualValues(t, 1, counted.calls.Load(), "a busy topic may only open its future iterator")
	require.Empty(t, result.chunk.items)
	require.Equal(t, cursor, result.cursor)
	require.Len(t, result.future, 1)
	require.Equal(t, [24]byte(future), result.future[0].key)
	require.Equal(t, []byte("payload"), result.future[0].payload.message.Payload)
}

func TestSchedulerFailedTopicsReleasePrefetchDuringBackoff(t *testing.T) {
	d, ledger := newPipelineTest(t)
	d.Limits.Candidates = 1
	d.Limits.PrepareTimeout = time.Second
	d.interval = time.Second
	fast := make(chan *pb.QueueMessage, 1)
	for _, topic := range []string{"a-fail", "b-fail"} {
		d.hub.Register(topic, topic, "workers", make(chan *pb.QueueMessage, 4))
		for id := uint64(1); id <= 3; id++ {
			putPipelineEvent(t, d, topic, id, time.Now().Add(-time.Minute).UnixMilli())
		}
	}
	d.hub.Register("fast", "c-fast", "workers", fast)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
		if bytes.Equal(items[0].Key[:8], utils.TopicLowerBound(utils.TopicHash("c-fast"))) {
			return ledger.PrepareBatch(ctx, items)
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil, fmt.Errorf("forced preparation failure")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	var once sync.Once
	t.Cleanup(func() { cancel(); once.Do(func() { close(release) }); <-done })
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("both preparation workers did not start")
		}
	}
	require.Eventually(t, func() bool {
		var m dto.Metric
		require.NoError(t, metrics.DeliveryQueueDepth.WithLabelValues("ready").Write(&m))
		return m.GetGauge().GetValue() == 2
	}, 500*time.Millisecond, time.Millisecond)
	key := putPipelineEvent(t, d, "c-fast", 99, time.Now().Add(-time.Minute).UnixMilli())
	once.Do(func() { close(release) })
	select {
	case msg := <-fast:
		require.Equal(t, key, msg.DeliveryTag)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("failed topics retained prefetch credits during their one-second backoff")
	}
}
