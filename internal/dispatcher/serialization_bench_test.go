package dispatcher

import (
	"context"
	"fmt"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"go.uber.org/zap"
)

// Measures the complete ledger path, including storage and instrumentation.
func BenchmarkDeliveryPrepareBatch(b *testing.B) {
	for _, refresh := range []bool{false, true} {
		b.Run(fmt.Sprintf("refresh=%t", refresh), func(b *testing.B) {
			db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			ledger := NewDeliveryLedger(db, nil, nil, nil)
			items := make([]raft.DeliveryPrepare, 64)
			batch := db.NewBatch()
			for i := range items {
				items[i] = raft.DeliveryPrepare{Key: utils.EventKey(1, 2, uint64(i+1)), Epoch: 1, Recipients: []string{"g:a", "g:b", "g:c", "u:one"}}
				if err := batch.Set(items[i].Key, []byte("payload")); err != nil {
					b.Fatal(err)
				}
			}
			if err := batch.Commit(storage.NoSync); err != nil {
				b.Fatal(err)
			}
			batch.Close()
			if _, err := ledger.PrepareBatch(context.Background(), items); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if refresh {
					for i := range items {
						items[i].Epoch++
					}
				}
				if _, err := ledger.PrepareBatch(context.Background(), items); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
