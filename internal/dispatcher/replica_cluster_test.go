package dispatcher

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/lni/dragonboat/v4"
	raftconfig "github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const replicaTestShard = uint64(1)
const replicaTestTimeout = 150 * time.Millisecond

type replicaTestNode struct {
	id          uint64
	nh          *dragonboat.NodeHost
	db          storage.DB
	sm          *metadata.MetadataStateMachine
	svc         *metadata.Service
	hub         *Hub
	dispatcher  *Dispatcher
	deleter     *Deleter
	coordinator *ConsumerCoordinator
}

type replicaTestCluster struct {
	t       *testing.T
	root    string
	members map[uint64]string
	nodes   map[uint64]*replicaTestNode
}

func newReplicaTestCluster(t *testing.T) *replicaTestCluster {
	t.Helper()
	c := &replicaTestCluster{t: t, root: t.TempDir(), members: make(map[uint64]string), nodes: make(map[uint64]*replicaTestNode)}
	for id := uint64(1); id <= 3; id++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		c.members[id] = listener.Addr().String()
		require.NoError(t, listener.Close())
	}
	// dispatchTopic uses the configured time bucket. NodeHost/read barriers are
	// injected per dispatcher; this singleton remains standalone during the test.
	previous := app.A
	application, err := app.Init(&config.Config{
		Storage:  config.Storage{Engine: "pebble", Pebble: config.Pebble{Mode: "memory"}},
		Delivery: config.Delivery{TimeBucket: time.Second},
	}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, node := range c.nodes {
			c.stop(node.id)
		}
		require.NoError(t, application.DB.Close())
		app.A = previous
	})
	for id := uint64(1); id <= 3; id++ {
		c.start(id, false)
	}
	c.sync()
	return c
}

func (c *replicaTestCluster) start(id uint64, restart bool) *replicaTestNode {
	c.t.Helper()
	root := filepath.Join(c.root, c.members[id])
	db, err := storage.NewPebble(config.Pebble{Mode: "disk", DataDir: filepath.Join(root, "events"), WALEnabled: true, CacheSize: "1MiB", MemtableSize: "1MiB"}, zap.NewNop())
	require.NoError(c.t, err)
	nhc := raftconfig.NodeHostConfig{NodeHostDir: filepath.Join(root, "raft"), RTTMillisecond: 10, RaftAddress: c.members[id]}
	nhc.Expert.LogDB = raftconfig.GetTinyMemLogDBConfig()
	nhc.Expert.LogDB.Shards = 1
	nhc.Expert.Engine = raftconfig.EngineConfig{ExecShards: 1, CommitShards: 1, ApplyShards: 1, SnapshotShards: 1, CloseShards: 1}
	nh, err := dragonboat.NewNodeHost(nhc)
	require.NoError(c.t, err)
	node := &replicaTestNode{id: id, nh: nh, db: db}
	c.nodes[id] = node
	node.sm = metadata.NewMetadataStateMachineFactory(zap.NewNop())(metadata.MetadataShardID, id).(*metadata.MetadataStateMachine)
	propose := func(shard uint64, cmd []byte) (statemachine.Result, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return nh.SyncPropose(ctx, nh.GetNoOPSession(shard), cmd)
	}
	node.svc = metadata.NewService(nh, func(ctx context.Context, cmd []byte) error {
		_, err := nh.SyncPropose(ctx, nh.GetNoOPSession(metadata.MetadataShardID), cmd)
		return err
	}, zap.NewNop())
	wake := make(chan struct{}, 1)
	node.hub = NewHub(NewRoundRobinStrategy(), zap.NewNop(), wake)
	node.hub.SetGroupMembership(node.sm.ConsumerGroup)
	node.hub.SetDeliveryFence(node.sm.ConsumerVersion, replicaTestTimeout)
	node.sm.SetConsumerBarrier(node.hub.WithRebalanceBarrier)
	backend := NewRaftDeleteBackend(func(cmd []byte) error { _, err := propose(replicaTestShard, cmd); return err }, zap.NewNop())
	ledger := NewDeliveryLedger(db, node.sm.DeliveryRecipients, backend, func(cmd []byte) (statemachine.Result, error) { return propose(replicaTestShard, cmd) })
	node.deleter = NewDeleter(ledger, time.Hour, zap.NewNop())
	node.deleter.AcknowledgeBatch = ledger.Acknowledge
	node.dispatcher = NewDispatcher(db, node.hub, node.deleter, time.Millisecond, replicaTestTimeout, wake, zap.NewNop())
	node.dispatcher.PrepareBatch = ledger.PrepareBatch
	node.dispatcher.MaintenanceRecipients = node.sm.DeliveryRecipients
	node.hub.SetDeliveryView(node.sm.TopicDeliverySnapshot)
	node.dispatcher.ReadBarrier = NewConsumerReadBarrier(context.Background(), nh, node.sm, node.hub, replicaTestShard)
	node.hub.OnNack = node.dispatcher.RemoveInFlight
	node.coordinator = NewConsumerCoordinator(node.hub, node.sm, node.svc, replicaTestShard, id, replicaTestTimeout, zap.NewNop())
	rc := raftconfig.Config{ShardID: metadata.MetadataShardID, ReplicaID: id, ElectionRTT: 10, HeartbeatRTT: 1, CheckQuorum: true, SnapshotEntries: 20, CompactionOverhead: 5}
	members := c.members
	if restart {
		members = nil
	}
	require.NoError(c.t, nh.StartReplica(members, false, func(uint64, uint64) statemachine.IStateMachine { return node.sm }, rc))
	repo, err := repository.NewEventRepository(db, zap.NewNop(), time.Second)
	require.NoError(c.t, err)
	rc.ShardID = replicaTestShard
	require.NoError(c.t, nh.StartOnDiskReplica(members, false, raft.NewEventStateMachineFactory(db, repo, func(keys [][]byte) {
		node.dispatcher.RemoveInFlightBatch(keys)
		node.hub.RemoveDeletedBatch(keys)
	}, zap.NewNop()), rc))
	return node
}

