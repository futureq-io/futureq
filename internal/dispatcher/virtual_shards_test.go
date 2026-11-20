package dispatcher

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type countedShardDB struct {
	storage.DB
	steps, seeks int
}

type countedShardIterator struct {
	storage.Iterator
	db *countedShardDB
}

func (db *countedShardDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	iter, err := db.DB.NewIter(opts)
	if err != nil {
		return nil, err
	}
	return &countedShardIterator{Iterator: iter, db: db}, nil
}

func (i *countedShardIterator) Next() bool {
	i.db.steps++
	return i.Iterator.Next()
}

func (i *countedShardIterator) SeekGE(key []byte) bool {
	i.db.seeks++
	return i.Iterator.SeekGE(key)
}

func TestVirtualShardDiscoverySkipsRemoteKeys(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			var db storage.DB
			var err error
			if engine == "pebble" {
				db, err = storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
			} else {
				db, err = storage.NewBoltDB(config.Bolt{File: filepath.Join(t.TempDir(), "events.db")})
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			counted := &countedShardDB{DB: db}
			wake := make(chan struct{}, 1)
			hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wake)
			hub.SetLocalReplica(2)
			hub.SetGroupMembership(func(_, _ string) []metadata.ConsumerMember {
				return []metadata.ConsumerMember{{ID: "a", NodeID: 1}, {ID: "b", NodeID: 1}, {ID: "local", NodeID: 2}}
			})
			hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
			d := NewDispatcher(counted, hub, nil, time.Millisecond, time.Second, wake, zap.NewNop())
			d.scanOnly = true
			d.TimeBucket = time.Second
			d.Limits.Candidates, d.Limits.Examined = 1024, 1024
			d.Limits.WorkTime = time.Second
			d.PreloadMaxMessages, d.PreloadLookahead = 1024, time.Minute
			now := time.Now().Truncate(time.Second).UnixMilli()
			hash := utils.TopicHash("orders")
			batch := db.NewBatch()
			defer batch.Close() //nolint:errcheck
			var dueKeys, futureKeys []string
			id := uint64(0)
			for _, layout := range []struct{ count, firstLocal int }{{32, 21}, {64, 42}} {
				// Two workers on replica 1 and one on replica 2 yield the high
				// 11/22 shards on replica 2 for these two layouts.
				count, firstLocal := layout.count, layout.firstLocal
				for ordinal := 0; ordinal < count; ordinal++ {
					for message := 0; message < 20; message++ {
						at := now - 1000
						if message >= 8 {
							at = now + 1000
						}
						if message >= 12 {
							at = now + 120000
						}
						value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: at})
						require.NoError(t, err)
						id++
						packed := utils.ShardStorageBucket(count+ordinal, utils.CalculateBucket(at, d.TimeBucket))
						key := utils.EventKey(packed, hash, id)
						require.NoError(t, batch.Set(key, value))
						if ordinal >= firstLocal && message < 8 {
							dueKeys = append(dueKeys, string(key))
						} else if ordinal >= firstLocal && message < 12 {
							futureKeys = append(futureKeys, string(key))
						}
					}
				}
			}
			require.NoError(t, batch.Commit(storage.Sync))
			chunk := d.collectTopic("orders", now)
			actual := make([]string, 0, len(chunk.items))
			for _, item := range chunk.items {
				actual = append(actual, string(item.Key))
			}
			require.Len(t, actual, len(dueKeys))
			for i, key := range actual {
				require.Equal(t, dueKeys[i], key)
			}
			require.Less(t, counted.steps+counted.seeks, 96*8/2, "discovery should skip most of the 768 due keys")
			t.Logf("due discovery found %d keys with %d Next calls and %d seeks", len(actual), counted.steps, counted.seeks)
			counted.steps, counted.seeks = 0, 0
			preload := &topicPreload{}
			require.True(t, d.preloadTopic("orders", preload, hub.InterestView("orders"), now))
			actual = actual[:0]
			for _, item := range preload.items {
				actual = append(actual, string(item.key[:]))
			}
			require.ElementsMatch(t, futureKeys, actual)
			require.Less(t, counted.steps+counted.seeks, 96*4, "preload should skip remote and out-of-window keys")
		})
	}
}

