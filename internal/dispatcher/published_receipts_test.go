package dispatcher

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	dto "github.com/prometheus/client_model/go"
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

func TestPublishedDispatchReportsReceiptReadFailureAndContinues(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.PrepareBatch = nil
	d.PublishedReceipts = true
	messages := make(chan *pb.QueueMessage, 3)
	d.hub.Register("local", "orders", "workers", messages)
	now := time.Now().UnixMilli()
	corruptKey := putPipelineEvent(t, d, "orders", 1, now-1000)
	putPipelineEvent(t, d, "orders", 2, now-1000) // Missing receipt is ordinary.
	healthyKey := putPipelineEvent(t, d, "orders", 3, now-1000)
	receipt, err := raft.MarshalPublishReceipt(d.hub.InterestView("orders").Epoch, []string{"g:workers"})
	require.NoError(t, err)
	batch := d.db.NewBatch()
	require.NoError(t, batch.Set(raft.DeliveryStateKey(corruptKey), []byte("corrupt receipt")))
	require.NoError(t, batch.Set(raft.DeliveryStateKey(healthyKey), receipt))
	require.NoError(t, batch.Commit(storage.NoSync))
	require.NoError(t, batch.Close())

	counter := metrics.DeliveryRejectedTotal.WithLabelValues("receipt_read")
	var before, after, hitsBefore, hitsAfter dto.Metric
	require.NoError(t, counter.Write(&before))
	require.NoError(t, metrics.DeliveryManifestHitsTotal.Write(&hitsBefore))
	require.Equal(t, 1, d.dispatchTopic("orders", now))
	require.NoError(t, counter.Write(&after))
	require.NoError(t, metrics.DeliveryManifestHitsTotal.Write(&hitsAfter))
	require.Equal(t, before.GetCounter().GetValue()+1, after.GetCounter().GetValue())
	require.Equal(t, hitsBefore.GetCounter().GetValue()+1, hitsAfter.GetCounter().GetValue())
	require.Len(t, messages, 1)
	require.Equal(t, healthyKey, (<-messages).DeliveryTag)
}

type slowPublishedReadDB struct {
	storage.DB
	key   []byte
	delay time.Duration
}

func (db slowPublishedReadDB) Get(key []byte) ([]byte, io.Closer, error) {
	if bytes.Equal(key, db.key) {
		time.Sleep(db.delay)
	}
	return db.DB.Get(key)
}

func TestPublishedReceiptUsesRemainingValidationBudget(t *testing.T) {
	for _, slowRead := range []string{"preparation_payload", "delivery_payload", "receipt"} {
		t.Run(slowRead, func(t *testing.T) {
			d, _ := newPreloadTest(t, "pebble")
			d.PrepareBatch = nil
			d.PublishedReceipts = true
			d.Limits.ValidationTimeout = 20 * time.Millisecond
			messages := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", messages)
			now := time.Now().UnixMilli()
			key := putPipelineEvent(t, d, "orders", 1, now-1000)
			next := putPipelineEvent(t, d, "orders", 2, now-999)
			receiptKey := raft.DeliveryStateKey(key)
			receipt, err := raft.MarshalPublishReceipt(d.hub.InterestView("orders").Epoch, []string{"g:workers"})
			require.NoError(t, err)
			batch := d.db.NewBatch()
			require.NoError(t, batch.Set(receiptKey, receipt))
			require.NoError(t, batch.Set(raft.DeliveryStateKey(next), receipt))
			require.NoError(t, batch.Commit(storage.NoSync))
			require.NoError(t, batch.Close())
			chunk := d.collectScheduledTopic("orders", now)
			require.Len(t, chunk.items, 2)

			delayKey := key
			if slowRead == "receipt" {
				delayKey = receiptKey
			} else {
				delete(chunk.payloads, string(key))
			}
			observed := &publishedReceiptReadDB{DB: slowPublishedReadDB{DB: d.db, key: delayKey, delay: 40 * time.Millisecond}}
			d.db = observed
			switch slowRead {
			case "preparation_payload":
				started := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), d.Limits.ValidationTimeout)
				defer cancel()
				_, err := d.prepareChunk(ctx, chunk)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				chunk.validationTime = time.Since(started)
			default:
				chunk.validationTime = 19 * time.Millisecond
			}
			sent := d.deliverChunk(chunk, nil)
			if slowRead == "receipt" {
				require.Equal(t, 1, sent, "a successful receipt read started within budget")
				require.Equal(t, 1, observed.reads, "the spent budget prevents reading the second receipt")
				require.Len(t, messages, 1)
				require.Equal(t, key, (<-messages).DeliveryTag)
			} else {
				require.Zero(t, sent)
				require.Empty(t, messages)
				require.Zero(t, observed.reads, "expired payload validation must prevent a receipt lookup")
			}
		})
	}
}

func TestPublishedDispatchProgressesPastSlowOldestReceipt(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.PrepareBatch = nil
	d.PublishedReceipts = true
	d.Limits.ValidationTimeout = 10 * time.Millisecond
	messages := make(chan *pb.QueueMessage, 2)
	d.hub.Register("local", "orders", "workers", messages)
	now := time.Now().UnixMilli()
	oldest := putPipelineEvent(t, d, "orders", 1, now-1000)
	healthy := putPipelineEvent(t, d, "orders", 2, now-999)
	receipt, err := raft.MarshalPublishReceipt(d.hub.InterestView("orders").Epoch, []string{"g:workers"})
	require.NoError(t, err)
	batch := d.db.NewBatch()
	require.NoError(t, batch.Set(raft.DeliveryStateKey(oldest), receipt))
	require.NoError(t, batch.Set(raft.DeliveryStateKey(healthy), receipt))
	require.NoError(t, batch.Commit(storage.NoSync))
	require.NoError(t, batch.Close())
	d.db = slowPublishedReadDB{DB: d.db, key: raft.DeliveryStateKey(oldest), delay: 30 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for _, key := range [][]byte{oldest, healthy} {
		select {
		case msg := <-messages:
			require.Equal(t, key, msg.DeliveryTag)
		case <-timeout.C:
			t.Fatal("delivery stalled behind the slow receipt")
		}
	}
}
