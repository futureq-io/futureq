package raft

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"go.uber.org/zap"
)

func BenchmarkDeliveryAckOverlay(b *testing.B) {
	for _, fanout := range []int{8, 64} {
		b.Run(fmt.Sprintf("recipients=%d", fanout), func(b *testing.B) {
			db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			key := utils.EventKey(1, 2, 3)
			state := &DeliveryState{Epoch: 1, Recipients: make([]string, fanout)}
			cmds := make([][]byte, fanout-1) // keep one interest pending
			for i := 0; i < fanout; i++ {
				recipient := fmt.Sprintf("g:%d", i)
				state.Recipients[i] = recipient
				if i < len(cmds) {
					cmds[i], err = MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: recipient}})
					if err != nil {
						b.Fatal(err)
					}
				}
			}
			state = newDeliveryState(state.Epoch, state.Recipients)
			data, err := marshalDeliveryState(state)
			if err != nil {
				b.Fatal(err)
			}
			batch := db.NewBatch()
			if err := batch.Set(key, []byte("payload")); err != nil {
				b.Fatal(err)
			}
			if err := batch.Set(DeliveryStateKey(key), data); err != nil {
				b.Fatal(err)
			}
			if err := batch.Commit(storage.NoSync); err != nil {
				b.Fatal(err)
			}
			batch.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				batch := db.NewBatch()
				delivery := NewDeliveryBatch(db, batch)
				for _, cmd := range cmds {
					if _, _, err := delivery.Apply(cmd); err != nil {
						b.Fatal(err)
					}
				}
				// The baseline writes eagerly. The optimized path flushes at this boundary.
				if flusher, ok := any(delivery).(interface{ Flush() error }); ok {
					if err := flusher.Flush(); err != nil {
						b.Fatal(err)
					}
				}
				if err := batch.Close(); err != nil {
					b.Fatal(err)
				} // discard so each iteration starts with the same receipt
			}
		})
	}
}

func BenchmarkDeliveryPrepareSizing(b *testing.B) {
	item := DeliveryPrepare{Key: utils.EventKey(1, 2, 3), Epoch: 123456789, Recipients: []string{"g:a", "g:b", "u:one", "g:<>&\u2028"}}
	b.Run("json", func(b *testing.B) {
		total := 0
		b.ReportAllocs()
		for b.Loop() {
			data, err := json.Marshal(item)
			if err != nil {
				b.Fatal(err)
			}
			total += len(data)
		}
		if total == 0 {
			b.Fatal("empty size")
		}
	})
	b.Run("exact", func(b *testing.B) {
		total := 0
		b.ReportAllocs()
		for b.Loop() {
			total += DeliveryPrepareSize(item)
		}
		if total == 0 {
			b.Fatal("empty size")
		}
	})
}