func (c *replicaTestCluster) stop(id uint64) {
	if node := c.nodes[id]; node != nil && node.nh != nil {
		node.nh.Close()
		require.NoError(c.t, node.db.Close())
		node.nh = nil
	}
}

func (c *replicaTestCluster) sync() {
	c.t.Helper()
	require.Eventually(c.t, func() bool {
		for _, node := range c.nodes {
			if node.nh == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			err := node.svc.SyncConsumers(ctx)
			if err == nil {
				_, err = node.nh.SyncRead(ctx, replicaTestShard, nil)
			}
			cancel()
			if err != nil {
				return false
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond)
}

func (c *replicaTestCluster) subscribe(nodeID uint64, id, group string) chan *pb.QueueMessage {
	c.t.Helper()
	node := c.nodes[nodeID]
	ch := make(chan *pb.QueueMessage, 16)
	require.NoError(c.t, node.hub.WithConsumerRegistry(func() error {
		node.hub.Register(id, "orders", group, ch)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return node.svc.ChangeConsumer(ctx, replicaTestShard, "orders", group, metadata.ConsumerMember{ID: id, NodeID: nodeID}, true)
	}))
	return ch
}

func (c *replicaTestCluster) activate() {
	c.t.Helper()
	require.Eventually(c.t, func() bool {
		active := true
		for _, node := range c.nodes {
			if node.nh == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := node.coordinator.Tick(ctx, time.Now())
			cancel()
			if err != nil {
				return false
			}
			_, ready := node.sm.ConsumerVersion()
			active = active && ready
		}
		return active
	}, 5*time.Second, 20*time.Millisecond)
	c.sync()
}

func (c *replicaTestCluster) publish(id uint64) []byte {
	c.t.Helper()
	msg := &storagepb.StoredMessage{Topic: "orders", Payload: []byte("payload"), EnqueuedAtUnixMs: time.Now().Add(-time.Minute).UnixMilli()}
	data, err := proto.Marshal(msg)
	require.NoError(c.t, err)
	bucket := utils.CalculateBucket(msg.EnqueuedAtUnixMs, time.Second)
	cmd, err := raft.MarshalStoreBatchCmd([]raft.StoreBatchItem{{ID: id, Bucket: bucket, TopicHash: utils.TopicHash("orders"), Msg: data}})
	require.NoError(c.t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = c.nodes[1].nh.SyncPropose(ctx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(c.t, err)
	c.sync()
	return utils.EventKey(bucket, utils.TopicHash("orders"), id)
}

func receiveReplicaMessage(t *testing.T, ch <-chan *pb.QueueMessage) *pb.QueueMessage {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(time.Second):
		t.Fatal("expected replica delivery")
		return nil
	}
}

func (c *replicaTestCluster) ack(nodeID uint64, id, group string, msg *pb.QueueMessage) {
	c.t.Helper()
	node := c.nodes[nodeID]
	require.True(c.t, node.hub.RemoveInFlightForConsumer(id, msg.DeliveryTag))
	node.deleter.MarkAcknowledged(msg.DeliveryTag, metadata.DeliveryRecipient(group, id))
	node.deleter.flush()
}

func TestReplicaClusterEarlyAckPreservesIndependentDeliveries(t *testing.T) {
	c := newReplicaTestCluster(t)
	a := c.subscribe(1, "a", "group-a")
	b := c.subscribe(2, "b", "group-b")
	u1 := c.subscribe(1, "u1", "")
	u2 := c.subscribe(2, "u2", "")
	c.activate()
	key := c.publish(1)
	require.Equal(t, 1, c.nodes[1].dispatcher.dispatchAll())
	c.ack(1, "a", "group-a", receiveReplicaMessage(t, a))
	c.ack(1, "u1", "", receiveReplicaMessage(t, u1))
	c.sync()
	state, err := raft.ReadDeliveryState(c.nodes[2].db, key)
	require.NoError(t, err)
	require.False(t, state.Needs("g:group-a"))
	require.True(t, state.Needs("g:group-b"))
	require.True(t, state.Needs("u:u2"))
	// Node 2 has not performed any dispatch before the ACKs above commit.
	require.Equal(t, 1, c.nodes[2].dispatcher.dispatchAll())
	c.ack(2, "b", "group-b", receiveReplicaMessage(t, b))
	c.ack(2, "u2", "", receiveReplicaMessage(t, u2))
	c.sync()
	for _, node := range c.nodes {
		_, _, err := node.db.Get(key)
		require.ErrorIs(t, err, storage.ErrNotFound)
		state, err := raft.ReadDeliveryState(node.db, key)
		require.NoError(t, err)
		require.Nil(t, state, "receipts are deleted atomically with the payload")
	}
}

func TestReplicaClusterTimeoutOfflineRebalanceAndRestart(t *testing.T) {
	c := newReplicaTestCluster(t)
	// A dispatch pass is bounded and may return zero during transient Raft
	// delays. Retry like the broker loop, retaining the exact delivery count.
	dispatch := func(nodeID uint64, expected int) {
		t.Helper()
		delivered := 0
		require.Eventually(t, func() bool {
			delivered += c.nodes[nodeID].dispatcher.dispatchAll()
			return delivered >= expected
		}, 5*time.Second, 20*time.Millisecond, "node %d did not deliver %d events", nodeID, expected)
		require.Equal(t, expected, delivered)
	}
	old := c.subscribe(1, "old", "workers")
	remote := c.subscribe(3, "remote", "workers")
	c.activate()
	c.publish(1) // remainder 1 belongs to node 3
	c.publish(2) // remainder 0 belongs to node 1
	dispatch(3, 1)
	staleHub := c.nodes[3].hub
	staleMessage := receiveReplicaMessage(t, remote)
	dispatch(1, 1)
	firstAttempt := receiveReplicaMessage(t, old) // remains unacknowledged
	c.stop(3)
	joined := c.subscribe(2, "joined", "workers")
	c.publish(3)
	require.Zero(t, c.nodes[2].dispatcher.dispatchAll(), "pending assignment must pause delivery")
	started := time.Now()
	c.activate() // node 1 times out its delivery; nodes 1/2 fence offline node 3
	require.Less(t, time.Since(started), 4*time.Second)
	require.Equal(t, []metadata.ConsumerMember{{ID: "old", NodeID: 1}, {ID: "joined", NodeID: 2}}, c.nodes[1].sm.ConsumerGroup("orders", "workers"))
	require.False(t, c.nodes[1].hub.CanSend("old", firstAttempt))
	require.False(t, staleHub.CanSend("remote", staleMessage))
	require.Empty(t, staleHub.DispatchToTopic("orders", staleMessage, staleMessage.DeliveryTag), "stale replica permit has expired")

	dispatch(2, 2) // IDs 1 and 3 belong to joined
	first := receiveReplicaMessage(t, joined)
	second := receiveReplicaMessage(t, joined)
	// NACK during the new assignment requeues only the incomplete interest.
	require.True(t, c.nodes[2].hub.RemoveInFlightForConsumer("joined", first.DeliveryTag))
	c.nodes[2].hub.OnNack(first.DeliveryTag)
	dispatch(2, 1)
	redelivery := receiveReplicaMessage(t, joined)
	require.Equal(t, first.DeliveryTag, redelivery.DeliveryTag)
	c.ack(2, "joined", "workers", redelivery)
	c.ack(2, "joined", "workers", second)
	dispatch(1, 1) // timed-out ID 2 retries
	c.ack(1, "old", "workers", receiveReplicaMessage(t, old))

	// Disconnect starts another barrier while the replica is still offline.
	c.nodes[2].hub.Unregister("joined")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, c.nodes[2].svc.ChangeConsumer(ctx, replicaTestShard, "orders", "workers", metadata.ConsumerMember{ID: "joined", NodeID: 2}, false))
	cancel()
	c.activate()
	c.publish(4)
	dispatch(1, 1)
	c.ack(1, "old", "workers", receiveReplicaMessage(t, old))

	// Reopen the same Raft and Pebble directories. It catches up before serving,
	// and a fresh subscription participates in the same registration barrier.
	c.start(3, true)
	c.sync()
	reconnected := c.subscribe(3, "reconnected", "workers")
	c.activate()
	c.publish(5)
	dispatch(3, 1)
	c.ack(3, "reconnected", "workers", receiveReplicaMessage(t, reconnected))
	c.sync()
	require.False(t, c.nodes[3].sm.ConsumerPresent("orders", "workers", "remote"))
	for _, node := range c.nodes {
		require.Empty(t, node.sm.ConsumerGroup("orders", "missing"))
		require.Zero(t, node.hub.GroupInFlightCount())
	}
}
