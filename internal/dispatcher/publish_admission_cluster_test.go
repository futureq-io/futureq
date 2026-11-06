package dispatcher

import (
	"context"
	"strings"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/internal/subscriptions"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func TestQuorumAdmissionOfflineRecipientSurvivesReplicaRestart(t *testing.T) {
	c := newReplicaTestCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	node := c.nodes[1]
	service := subscriptions.New(node.db, 0, zap.NewNop())
	service.Propose = func(ctx context.Context, cmd []byte) (statemachine.Result, error) {
		return node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), cmd)
	}
	group, err := service.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	universal, err := service.Create(ctx, "orders", "", "audit")
	require.NoError(t, err)
	groupID := strings.TrimPrefix(group.Recipient, "g:")
	worker := c.subscribe(1, "worker", groupID)
	c.activate()
	c.sync()
	due := time.Now().Add(-time.Minute).UnixMilli()
	data, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("offline"), EnqueuedAtUnixMs: due})
	require.NoError(t, err)
	cmd, err := raft.MarshalPublishBatchCmd([]raft.StoreBatchItem{{ID: 708, Bucket: utils.CalculateBucket(due, time.Second), TopicHash: utils.TopicHash("orders"), Msg: data}})
	require.NoError(t, err)
	result, err := node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(t, err)
	require.NoError(t, raft.CheckPublishResult(result, 1))
	for _, replica := range c.nodes {
		ledger := NewDeliveryLedger(replica.db, nil, nil, nil)
		replica.dispatcher.PrepareBatch = ledger.PublishedBatch
	}
	c.sync()
	require.Equal(t, 1, node.dispatcher.dispatchAll())
	message := receiveReplicaMessage(t, worker)
	c.ack(1, "worker", groupID, message)
	c.sync()
	// The receipt chosen during apply retains the never-connected subscriber.
	c.stop(2)
	c.start(2, true)
	ledger := NewDeliveryLedger(c.nodes[2].db, nil, nil, nil)
	c.nodes[2].dispatcher.PrepareBatch = ledger.PublishedBatch
	auditID := strings.TrimPrefix(universal.Recipient, "g:")
	audit := c.subscribe(2, "audit-session", auditID)
	c.activate()
	c.sync()
	require.Equal(t, 1, c.nodes[2].dispatcher.dispatchAll())
	require.Equal(t, message.DeliveryTag, receiveReplicaMessage(t, audit).DeliveryTag)
	c.ack(2, "audit-session", auditID, message)
	c.sync()
	for _, replica := range c.nodes {
		_, _, err := replica.db.Get(message.DeliveryTag)
		require.ErrorIs(t, err, storage.ErrNotFound)
	}
}
