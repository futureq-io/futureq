package handlers

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type standaloneReceiptDB struct {
	storage.DB
	receiptReads      atomic.Int64
	beforeReceiptRead func([]byte)
	committed         chan struct{}
	finish            chan struct{}
}

func (db *standaloneReceiptDB) Get(key []byte) ([]byte, io.Closer, error) {
	if bytes.HasPrefix(key, []byte("metadata/delivery/")) {
		db.receiptReads.Add(1)
		if db.beforeReceiptRead != nil {
			db.beforeReceiptRead(key)
		}
	}
	return db.DB.Get(key)
}

func (db *standaloneReceiptDB) NewBatch() storage.Batch {
	return &standaloneReceiptBatch{Batch: db.DB.NewBatch(), db: db}
}

type standaloneReceiptBatch struct {
	storage.Batch
	db *standaloneReceiptDB
}

func (b *standaloneReceiptBatch) Commit(mode storage.SyncMode) error {
	err := b.Batch.Commit(mode)
	if err == nil && mode == storage.Sync && b.db.committed != nil {
		close(b.db.committed)
		<-b.db.finish
	}
	return err
}

func TestStandaloneReceiptsServePartialAndFinalAcksFromCache(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			a, _, producer, _ := openPublishTest(t, engine)
			first, err := a.Subscriptions.Create(context.Background(), "orders", "first", "")
			require.NoError(t, err)
			second, err := a.Subscriptions.Create(context.Background(), "orders", "second", "")
			require.NoError(t, err)
			db := &standaloneReceiptDB{DB: a.DB}
			a.DB = db
			ledger := dispatcher.NewDeliveryLedger(db, nil, nil, nil)
			ledger.StandaloneReceipts = raft.NewStandaloneReceiptCache(1<<20, 16)
			producer.StandalonePublishLedger = ledger

			_, err = producer.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{
				{Topic: "orders"}, {Topic: "orders"},
			}})
			require.NoError(t, err)
			keys := publishedKeys(t, db)
			require.Len(t, keys, 2)

			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: keys[0], Recipient: first.Recipient}}))
			state, err := raft.ReadDeliveryState(db.DB, keys[0])
			require.NoError(t, err)
			require.False(t, state.Needs(first.Recipient))
			require.True(t, state.Needs(second.Recipient))
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{
				{Key: keys[0], Recipient: second.Recipient},
				{Key: keys[1], Recipient: second.Recipient},
			}))
			require.EqualValues(t, 0, db.receiptReads.Load())
			require.Equal(t, keys[1:], publishedKeys(t, db), "a shared roster must keep ACK state independent per key")
			state, err = raft.ReadDeliveryState(db.DB, keys[0])
			require.NoError(t, err)
			require.Nil(t, state)
			state, err = raft.ReadDeliveryState(db.DB, keys[1])
			require.NoError(t, err)
			require.True(t, state.Needs(first.Recipient))
		})
	}
}