func TestVirtualShardReplicaRebalancePreservesOldTagAndFanout(t *testing.T) {
	c := newReplicaTestCluster(t)
	for id, node := range c.nodes {
		node.hub.SetLocalReplica(id)
	}
	c.subscribe(1, "old", "workers")
	worker := c.subscribe(2, "remaining", "workers")
	universal := c.subscribe(3, "audit", "")
	c.activate()

	now := time.Now().Add(-time.Minute).UnixMilli()
	value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: now})
	require.NoError(t, err)
	hash := utils.TopicHash("orders")
	oldBucket := utils.ShardStorageBucket(2, utils.CalculateBucket(now, time.Second))
	newBucket := utils.ShardStorageBucket(7, utils.CalculateBucket(now, time.Second))
	cmd, err := raft.MarshalStoreBatchCmd([]raft.StoreBatchItem{
		{ID: 1, Bucket: oldBucket, TopicHash: hash, Msg: value},
		{ID: 2, Bucket: newBucket, TopicHash: hash, Msg: value},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = c.nodes[1].nh.SyncPropose(ctx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(t, err)
	c.sync()
	oldKey, newKey := utils.EventKey(oldBucket, hash, 1), utils.EventKey(newBucket, hash, 2)
	require.Eventually(t, func() bool { return c.nodes[2].dispatcher.dispatchAll() == 1 }, 5*time.Second, 20*time.Millisecond)
	newMessage := receiveReplicaMessage(t, worker)
	require.Equal(t, newKey, newMessage.DeliveryTag)
	c.ack(2, "remaining", "workers", newMessage)

	var audited [][]byte
	require.Eventually(t, func() bool {
		c.nodes[3].dispatcher.dispatchAll()
		for len(universal) > 0 {
			msg := <-universal
			audited = append(audited, msg.DeliveryTag)
			c.ack(3, "audit", "", msg)
		}
		return len(audited) >= 2
	}, 5*time.Second, 20*time.Millisecond)
	require.ElementsMatch(t, [][]byte{oldKey, newKey}, audited)

	c.nodes[1].hub.Unregister("old")
	require.NoError(t, c.nodes[1].svc.ChangeConsumer(ctx, replicaTestShard, "orders", "workers", metadata.ConsumerMember{ID: "old", NodeID: 1}, false))
	c.activate()
	require.Eventually(t, func() bool { return c.nodes[2].dispatcher.dispatchAll() == 1 }, 5*time.Second, 20*time.Millisecond)
	oldMessage := receiveReplicaMessage(t, worker)
	require.Equal(t, oldKey, oldMessage.DeliveryTag)
	c.ack(2, "remaining", "workers", oldMessage)
	c.sync()
	for _, node := range c.nodes {
		for _, key := range [][]byte{oldKey, newKey} {
			_, _, err := node.db.Get(key)
			require.ErrorIs(t, err, storage.ErrNotFound)
		}
	}
}

func TestVirtualShardDispatchUsesLocalRoundRobin(t *testing.T) {
	members := []metadata.ConsumerMember{{ID: "a", NodeID: 1}, {ID: "b", NodeID: 1}, {ID: "c", NodeID: 2}}
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	hub.SetLocalReplica(1)
	hub.SetGroupMembership(func(_, _ string) []metadata.ConsumerMember { return members })
	first, second, uncommitted := make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2)
	hub.Register("a", "orders", "workers", first)
	hub.Register("b", "orders", "workers", second)
	hub.Register("joining", "orders", "workers", uncommitted)
	for id, expected := range []chan *pb.QueueMessage{first, second, first, second} {
		// One shard, with IDs that always give the same legacy modulo owner.
		key := utils.EventKey(utils.ShardStorageBucket(64, 1), utils.TopicHash("orders"), uint64(3*id+1))
		msg := &pb.QueueMessage{Topic: "orders", DeliveryTag: key}
		require.Equal(t, []string{"workers"}, hub.DispatchToTopic("orders", msg, key))
		require.Equal(t, key, receiveReplicaMessage(t, expected).DeliveryTag)
	}
	remote := utils.EventKey(utils.ShardStorageBucket(127, 1), utils.TopicHash("orders"), 5)
	require.Empty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, remote))
	require.Empty(t, uncommitted)
}

