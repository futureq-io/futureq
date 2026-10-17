package dispatcher

// This workload intentionally uses only the pre-migration public/test adapter
// surface, so the exact file can also run against cd2109f in a disposable tree.
import (
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
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

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

func TestSchedulerWorkload(t *testing.T) {
	if os.Getenv("FUTUREQ_WORKLOAD") == "" {
		t.Skip("opt-in local end-to-end workload")
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
		raw, err = storage.NewBoltDB(config.Bolt{File: filepath.Join(t.TempDir(), "data.db")})
	} else {
		cfg := config.Pebble{Mode: "memory"}
		if engine == "pebble-disk" {
			cfg.Mode = "disk"
			cfg.DataDir = t.TempDir()
			cfg.WALEnabled = true
		}
		raw, err = storage.NewPebble(cfg, zap.NewNop())
	}
	require.NoError(t, err)
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
			require.NoError(t, e)
			key := utils.EventKey(utils.CalculateBucket(now+int64(delay), d.TimeBucket), utils.TopicHash(topic), uint64(i+1))
			require.NoError(t, batch.Set(key, value))
		}
		require.NoError(t, batch.Commit(storage.Sync))
		require.NoError(t, batch.Close())
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
			t.Fatal("workload delivery stalled")
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
	require.NoError(t, e)
	t.Log("WORKLOAD " + string(encoded))
	require.Zero(t, duplicates)
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

func TestSchedulerIdleWorkload(t *testing.T) {
	if os.Getenv("FUTUREQ_IDLE_WORKLOAD") == "" {
		t.Skip("opt-in idle measurement")
	}
	d, _ := newPipelineTest(t)
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
	t.Logf("IDLE active=%s mode=%s cpu_seconds=%.6f allocated_bytes=%d scans=%d", os.Getenv("FUTUREQ_IDLE_ACTIVE"), os.Getenv("FUTUREQ_WORKLOAD_MODE"), cpu, after.TotalAlloc-before.TotalAlloc, db.scans.Load())
}
