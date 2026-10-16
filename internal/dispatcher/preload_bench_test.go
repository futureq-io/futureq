package dispatcher

import (
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// This compares collection at the due instant, including rebuilding the cached
// metadata for each iteration. It excludes preparation, delivery, and the earlier
// speculative scan; it is not a broker throughput benchmark.
func BenchmarkDueCandidateCollection(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "scan"
		if cached {
			name = "preloaded"
		}
		b.Run(name, func(b *testing.B) {
			db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			wake := make(chan struct{}, 1)
			hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wake)
			hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 64))
			d := NewDispatcher(db, hub, nil, time.Millisecond, time.Second, wake, zap.NewNop())
			d.TimeBucket, d.Limits.WorkTime = time.Millisecond, time.Hour
			now := time.Now().UnixMilli()
			value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: make([]byte, 4096), EnqueuedAtUnixMs: now - 1000})
			if err != nil {
				b.Fatal(err)
			}
			batch := db.NewBatch()
			candidates := make(candidateHeap, 64)
			for i := range candidates {
				key := utils.EventKey(uint64(now-1000), utils.TopicHash("orders"), uint64(i))
				candidates[i] = preloadCandidate{key: [24]byte(key), due: now - 1000}
				if err := batch.Set(key, value); err != nil {
					b.Fatal(err)
				}
			}
			if err := batch.Commit(storage.NoSync); err != nil {
				b.Fatal(err)
			}
			_ = batch.Close()
			view := hub.InterestView("orders")
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var chunk deliveryChunk
				if cached {
					p := &topicPreload{items: append(candidateHeap(nil), candidates...), keys: make(map[[24]byte]bool, len(candidates))}
					for _, candidate := range candidates {
						p.keys[candidate.key] = true
					}
					d.preloadCount = len(candidates)
					chunk = d.collectPreloaded("orders", p, view, now)
				} else {
					chunk = d.collectTopic("orders", now)
				}
				if len(chunk.items) != len(candidates) {
					b.Fatalf("collected %d candidates, expected %d", len(chunk.items), len(candidates))
				}
			}
		})
	}
}
