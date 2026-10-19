package dispatcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"
)

type SchedulerSuite struct {
	suite.Suite
}

func TestSchedulerSuite(t *testing.T) {
	suite.Run(t, new(SchedulerSuite))
}

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

func (s *SchedulerSuite) TestSchedulerBusyTopicCannotExhaustDiscoveryAdmission() {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, workerCount := range []int{2, 4} {
			s.Run(fmt.Sprintf("%s/workers-%d", engine, workerCount), func() {
				require := s.Require()

				d, ledger := newPreloadTest(s.T(), engine)
				d.PreloadLookahead, d.PreloadMaxMessages = 0, 0
				d.Limits.Candidates = 1
				d.Limits.Workers = workerCount
				d.Limits.PrepareTimeout = time.Second
				slow, fast := make(chan *pb.QueueMessage, 8), make(chan *pb.QueueMessage, 1)
				d.hub.Register("slow", "a-slow", "workers", slow)
				d.hub.Register("fast", "b-fast", "workers", fast)
				var slowKeys [][]byte
				for id := uint64(1); id <= 4; id++ {
					slowKeys = append(slowKeys, putPipelineEvent(s.T(), d, "a-slow", id, time.Now().Add(-time.Minute).UnixMilli()))
				}
				// Seed two independently admitted scan results within the two-entry due
				// allowance. Assigning one must not leave credits trapped by its busy topic.
				view := d.hub.InterestView("a-slow")
				p := &topicPreload{epoch: view.Epoch}
				d.preloads = map[string]*topicPreload{"a-slow": p}
				for range 2 {
					d.admitChunk(p, d.collectTopic("a-slow", time.Now().UnixMilli()), nil)
				}
				require.Equal(2, d.readyCount)
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
				s.T().Cleanup(func() { cancel(); releaseOnce.Do(func() { close(release) }); <-done })
				select {
				case <-entered:
				case <-time.After(time.Second):
					s.T().Fatal("slow preparation did not start")
				}
				// On the regression the ready allowance fills; on the fix another topic
				// can scan. Gate the late insert on either observable outcome.
				require.Eventually(func() bool {
					select {
					case <-fastScan:
						return true
					default:
					}
					var m dto.Metric
					require.NoError(metrics.DeliveryQueueDepth.WithLabelValues("ready").Write(&m))
					return m.GetGauge().GetValue() == 2
				}, 500*time.Millisecond, time.Millisecond)
				key := putPipelineEvent(s.T(), d, "b-fast", 99, time.Now().Add(-time.Minute).UnixMilli())
				select {
				case d.wakeCh <- struct{}{}:
				default:
				}
				select {
				case msg := <-fast:
					require.Equal(key, msg.DeliveryTag)
				case <-time.After(250 * time.Millisecond):
					s.T().Fatal("busy topic exhausted admission while another preparation worker was idle")
				}
				releaseOnce.Do(func() { close(release) })
				seen := make(map[string]bool)
				for range slowKeys {
					select {
					case msg := <-slow:
						require.False(seen[string(msg.DeliveryTag)])
						seen[string(msg.DeliveryTag)] = true
					case <-time.After(time.Second):
						s.T().Fatal("evicted ready candidates were not rediscovered after preparation resumed")
					}
				}
				for _, key := range slowKeys {
					require.True(seen[string(key)])
				}
			})
		}
	}
}