func TestStandalonePublishCachePreservesConcurrentReceiptChanges(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "partial ACK"
		if remove {
			name = "TTL delete"
		}
		t.Run(name, func(t *testing.T) {
			a, _, producer, _ := openPublishTest(t, "pebble")
			first, err := a.Subscriptions.Create(context.Background(), "orders", "first", "")
			require.NoError(t, err)
			second, err := a.Subscriptions.Create(context.Background(), "orders", "second", "")
			require.NoError(t, err)
			db := &standaloneReceiptDB{DB: a.DB, committed: make(chan struct{}), finish: make(chan struct{})}
			a.DB = db
			ledger := dispatcher.NewDeliveryLedger(db, nil, dispatcher.NewDirectDeleteBackend(db, zap.NewNop()), nil)
			ledger.StandaloneReceipts = raft.NewStandaloneReceiptCache(1<<20, 16)
			producer.StandalonePublishLedger = ledger

			publishDone := make(chan struct{})
			var publishErr error
			go func() {
				defer close(publishDone)
				_, publishErr = producer.processBatch(context.Background(), &pb.PublishBatch{Messages: []*pb.PublishMessage{
					{Topic: "orders"}, {Topic: "orders"},
				}})
			}()
			defer func() { <-publishDone }()
			finish := sync.OnceFunc(func() { close(db.finish) })
			defer finish()
			select {
			case <-db.committed:
			case <-time.After(5 * time.Second):
				t.Fatal("publish did not commit")
			}
			keys := publishedKeys(t, db)
			require.Len(t, keys, 2)

			mutationDone := make(chan error, 1)
			go func() {
				if remove {
					mutationDone <- ledger.DeleteKeys(keys[:1])
					return
				}
				mutationDone <- ledger.Acknowledge([]raft.DeliveryAck{{Key: keys[0], Recipient: first.Recipient}})
			}()
			select {
			case err := <-mutationDone:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				finish()
				<-mutationDone
				t.Fatal("receipt mutation blocked on publish sync")
			}
			finish()
			<-publishDone
			require.NoError(t, publishErr)

			reads := db.receiptReads.Load()
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: keys[1], Recipient: second.Recipient}}))
			require.Equal(t, reads, db.receiptReads.Load(), "an unrelated receipt should still be cached")
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: keys[0], Recipient: second.Recipient}}))
			require.Equal(t, keys[1:], publishedKeys(t, db))
			state, err := raft.ReadDeliveryState(db.DB, keys[0])
			require.NoError(t, err)
			require.Nil(t, state, "delayed cache admission must not restore ACKed or deleted state")
		})
	}
}

func TestStandalonePublishCacheCommitsWhileUnrelatedAckReadsReceipt(t *testing.T) {
	ctx := context.Background()
	a, _, producer, _ := openPublishTest(t, "pebble")
	first, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	require.NoError(t, a.Subscriptions.CreateTopic(ctx, "other", nil))
	second, err := a.Subscriptions.Create(ctx, "other", "workers", "")
	require.NoError(t, err)

	// Seed a receipt in storage before installing the cache.
	_, err = producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}})
	require.NoError(t, err)
	keys := publishedKeys(t, a.DB)
	require.Len(t, keys, 1)
	blockedKey := raft.DeliveryStateKey(keys[0])
	readStarted := make(chan struct{})
	resumeRead := make(chan struct{})
	db := &standaloneReceiptDB{
		DB: a.DB,
		beforeReceiptRead: func(key []byte) {
			if bytes.Equal(key, blockedKey) {
				close(readStarted)
				<-resumeRead
			}
		},
	}
	a.DB = db
	ledger := dispatcher.NewDeliveryLedger(db, nil, nil, nil)
	ledger.StandaloneReceipts = raft.NewStandaloneReceiptCache(1<<20, 16)
	producer.StandalonePublishLedger = ledger

	ackDone := make(chan struct{})
	var ackErr error
	go func() {
		defer close(ackDone)
		ackErr = ledger.Acknowledge([]raft.DeliveryAck{{Key: keys[0], Recipient: first.Recipient}})
	}()
	release := sync.OnceFunc(func() { close(resumeRead) })
	defer func() {
		release()
		<-ackDone
	}()
	select {
	case <-readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("ACK did not reach its receipt read")
	}

	publishDone := make(chan struct{})
	var publishErr error
	go func() {
		defer close(publishDone)
		_, publishErr = producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "other"}}})
	}()
	defer func() {
		release()
		<-publishDone
	}()
	select {
	case <-publishDone:
		require.NoError(t, publishErr)
		require.Len(t, publishedKeys(t, db), 2)
	case <-time.After(5 * time.Second):
		t.Fatal("publish waited for an unrelated ACK receipt read")
	}

	release()
	<-ackDone
	require.NoError(t, ackErr)
	keys = publishedKeys(t, db)
	require.Len(t, keys, 1)
	require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: keys[0], Recipient: second.Recipient}}))
	require.EqualValues(t, 1, db.receiptReads.Load(), "the unrelated publish still warms the cache")
}
