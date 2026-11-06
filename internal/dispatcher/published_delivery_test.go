package dispatcher

import (
	"context"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func putPublishedEvent(t *testing.T, d *Dispatcher, id uint64, due int64, names []string) []byte {
	t.Helper()
	msg, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("payload"), EnqueuedAtUnixMs: due})
	require.NoError(t, err)
	key := utils.EventKey(utils.CalculateBucket(due, d.TimeBucket), utils.TopicHash("orders"), id)
	receipt, err := raft.MarshalPublishReceipt(0, names)
	require.NoError(t, err)
	b := d.db.NewBatch()
	defer b.Close() //nolint:errcheck
	require.NoError(t, b.Set(key, msg))
	require.NoError(t, b.Set(raft.DeliveryStateKey(key), receipt))
	require.NoError(t, b.Commit(storage.Sync))
	return key
}

func TestPublishBoundDeliverySkipsCommitsAndKeepsOfflineGroups(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, ledger := newPreloadTest(t, engine)
			d.PrepareBatch = ledger.PublishedBatch
			a, b, late := make(chan *pb.QueueMessage, 4), make(chan *pb.QueueMessage, 4), make(chan *pb.QueueMessage, 4)
			d.hub.Register("a", "orders", "a", a)
			key := putPublishedEvent(t, d, 1, time.Now().Add(-time.Second).UnixMilli(), []string{"g:a", "g:b"})
			observed := &ledgerCommitModeDB{DB: d.db}
			ledger.db = observed
			barriers := 0
			d.ReadBarrier = func() error { barriers++; return nil }
			require.Equal(t, 1, d.dispatchAll())
			require.Len(t, a, 1)
			require.Empty(t, observed.modes, "delivery must not create a storage batch")
			require.Equal(t, 1, barriers, "no per-batch post-preparation read")
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
			d.hub.Unregister("a")
			state, err := raft.ReadDeliveryState(d.db, key)
			require.NoError(t, err)
			require.True(t, state.Needs("g:b"), "offline B must retain its obligation")
			d.hub.Register("b-reconnected", "orders", "b", b)
			d.hub.Register("new-group", "orders", "late", late)
			require.Equal(t, 1, d.dispatchAll())
			require.Len(t, b, 1)
			require.Empty(t, late, "groups created after publication must not receive history")
			require.Equal(t, []storage.SyncMode{storage.NoSync}, observed.modes)
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:b"}}))
			_, _, err = d.db.Get(key)
			require.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
}

func TestPublishBoundDeliveryStillFencesOwnershipAndDueTime(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	ch := make(chan *pb.QueueMessage, 2)
	d.hub.Register("a", "orders", "a", ch)
	putPublishedEvent(t, d, 1, time.Now().Add(time.Hour).UnixMilli(), []string{"g:a"})
	require.Zero(t, d.dispatchAll(), "publish-time receipts cannot permit early delivery")
	key := putPublishedEvent(t, d, 2, time.Now().Add(-time.Second).UnixMilli(), []string{"g:a"})
	epoch, active := uint64(99), true
	d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, active }, time.Second)
	d.ReadBarrier = func() error { d.hub.GrantDeliveryPermit(epoch, time.Now()); return nil }
	require.Equal(t, 1, d.dispatchAll())
	msg := <-ch
	require.Equal(t, key, msg.DeliveryTag)
	d.hub.WithRebalanceBarrier(func() { epoch++; active = false })
	require.False(t, d.hub.BeginSend("a", msg), "frozen rosters must not bypass actual-send fences")
}

func TestPublishBoundUniversalCleanupRunsOutsideDelivery(t *testing.T) {
	d, ledger := newPreloadTest(t, "pebble")
	d.hub.Register("a", "orders", "a", make(chan *pb.QueueMessage, 2))
	key := putPublishedEvent(t, d, 1, time.Now().Add(-time.Second).UnixMilli(), []string{"g:a", "u:gone"})
	view := d.hub.InterestView("orders")
	items := []raft.DeliveryPrepare{{Key: key, Epoch: view.Epoch, Recipients: view.Recipients}}
	states, err := ledger.PublishedBatch(context.Background(), items)
	require.NoError(t, err)
	require.True(t, states[string(key)].Needs("u:gone"))
	_, err = ledger.ReconcileBatch(context.Background(), items)
	require.NoError(t, err)
	state, err := raft.ReadDeliveryState(d.db, key)
	require.NoError(t, err)
	require.True(t, state.PublishBound)
	require.False(t, state.Needs("u:gone"))
	require.True(t, state.Needs("g:a"))
}

