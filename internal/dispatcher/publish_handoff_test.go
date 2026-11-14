package dispatcher

import (
	"context"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPublishHandoffPreservesReservedMessagesAndCachedPayload(t *testing.T) {
	for _, mode := range []string{"quorum", "early-ack"} {
		t.Run(mode, func(t *testing.T) {
			c := newReplicaTestCluster(t)
			ch := c.subscribe(1, "worker", "s:workers")
			c.activate()
			submit := func(nodeID uint64, command []byte) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				node := c.nodes[nodeID]
				_, err := node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), command)
				require.NoError(t, err)
				c.sync()
			}
			if mode == "quorum" {
				command, err := raft.MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{
					Operation: raft.SubscriptionCreate, NowMs: time.Now().UnixMilli(),
					Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", GroupId: "workers", Recipient: "g:s:workers"}},
				})
				require.NoError(t, err)
				submit(1, command)
			}
			oldLeader, _, valid, err := c.nodes[1].nh.GetLeaderID(replicaTestShard)
			require.NoError(t, err)
			require.True(t, valid)
			// This is the same allocator the publisher and state machine share.
			// Reserve before the handoff, but deliberately delay submission.
			oldReservation := c.nodes[oldLeader].repo.NextID()
			newLeader := oldLeader%3 + 1
			require.NoError(t, c.nodes[oldLeader].nh.RequestLeaderTransfer(replicaTestShard, newLeader))
			require.Eventually(t, func() bool {
				for _, node := range c.nodes {
					leader, _, valid, err := node.nh.GetLeaderID(replicaTestShard)
					if err != nil || !valid || leader != newLeader {
						return false
					}
				}
				return true
			}, 5*time.Second, 10*time.Millisecond)
			c.sync()
			newReservation := c.nodes[newLeader].repo.NextID()
			require.Equal(t, oldReservation, newReservation, "uncommitted reservations are local, so they can collide")
			due := time.Now().Add(-10 * time.Second).UnixMilli()
			bucket := utils.CalculateBucket(due, time.Second)
			epoch, names, active := c.nodes[1].sm.DeliveryRecipients("orders")
			require.True(t, active)
			receipt, err := raft.MarshalPublishReceipt(epoch, names)
			require.NoError(t, err)
			publish := func(nodeID, id uint64, payload string) {
				value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte(payload), EnqueuedAtUnixMs: due})
				require.NoError(t, err)
				items := []raft.StoreBatchItem{{ID: id, Bucket: bucket, TopicHash: utils.TopicHash("orders"), Msg: value}}
				var command []byte
				if mode == "quorum" {
					command, err = raft.MarshalPublishBatchCmd(items)
				} else {
					items[0].Receipt = receipt
					command, err = raft.MarshalStoreDeliveryBatchCmd(items)
				}
				require.NoError(t, err)
				submit(nodeID, command)
			}
			publish(newLeader, newReservation, "new-leader-message")
			d := c.nodes[1].dispatcher
			reader := NewDeliveryLedger(d.db, nil, nil, nil)
			d.PrepareBatch = reader.PublishedBatch
			require.True(t, d.refreshPermit(context.Background()))
			chunk := d.collectScheduledTopic("orders", time.Now().UnixMilli())
			require.Len(t, chunk.items, 1)
			key := utils.EventKey(bucket, utils.TopicHash("orders"), newReservation)
			require.Equal(t, "new-leader-message", string(chunk.payloads[string(key)].message.Payload))
			// Dragonboat forwards this proposal from the former leader. Ordered
			// apply must assign a fresh ID rather than overwrite the cached key.
			publish(oldLeader, oldReservation, "delayed-old-leader-message")
			for _, node := range c.nodes {
				for id, want := range map[uint64]string{newReservation: "new-leader-message", newReservation + 1: "delayed-old-leader-message"} {
					value, closer, err := node.db.Get(utils.EventKey(bucket, utils.TopicHash("orders"), id))
					require.NoError(t, err)
					var stored storagepb.StoredMessage
					require.NoError(t, proto.Unmarshal(value, &stored))
					require.NoError(t, closer.Close())
					require.Equal(t, want, string(stored.Payload), "all replicas must preserve both messages")
				}
			}
			require.True(t, d.refreshPermit(context.Background()))
			states, err := d.prepareChunk(context.Background(), chunk)
			require.NoError(t, err)
			require.Equal(t, 1, d.deliverChunk(chunk, states))
			first := receiveReplicaMessage(t, ch)
			require.Equal(t, "new-leader-message", string(first.Payload))
			require.Equal(t, 1, d.dispatchAll())
			second := receiveReplicaMessage(t, ch)
			require.Equal(t, "delayed-old-leader-message", string(second.Payload))
			require.NotEqual(t, first.DeliveryTag, second.DeliveryTag)
		})
	}
}