func (s *SchedulerSuite) TestSchedulerUnfitBatchBacksOffDiscovery() {
	for _, engine := range []string{"pebble", "bolt"} {
		s.Run(engine, func() {
			require := s.Require()

			d, ledger := newPreloadTest(s.T(), engine)
			d.PreloadLookahead, d.PreloadMaxMessages = 0, 0
			d.interval = time.Second
			d.Limits.Bytes = 1024
			queues := make([]chan *pb.QueueMessage, 30)
			for i := range queues {
				queues[i] = make(chan *pb.QueueMessage, 1)
				d.hub.Register(fmt.Sprintf("%02d%s", i, strings.Repeat("u", 80)), "orders", "", queues[i])
			}
			key := putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(-time.Minute).UnixMilli())
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
			s.T().Logf("iterator calls in 150ms: %d", counted.calls.Load())
			require.LessOrEqual(counted.calls.Load(), int64(4), "an unfit recipient set must wait for backoff instead of immediate rediscovery")
			require.Zero(preparations.Load())
			require.Zero(d.readyCount)
			require.Contains(logs.String(), "delivery interests exceed preparation byte limit")
			for _, queue := range queues {
				require.Empty(queue)
			}
			// Rejection releases cache references, not durable data.
			value, closer, err := d.db.Get(key)
			require.NoError(err)
			require.NotEmpty(value)
			require.NoError(closer.Close())
		})
	}
}

func (s *SchedulerSuite) TestSchedulerLateScanCannotRetainBusyTopicAdmission() {
	for _, engine := range []string{"pebble", "bolt"} {
		s.Run(engine, func() {
			require := s.Require()

			d, ledger := newPreloadTest(s.T(), engine)
			d.Limits.Candidates = 1
			d.Limits.PrepareTimeout = time.Second
			slow, fast := make(chan *pb.QueueMessage, 8), make(chan *pb.QueueMessage, 1)
			d.hub.Register("slow", "a-slow", "workers", slow)
			d.hub.Register("fast", "b-fast", "workers", fast)
			due := time.Now().Add(100 * time.Millisecond).UnixMilli()
			keys := [][]byte{putPipelineEvent(s.T(), d, "a-slow", 1, due)}
			require.Zero(d.dispatchAll())
			require.Equal(1, d.preloadCount)
			for id := uint64(2); id <= 4; id++ {
				keys = append(keys, putPipelineEvent(s.T(), d, "a-slow", id, time.Now().Add(-time.Minute).UnixMilli()))
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
			s.T().Cleanup(func() {
				cancel()
				stopScan.Do(func() { close(releaseScan) })
				stopPrepare.Do(func() { close(releasePrepare) })
				<-done
			})
			select {
			case <-scanEntered:
			case <-time.After(time.Second):
				s.T().Fatal("scan did not start before cached deadline")
			}
			select {
			case <-prepareEntered:
			case <-time.After(time.Second):
				s.T().Fatal("blocked scan prevented cached preparation")
			}
			stopScan.Do(func() { close(releaseScan) })
			// Before release, the blocked scan always occupies one discovery slot.
			// Zero confirms its result has been consumed, even on the broken version.
			require.Eventually(func() bool {
				var m dto.Metric
				require.NoError(metrics.DeliveryQueueDepth.WithLabelValues("discovery").Write(&m))
				return m.GetGauge().GetValue() == 0
			}, 500*time.Millisecond, time.Millisecond)
			fastKey := putPipelineEvent(s.T(), d, "b-fast", 99, time.Now().Add(-time.Minute).UnixMilli())
			select {
			case d.wakeCh <- struct{}{}:
			default:
			}
			select {
			case msg := <-fast:
				require.Equal(fastKey, msg.DeliveryTag)
			case <-time.After(250 * time.Millisecond):
				s.T().Fatal("late scan result retained global due credits for a busy topic")
			}
			stopPrepare.Do(func() { close(releasePrepare) })
			seen := make(map[string]bool)
			for range keys {
				select {
				case msg := <-slow:
					require.False(seen[string(msg.DeliveryTag)])
					seen[string(msg.DeliveryTag)] = true
				case <-time.After(time.Second):
					s.T().Fatal("late-scan candidates did not recover after busy job completed")
				}
			}
			for _, key := range keys {
				require.True(seen[string(key)])
			}
		})
	}
}

func (s *SchedulerSuite) TestReleaseReadyPreservesFuturePayloadsAndRewindsRepair() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	d.Limits.Candidates = 3
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 8))
	now := time.Now().UnixMilli()
	for id := uint64(1); id <= 3; id++ {
		putPipelineEvent(s.T(), d, "orders", id, now-1000)
	}
	future := putPipelineEvent(s.T(), d, "orders", 4, now+5000)
	p := &topicPreload{epoch: d.hub.InterestView("orders").Epoch}
	scanned := d.collectTopic("orders", now)
	require.Len(scanned.items, 3)
	d.admitChunk(p, scanned, nil)
	d.preloadTopic("orders", p, d.hub.InterestView("orders"), now)
	require.Equal(3, p.readyCount)
	require.Equal(1, d.preloadCount)
	backing := p.items[:cap(p.items)]
	d.releaseReady("orders", p)
	require.Zero(d.readyCount)
	require.Zero(p.readyCount)
	require.Len(p.items, 1)
	require.Len(p.keys, 1)
	require.Equal([24]byte(future), p.items[0].key)
	require.Equal([]byte("payload"), p.items[0].payload.message.Payload)
	for _, slot := range backing {
		require.False(slot.ready, "removed due references must be cleared from the old array")
		if slot.payload.message != nil {
			require.Equal([24]byte(future), slot.key, "the old backing array must not retain evicted payloads")
		}
	}
	require.Equal(uint64(1), p.repairGeneration)
	require.Nil(d.cursors["orders"])
	repair := d.collectTopic("orders", now)
	require.Len(repair.items, 3, "eviction must preserve discovery of every unsent durable message")
	require.Equal(3, d.admitChunk(p, repair, nil))
	require.Equal(3, p.readyCount)
	require.Equal(3, d.readyCount)
}