func TestPublishBoundRaftFollowerReconnectAndRestart(t *testing.T) {
	c := newReplicaTestCluster(t)
	for _, node := range c.nodes {
		reader := NewDeliveryLedger(node.db, nil, nil, nil)
		node.dispatcher.PrepareBatch = reader.PublishedBatch
	}
	a := c.subscribe(1, "a", "a")
	c.subscribe(2, "b", "b")
	c.activate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	node := c.nodes[1]
	for _, group := range []string{"a", "b"} {
		cmd, err := raft.MarshalRegisterDeliveryGroupCmd("orders", group)
		require.NoError(t, err)
		_, err = node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), cmd)
		require.NoError(t, err)
	}
	c.nodes[2].hub.Unregister("b")
	require.NoError(t, c.nodes[2].svc.ChangeConsumer(ctx, replicaTestShard, "orders", "b", metadata.ConsumerMember{ID: "b", NodeID: 2}, false))
	c.activate()
	c.sync()
	names, err := raft.ReadDeliveryGroups(node.db, "orders")
	require.NoError(t, err)
	require.Equal(t, []string{"g:a", "g:b"}, names)
	_, receiptNames, _ := node.sm.DeliveryRecipients("orders")
	require.Equal(t, []string{"g:a"}, receiptNames, "live membership excludes B")
	epoch, _ := node.sm.ConsumerVersion()
	receipt, err := raft.MarshalPublishReceipt(epoch, names)
	require.NoError(t, err)
	due := time.Now().Add(-time.Minute).UnixMilli()
	data, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("payload"), EnqueuedAtUnixMs: due})
	require.NoError(t, err)
	bucket := utils.CalculateBucket(due, time.Second)
	cmd, err := raft.MarshalStoreDeliveryBatchCmd([]raft.StoreBatchItem{{ID: 999, Bucket: bucket, TopicHash: utils.TopicHash("orders"), Msg: data, Receipt: receipt}})
	require.NoError(t, err)
	_, err = node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(t, err)
	c.sync()
	require.Equal(t, 1, node.dispatcher.dispatchAll())
	msg := receiveReplicaMessage(t, a)
	c.ack(1, "a", "a", msg)
	c.sync()
	c.stop(2)
	c.start(2, true)
	reader := NewDeliveryLedger(c.nodes[2].db, nil, nil, nil)
	c.nodes[2].dispatcher.PrepareBatch = reader.PublishedBatch
	b := c.subscribe(2, "b-new-session", "b")
	c.activate()
	state, err := raft.ReadDeliveryState(c.nodes[2].db, msg.DeliveryTag)
	require.NoError(t, err)
	require.True(t, state.PublishBound)
	require.True(t, state.Needs("g:b"))
	require.False(t, state.Needs("g:a"))
	require.Equal(t, 1, c.nodes[2].dispatcher.dispatchAll())
	require.Equal(t, msg.DeliveryTag, receiveReplicaMessage(t, b).DeliveryTag)
}

func TestPublishBoundProductionSchedulerDoesNotWrite(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, ledger := newPreloadTest(t, engine)
			ch := make(chan *pb.QueueMessage, 2)
			d.hub.Register("a", "orders", "a", ch)
			key := putPublishedEvent(t, d, 1, time.Now().Add(-time.Second).UnixMilli(), []string{"g:a", "g:offline"})
			observed := &ledgerCommitModeDB{DB: d.db}
			ledger.db = observed
			d.PrepareBatch = ledger.PublishedBatch
			d.ReconcileBatch = ledger.ReconcileBatch
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			select {
			case msg := <-ch:
				require.Equal(t, key, msg.DeliveryTag)
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("published message never reached sender queue")
			}
			cancel()
			<-done
			require.Empty(t, observed.modes, "normal delivery and named-group maintenance must not commit")
		})
	}
}

func TestPublishedReaderNeverInfersMissingDestinations(t *testing.T) {
	d, ledger := newPreloadTest(t, "pebble")
	key := putPipelineEvent(t, d, "orders", 1, time.Now().UnixMilli())
	observed := &ledgerCommitModeDB{DB: d.db}
	ledger.db = observed
	states, err := ledger.PublishedBatch(context.Background(), []raft.DeliveryPrepare{{Key: key, Epoch: 5, Recipients: []string{"g:new"}}})
	require.NoError(t, err)
	require.Nil(t, states[string(key)])
	require.Empty(t, observed.modes)
	state, err := raft.ReadDeliveryState(d.db, key)
	require.NoError(t, err)
	require.Nil(t, state)
}
