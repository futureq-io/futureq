package dispatcher

import (
	"context"
	"strings"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/internal/subscriptions"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func TestRaftDurableUniversalSubscriptionRestartAndRemoval(t *testing.T) {
	c := newReplicaTestCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serviceFor := func(node *replicaTestNode) *subscriptions.Service {
		service := subscriptions.New(node.db, 0, zap.NewNop())
		service.Propose = func(ctx context.Context, cmd []byte) (statemachine.Result, error) {
			return node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), cmd)
		}
		return service
	}
	service := serviceFor(c.nodes[1])
	group, err := service.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	universal, err := service.Create(ctx, "orders", "", "audit")
	require.NoError(t, err)
	groupID := strings.TrimPrefix(group.Recipient, "g:")
	universalID := strings.TrimPrefix(universal.Recipient, "g:")
	a := c.subscribe(1, "worker", groupID)
	c.activate()
	c.sync()
	names, err := raft.ReadSubscriptionRecipients(c.nodes[1].db, "orders")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{group.Recipient, universal.Recipient}, names)
	receipt, err := raft.MarshalPublishReceipt(0, names)
	require.NoError(t, err)
	due := time.Now().Add(-time.Minute).UnixMilli()
	data, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("offline"), EnqueuedAtUnixMs: due})
	require.NoError(t, err)
	bucket := utils.CalculateBucket(due, time.Second)
	cmd, err := raft.MarshalStoreDeliveryBatchCmd([]raft.StoreBatchItem{{ID: 707, Bucket: bucket, TopicHash: utils.TopicHash("orders"), Msg: data, Receipt: receipt}})
	require.NoError(t, err)
	_, err = c.nodes[1].nh.SyncPropose(ctx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(t, err)
	for _, node := range c.nodes {
		reader := NewDeliveryLedger(node.db, nil, nil, nil)
		node.dispatcher.PrepareBatch = reader.PublishedBatch
	}
	c.sync()
	require.Equal(t, 1, c.nodes[1].dispatcher.dispatchAll())
	message := receiveReplicaMessage(t, a)
	c.ack(1, "worker", groupID, message)
	c.sync()
	// Universal interest has never had a live connection and must survive restart.
	c.stop(2)
	c.start(2, true)
	reader := NewDeliveryLedger(c.nodes[2].db, nil, nil, nil)
	c.nodes[2].dispatcher.PrepareBatch = reader.PublishedBatch
	audit := c.subscribe(2, "audit-session", universalID)
	c.activate()
	c.sync()
	require.Equal(t, 1, c.nodes[2].dispatcher.dispatchAll())
	require.Equal(t, message.DeliveryTag, receiveReplicaMessage(t, audit).DeliveryTag)
	require.NoError(t, service.Unsubscribe(ctx, "orders", "", "audit"))
	require.NoError(t, service.Maintain(ctx, time.Now()))
	c.sync()
	for _, node := range c.nodes {
		_, _, err := node.db.Get(message.DeliveryTag)
		require.ErrorIs(t, err, storage.ErrNotFound)
	}
	recreated, err := service.Create(ctx, "orders", "", "audit")
	require.NoError(t, err)
	require.NotEqual(t, universal.Recipient, recreated.Recipient)
	// The assignment can still contain a draining old worker; it cannot revive
	// durable interest or attach the deleted generation to new publications.
	_, live, _ := c.nodes[2].sm.DeliveryRecipients("orders")
	require.Contains(t, live, metadata.DeliveryRecipient(universalID, ""))
	c.sync()
	names, err = raft.ReadSubscriptionRecipients(c.nodes[2].db, "orders")
	require.NoError(t, err)
	require.NotContains(t, names, universal.Recipient)
	require.Contains(t, names, recreated.Recipient)
}