func (s *SchedulerSuite) TestFutureOnlyDiscoveryDoesNotScanOrAdvanceDueCursor() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 4))
	now := time.Now().UnixMilli()
	due := putPipelineEvent(s.T(), d, "orders", 1, now-1000)
	future := putPipelineEvent(s.T(), d, "orders", 2, now+5000)
	counted := &schedulerScanDB{DB: d.db}
	d.db = counted
	cursor := scanCursor{epoch: d.hub.InterestView("orders").Epoch, after: append(append([]byte(nil), due...), 0)}
	result := d.discover(discoveryJob{topic: "orders", owner: &topicPreload{epoch: cursor.epoch}, cursor: cursor, dueSlots: 0, futureSlots: 2})
	require.EqualValues(1, counted.calls.Load(), "a busy topic may only open its future iterator")
	require.Empty(result.chunk.items)
	require.Equal(cursor, result.cursor)
	require.Len(result.future, 1)
	require.Equal([24]byte(future), result.future[0].key)
	require.Equal([]byte("payload"), result.future[0].payload.message.Payload)
}

func (s *SchedulerSuite) TestSchedulerFailedTopicsReleasePrefetchDuringBackoff() {
	require := s.Require()

	d, ledger := newPipelineTest(s.T())
	d.Limits.Candidates = 1
	d.Limits.PrepareTimeout = time.Second
	d.interval = time.Second
	fast := make(chan *pb.QueueMessage, 1)
	for _, topic := range []string{"a-fail", "b-fail"} {
		d.hub.Register(topic, topic, "workers", make(chan *pb.QueueMessage, 4))
		for id := uint64(1); id <= 3; id++ {
			putPipelineEvent(s.T(), d, topic, id, time.Now().Add(-time.Minute).UnixMilli())
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
	s.T().Cleanup(func() { cancel(); once.Do(func() { close(release) }); <-done })
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			s.T().Fatal("both preparation workers did not start")
		}
	}
	require.Eventually(func() bool {
		var m dto.Metric
		require.NoError(metrics.DeliveryQueueDepth.WithLabelValues("ready").Write(&m))
		return m.GetGauge().GetValue() == 2
	}, 500*time.Millisecond, time.Millisecond)
	key := putPipelineEvent(s.T(), d, "c-fast", 99, time.Now().Add(-time.Minute).UnixMilli())
	once.Do(func() { close(release) })
	select {
	case msg := <-fast:
		require.Equal(key, msg.DeliveryTag)
	case <-time.After(250 * time.Millisecond):
		s.T().Fatal("failed topics retained prefetch credits during their one-second backoff")
	}
}

