package dispatcher

import (
	"context"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPreloadRaftReplicasPreserveOwnershipAndFanout(t *testing.T) {
	c := newReplicaTestCluster(t)
	first := c.subscribe(1, "first", "workers")
	second := c.subscribe(2, "second", "workers")
	universal := c.subscribe(3, "universal", "")
	c.activate()
	due := time.Now().Add(2 * time.Second).UnixMilli()
	value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("future"), EnqueuedAtUnixMs: due - 1000, DelayMs: 1000})
	require.NoError(t, err)
	bucket, hash := utils.CalculateBucket(due, time.Second), utils.TopicHash("orders")
	cmd, err := raft.MarshalStoreBatchCmd([]raft.StoreBatchItem{
		{ID: 10, Bucket: bucket, TopicHash: hash, Msg: value},
		{ID: 11, Bucket: bucket, TopicHash: hash, Msg: value},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = c.nodes[1].nh.SyncPropose(ctx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(t, err)
	c.sync()
	for id := uint64(1); id <= 3; id++ {
		d := c.nodes[id].dispatcher
		d.PreloadLookahead, d.PreloadMaxMessages = 10*time.Second, 4
		require.Zero(t, d.dispatchAll(), "future messages must not prepare or deliver")
		require.NotZero(t, d.preloadCount, "each replica stages only its local interests")
		for eventID := uint64(10); eventID <= 11; eventID++ {
			state, err := raft.ReadDeliveryState(d.db, utils.EventKey(bucket, hash, eventID))
			require.NoError(t, err)
			require.Nil(t, state, "preload does not propose an early manifest")
		}
	}
	require.Empty(t, first)
	require.Empty(t, second)
	require.Empty(t, universal)
	time.Sleep(time.Until(time.UnixMilli(due)))
	for id := uint64(1); id <= 3; id++ {
		c.nodes[id].dispatcher.dispatchAll()
	}
	require.Len(t, first, 1)
	require.Len(t, second, 1)
	require.Len(t, universal, 2)
	firstMessage, secondMessage := <-first, <-second
	require.NotEqual(t, firstMessage.DeliveryTag, secondMessage.DeliveryTag, "group ownership must remain disjoint")
	universalKeys := [][]byte{(<-universal).DeliveryTag, (<-universal).DeliveryTag}
	require.ElementsMatch(t, [][]byte{firstMessage.DeliveryTag, secondMessage.DeliveryTag}, universalKeys)
}