func TestReplicaCountDistributionSkipsEmptyReplicas(t *testing.T) {
	members := []metadata.ConsumerMember{{ID: "a", NodeID: 1}, {ID: "b", NodeID: 1}, {ID: "c", NodeID: 3}}
	policy := metadata.ShardDistributionReplicaCount
	first, end := describeReplica(members, 1, policy).shardRange(64)
	require.Equal(t, [2]int{0, 32}, [2]int{first, end})
	first, end = describeReplica(members, 2, policy).shardRange(64)
	require.Equal(t, [2]int{0, 0}, [2]int{first, end})
	first, end = describeReplica(members, 3, policy).shardRange(64)
	require.Equal(t, [2]int{32, 64}, [2]int{first, end})
	// An unsharded key uses the same weights, with the ID selecting a replica.
	for id := uint64(0); id < 4; id++ {
		key := utils.EventKey(1, 1, id)
		require.Equal(t, id%2 == 0, describeReplica(members, 1, policy).owns(0, 0, key))
		require.False(t, describeReplica(members, 2, policy).owns(0, 0, key))
		require.Equal(t, id%2 == 1, describeReplica(members, 3, policy).owns(0, 0, key))
	}
}

func TestShardDistributionRebalancesQueuedDelivery(t *testing.T) {
	c := newReplicaTestCluster(t)
	c.nodes[1].hub.SetDeliveryTimeout(10 * time.Second)
	for id, node := range c.nodes {
		node.hub.SetLocalReplica(id)
		node.hub.SetShardDistribution(node.sm.ShardDistribution)
	}
	first := c.subscribe(1, "first", "workers")
	c.subscribe(1, "second", "workers")
	other := c.subscribe(2, "other", "workers")
	c.activate()
	at := time.Now().Add(-time.Minute).UnixMilli()
	data, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: at})
	require.NoError(t, err)
	// Shard 36 belongs to replica 1 at 42/22 and replica 2 at 32/32.
	bucket := utils.ShardStorageBucket(64+36, utils.CalculateBucket(at, time.Second))
	cmd, err := raft.MarshalStoreBatchCmd([]raft.StoreBatchItem{{ID: 1, Bucket: bucket, TopicHash: utils.TopicHash("orders"), Msg: data}})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = c.nodes[1].nh.SyncPropose(ctx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(t, err)
	c.sync()
	require.Eventually(t, func() bool { return c.nodes[1].dispatcher.dispatchAll() == 1 }, 5*time.Second, 20*time.Millisecond)
	previous := receiveReplicaMessage(t, first)
	require.Zero(t, c.nodes[2].dispatcher.dispatchAll())

	for _, node := range c.nodes {
		node.coordinator.ShardDistribution = metadata.ShardDistributionReplicaCount
	}
	require.Eventually(t, func() bool {
		for _, node := range c.nodes {
			if err := node.coordinator.Tick(ctx, time.Now()); err != nil {
				return false
			}
		}
		_, pending, _ := c.nodes[2].sm.ConsumerStatus(2)
		return pending && c.nodes[2].sm.ShardDistribution() == metadata.ShardDistributionReplicaCount
	}, 5*time.Second, 20*time.Millisecond)
	require.Zero(t, c.nodes[2].dispatcher.dispatchAll(), "new owner must wait for the old assignment to drain")
	c.sync()
	require.False(t, c.nodes[1].hub.CanSend("first", previous))
	require.True(t, c.nodes[1].hub.RemoveInFlightForConsumer("first", previous.DeliveryTag))
	c.activate()
	require.Eventually(t, func() bool { return c.nodes[2].dispatcher.dispatchAll() == 1 }, 5*time.Second, 20*time.Millisecond)
	reassigned := receiveReplicaMessage(t, other)
	require.Equal(t, previous.DeliveryTag, reassigned.DeliveryTag)
	c.ack(2, "other", "workers", reassigned)
	c.sync()
	for _, node := range c.nodes {
		_, _, err := node.db.Get(previous.DeliveryTag)
		require.ErrorIs(t, err, storage.ErrNotFound)
	}
}