type workloadDB struct {
	storage.DB
	payloadGets, metadataGets, scans atomic.Int64
}

func (db *workloadDB) Get(key []byte) ([]byte, io.Closer, error) {
	if len(key) == 24 {
		db.payloadGets.Add(1)
	} else {
		db.metadataGets.Add(1)
	}
	return db.DB.Get(key)
}
func (db *workloadDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	db.scans.Add(1)
	return db.DB.NewIter(opts)
}
func workloadInt(name string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return n
}
func workloadCPU() float64 {
	var u syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	return float64(u.Utime.Sec+u.Stime.Sec) + float64(u.Utime.Usec+u.Stime.Usec)/1e6
}

func (s *SchedulerSuite) TestSchedulerWorkload() {
	require := s.Require()

	if os.Getenv("FUTUREQ_WORKLOAD") == "" {
		s.T().Skip("opt-in local end-to-end workload")
	}
	engine := os.Getenv("FUTUREQ_WORKLOAD_ENGINE")
	if engine == "" {
		engine = "pebble-memory"
	}
	count := workloadInt("FUTUREQ_WORKLOAD_COUNT", 1200)
	size := workloadInt("FUTUREQ_WORKLOAD_SIZE", 4096)
	delay := workloadInt("FUTUREQ_WORKLOAD_DELAY_MS", 50)
	rate := workloadInt("FUTUREQ_WORKLOAD_RATE", 5000)
	topicCount := workloadInt("FUTUREQ_WORKLOAD_TOPICS", 1)
	var raw storage.DB
	var err error
	if engine == "bolt" {
		raw, err = storage.NewBoltDB(config.Bolt{File: filepath.Join(s.T().TempDir(), "data.db")})
	} else {
		cfg := config.Pebble{Mode: "memory"}
		if engine == "pebble-disk" {
			cfg.Mode = "disk"
			cfg.DataDir = s.T().TempDir()
			cfg.WALEnabled = true
		}
		raw, err = storage.NewPebble(cfg, zap.NewNop())
	}
	require.NoError(err)
	defer raw.Close()
	db := &workloadDB{DB: raw}
	wake := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wake)
	ledger := NewDeliveryLedger(db, hub.LocalDeliveryRecipients, NewDirectDeleteBackend(db, zap.NewNop()), nil)
	deleter := NewDeleter(ledger, time.Duration(workloadInt("FUTUREQ_WORKLOAD_ACK_MS", 1))*time.Millisecond, zap.NewNop())
	deleter.AcknowledgeBatch = ledger.Acknowledge
	deleter.OnAcknowledged = hub.RemoveAcknowledgedBatch
	ledger.OnDelete = hub.RemoveDeletedBatch
	d := NewDispatcher(db, hub, deleter, 5*time.Millisecond, 5*time.Second, wake, zap.NewNop())
	d.PrepareBatch = ledger.PrepareBatch
	if ms := workloadInt("FUTUREQ_WORKLOAD_PREPARE_MS", 0); ms > 0 {
		d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
			timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
				return ledger.PrepareBatch(ctx, items)
			}
		}
	}
	d.MaintenanceRecipients = hub.LocalDeliveryRecipients
	d.PreloadLookahead = time.Second
	d.PreloadMaxMessages = 512
	if os.Getenv("FUTUREQ_WORKLOAD_MODE") == "due-only" {
		d.PreloadLookahead = 0
		d.PreloadMaxMessages = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	latencies := make([]float64, 0, count)
	enqueuedBefore := workloadEnqueueBuckets(topicCount)
	seen := make(map[string]bool, count)
	duplicates := 0
	var intentionalRetries atomic.Int64
	received := make(chan struct{}, count)
	for i := range topicCount {
		topic := fmt.Sprintf("topic-%d", i)
		id := topic
		ch := make(chan *pb.QueueMessage, 1024)
		hub.Register(id, topic, "workers", ch)
		wg.Add(1)
		go func() {
			defer wg.Done()
			nacked := make(map[string]bool)
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-ch:
					if !hub.BeginSend(id, msg) {
						continue
					}
					if every := workloadInt("FUTUREQ_WORKLOAD_NACK_EVERY", 0); every > 0 && binary.BigEndian.Uint64(msg.DeliveryTag[16:])%uint64(every) == 0 && !nacked[string(msg.DeliveryTag)] {
						nacked[string(msg.DeliveryTag)] = true
						intentionalRetries.Add(1)
						hub.AcknowledgeSent(id, msg.DeliveryTag)
						continue
					}
					if ms := workloadInt("FUTUREQ_WORKLOAD_SLOW_CONSUMER_MS", 0); ms > 0 && id == "topic-0" {
						timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
						select {
						case <-ctx.Done():
							timer.Stop()
							return
						case <-timer.C:
						}
					}
					late := max(0, float64(time.Now().UnixNano())/1e6-float64(msg.EnqueuedAtUnixMs+msg.DelayMs))
					mu.Lock()
					if seen[string(msg.DeliveryTag)] {
						duplicates++
					} else {
						seen[string(msg.DeliveryTag)] = true
						latencies = append(latencies, late)
						received <- struct{}{}
					}
					mu.Unlock()
					if hub.MarkAcknowledgedSent(id, msg.DeliveryTag) {
						deleter.MarkAcknowledged(msg.DeliveryTag, "g:workers")
					}
				}
			}
		}()
	}
	wg.Add(2)
	go func() { defer wg.Done(); d.Run(ctx) }()
	go func() { defer wg.Done(); deleter.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()
	var peak atomic.Uint64
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	cpu := workloadCPU()
	started := time.Now()
	peak.Store(before.HeapAlloc)
	sampleDone := make(chan struct{})
	go func() {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		defer close(sampleDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
					if peak.CompareAndSwap(old, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	payload := make([]byte, size)
	seed := uint32(7)
	for i := range payload {
		seed = 1664525*seed + 1013904223
		payload[i] = byte(seed >> 24)
	}
	const batchSize = 16
	for offset := 0; offset < count; offset += batchSize {
		target := started.Add(time.Duration(offset) * time.Second / time.Duration(rate))
		if wait := time.Until(target); wait > 0 {
			time.Sleep(wait)
		}
		batch := db.NewBatch()
		for i := offset; i < min(offset+batchSize, count); i++ {
			topic := fmt.Sprintf("topic-%d", i%topicCount)
			now := time.Now().UnixMilli()
			body := payload
			if size == 0 {
				n := 1024
				if i%8 == 0 {
					n = 65536
				}
				body = make([]byte, n)
			}
			msg := &storagepb.StoredMessage{Topic: topic, Payload: body, EnqueuedAtUnixMs: now, DelayMs: int64(delay)}
			value, e := proto.Marshal(msg)
			require.NoError(e)
			key := utils.EventKey(utils.CalculateBucket(now+int64(delay), d.TimeBucket), utils.TopicHash(topic), uint64(i+1))
			require.NoError(batch.Set(key, value))
		}
		require.NoError(batch.Commit(storage.Sync))
		require.NoError(batch.Close())
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	for range count {
		select {
		case <-received:
		case <-timeout.C:
			s.T().Fatal("workload delivery stalled")
		}
	}
	elapsed := time.Since(started).Seconds()
	cpu = workloadCPU() - cpu
	cancel()
	wg.Wait()
	<-sampleDone
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	enqueueAfter := workloadEnqueueBuckets(topicCount)
	for i := range enqueueAfter {
		enqueueAfter[i].count -= enqueuedBefore[i].count
	}
	mu.Lock()
	sort.Float64s(latencies)
	result := map[string]any{"engine": engine, "mode": os.Getenv("FUTUREQ_WORKLOAD_MODE"), "messages": count, "payload_bytes": size, "delay_ms": delay, "topics": topicCount, "offered_per_second": rate, "elapsed_seconds": elapsed, "throughput": float64(count) / elapsed, "cpu_seconds": cpu, "allocated_bytes_per_message": float64(after.TotalAlloc-before.TotalAlloc) / float64(count), "peak_heap_bytes": peak.Load(), "payload_gets_per_message": float64(db.payloadGets.Load()) / float64(count), "metadata_gets_per_message": float64(db.metadataGets.Load()) / float64(count), "range_scans": db.scans.Load(), "duplicates": duplicates, "intentional_retries": intentionalRetries.Load(), "send_p50_ms": latencies[(len(latencies)-1)*50/100], "send_p95_ms": latencies[(len(latencies)-1)*95/100], "send_p99_ms": latencies[(len(latencies)-1)*99/100], "enqueue_p50_bucket_ms": workloadQuantile(enqueueAfter, 0.5), "enqueue_p95_bucket_ms": workloadQuantile(enqueueAfter, 0.95), "enqueue_p99_bucket_ms": workloadQuantile(enqueueAfter, 0.99)}
	mu.Unlock()
	encoded, e := json.Marshal(result)
	require.NoError(e)
	s.T().Log("WORKLOAD " + string(encoded))
	require.Zero(duplicates)
}

type workloadBucket struct {
	upper float64
	count uint64
}

func workloadEnqueueBuckets(topics int) []workloadBucket {
	var result []workloadBucket
	for i := range topics {
		var metric dto.Metric
		_ = metrics.DeliverySenderEnqueueLatenessMs.WithLabelValues(fmt.Sprintf("topic-%d", i)).(interface{ Write(*dto.Metric) error }).Write(&metric)
		if result == nil {
			result = make([]workloadBucket, len(metric.GetHistogram().Bucket))
		}
		for j, b := range metric.GetHistogram().Bucket {
			result[j].upper = b.GetUpperBound()
			result[j].count += b.GetCumulativeCount()
		}
	}
	return result
}
func workloadQuantile(buckets []workloadBucket, q float64) float64 {
	if len(buckets) == 0 {
		return 0
	}
	target := float64(buckets[len(buckets)-1].count) * q
	for _, bucket := range buckets {
		if float64(bucket.count) >= target {
			return bucket.upper
		}
	}
	return buckets[len(buckets)-1].upper
}

func (s *SchedulerSuite) TestSchedulerIdleWorkload() {
	if os.Getenv("FUTUREQ_IDLE_WORKLOAD") == "" {
		s.T().Skip("opt-in idle measurement")
	}
	d, _ := newPipelineTest(s.T())
	if os.Getenv("FUTUREQ_WORKLOAD_MODE") != "due-only" {
		d.PreloadLookahead = time.Second
		d.PreloadMaxMessages = 512
	}
	if os.Getenv("FUTUREQ_IDLE_ACTIVE") == "1" {
		d.hub.Register("local", "empty", "workers", make(chan *pb.QueueMessage, 1))
	}
	db := &workloadDB{DB: d.db}
	d.db = db
	d.interval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cpu := workloadCPU()
	d.Run(ctx)
	cpu = workloadCPU() - cpu
	runtime.ReadMemStats(&after)
	s.T().Logf("IDLE active=%s mode=%s cpu_seconds=%.6f allocated_bytes=%d scans=%d", os.Getenv("FUTUREQ_IDLE_ACTIVE"), os.Getenv("FUTUREQ_WORKLOAD_MODE"), cpu, after.TotalAlloc-before.TotalAlloc, db.scans.Load())
}
