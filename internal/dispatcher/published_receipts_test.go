package dispatcher

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

type publishedReceiptReadDB struct {
	storage.DB
	reads int
}

func (db *publishedReceiptReadDB) Get(key []byte) ([]byte, io.Closer, error) {
	if bytes.HasPrefix(key, []byte("metadata/delivery/")) {
		db.reads++
	}
	return db.DB.Get(key)
}

func TestPublishedDispatchReadsCurrentReceiptOnce(t *testing.T) {
	for _, mode := range []string{"pending", "acknowledged", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			d, ledger := newPreloadTest(t, "pebble")
			d.PrepareBatch = nil
			d.PublishedReceipts = true
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			now := time.Now().UnixMilli()
			key := putPipelineEvent(t, d, "orders", 1, now-1000)
			epoch := d.hub.InterestView("orders").Epoch
			prepare := raft.DeliveryPrepare{Key: key, Epoch: epoch, Recipients: []string{"g:workers"}}
			if mode == "unbound" {
				_, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{prepare})
				require.NoError(t, err)
			} else {
				receipt, err := raft.MarshalPublishReceipt(epoch, []string{"g:workers", "g:offline"})
				require.NoError(t, err)
				batch := d.db.NewBatch()
				require.NoError(t, batch.Set(raft.DeliveryStateKey(key), receipt))
				require.NoError(t, batch.Commit(storage.NoSync))
				require.NoError(t, batch.Close())
			}

			chunk := d.collectScheduledTopic("orders", now)
			require.Len(t, chunk.items, 1)
			observed := &publishedReceiptReadDB{DB: d.db}
			d.db = observed
			states, err := d.prepareChunk(context.Background(), chunk)
			require.NoError(t, err)
			require.Zero(t, observed.reads)
			if mode == "acknowledged" {
				require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:workers"}}))
			}
			sent := d.deliverChunk(chunk, states)
			require.Equal(t, 1, observed.reads)
			if mode == "pending" {
				require.Equal(t, 1, sent)
				require.Len(t, ch, 1)
				require.Equal(t, key, (<-ch).DeliveryTag)
			} else {
				require.Zero(t, sent)
				require.Empty(t, ch)
			}
		})
	}
}
