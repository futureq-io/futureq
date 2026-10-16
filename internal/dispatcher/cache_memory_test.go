package dispatcher

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestFullMessageCacheSizing(t *testing.T) {
	if os.Getenv("FUTUREQ_CACHE_MEMORY") == "" {
		t.Skip("opt-in retained-cache measurement")
	}
	oldRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = oldRate }()
	for _, size := range []int{1024, 65536, 102400} {
		t.Run(stringSize(size), func(t *testing.T) {
			d, _ := newPreloadTest(t, "pebble")
			d.PreloadMaxMessages = 512
			d.PreloadLookahead = time.Minute
			d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
			now := time.Now().UnixMilli()
			due := now + 30000
			value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: make([]byte, size), EnqueuedAtUnixMs: due - 1000, DelayMs: 1000})
			require.NoError(t, err)
			batch := d.db.NewBatch()
			for i := range d.PreloadMaxMessages {
				require.NoError(t, batch.Set(utils.EventKey(utils.CalculateBucket(due, d.TimeBucket), utils.TopicHash("orders"), uint64(i)), value))
			}
			require.NoError(t, batch.Commit(storage.Sync))
			require.NoError(t, batch.Close())
			require.NoError(t, d.db.Flush())
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			for i := 0; i < 2000 && d.preloadCount < d.PreloadMaxMessages; i++ {
				require.Empty(t, d.collectScheduledTopic("orders", now).items)
			}
			require.Equal(t, 512, d.preloadCount)
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			runtime.GC()
			runtime.GC()
			profileBytes := liveCacheAllocationBytes()
			bytes := 0
			for _, p := range d.preloads {
				for _, entry := range p.items {
					bytes += len(entry.payload.message.Payload)
				}
			}
			t.Logf("CACHE_MEMORY count=%d payload_size=%d payload_bytes=%d live_heap_delta=%d candidate_struct=%d stored_message_struct=%d cache_profile_inuse_bytes=%d", d.preloadCount, size, bytes, int64(after.HeapAlloc)-int64(before.HeapAlloc), unsafe.Sizeof(preloadCandidate{}), unsafe.Sizeof(storagepb.StoredMessage{}), profileBytes)
			require.Equal(t, 512*size, bytes)
			d.prunePreloads(nil)
			require.Zero(t, d.preloadCount)
		})
	}
}
func stringSize(n int) string {
	if n == 1024 {
		return "1KiB"
	}
	if n == 65536 {
		return "64KiB"
	}
	return "100KiB"
}

func liveCacheAllocationBytes() int64 {
	n, _ := runtime.MemProfile(nil, true)
	records := make([]runtime.MemProfileRecord, n+100)
	n, _ = runtime.MemProfile(records, true)
	var total int64
	for _, record := range records[:n] {
		frames := runtime.CallersFrames(record.Stack())
		for {
			frame, more := frames.Next()
			if strings.Contains(frame.Function, "dispatcher.cachePayload") {
				total += record.InUseBytes()
				break
			}
			if !more {
				break
			}
		}
	}
	return total
}
