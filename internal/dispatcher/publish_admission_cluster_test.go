package dispatcher

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/internal/subscriptions"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type ClusterRetrySuite struct{ suite.Suite }

func TestClusterRetrySuite(t *testing.T) {
	suite.Run(t, new(ClusterRetrySuite))
}

func (s *ClusterRetrySuite) TestNewLeaderDeadLettersAnAbandonedFinalAttempt() {
	c := newReplicaTestCluster(s.T())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	waitLeader := func() *replicaTestNode {
		var leader *replicaTestNode
		s.Require().Eventually(func() bool {
			for _, node := range c.nodes {
				if node.nh != nil {
					id, _, valid, err := node.nh.GetLeaderID(replicaTestShard)
					if err == nil && valid && id == node.id {
						leader = node
						return true
					}
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond)
		return leader
	}
	proposeOn := func(node *replicaTestNode) func([]byte) (statemachine.Result, error) {
		return func(cmd []byte) (statemachine.Result, error) {
			return node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), cmd)
		}
	}
	leader := waitLeader()
	service := subscriptions.New(leader.db, 0, zap.NewNop())
	service.Propose = func(_ context.Context, cmd []byte) (statemachine.Result, error) {
		return proposeOn(leader)(cmd)
	}
	s.Require().NoError(service.CreateTopic(ctx, "orders", nil))
	subscription, err := service.Create(ctx, "orders", "workers", "")
	s.Require().NoError(err)
	now := time.Now()
	payload, err := proto.Marshal(&storagepb.StoredMessage{
		Topic: "orders", Payload: []byte("abandoned"), EnqueuedAtUnixMs: now.UnixMilli(), MaxRetries: new(uint32(0)),
	})
	s.Require().NoError(err)
	bucket, topic, id := utils.CalculateBucket(now.UnixMilli(), time.Second), utils.TopicHash("orders"), uint64(123)
	cmd, err := raft.MarshalPublishBatchCmd([]raft.StoreBatchItem{{ID: id, Bucket: bucket, TopicHash: topic, Msg: payload}})
	s.Require().NoError(err)
	result, err := proposeOn(leader)(cmd)
	s.Require().NoError(err)
	s.Require().NoError(raft.CheckPublishResult(result, 1))

	ledger := NewDeliveryLedger(leader.db, nil, nil, proposeOn(leader))
	attempt, err := ledger.ApplyRetry(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_START, NowMs: now.UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: utils.EventKey(bucket, topic, id), Recipient: subscription.Recipient,
			Token: bytes.Repeat([]byte{23}, raft.AttemptTokenSize), DeadlineMs: now.Add(200 * time.Millisecond).UnixMilli(),
		}},
	})
	s.Require().NoError(err)
	s.Require().Equal(uint64(1), attempt)
	c.stop(leader.id)
	successor := waitLeader()
	s.Require().NotEqual(leader.id, successor.id)
	successorLedger := NewDeliveryLedger(successor.db, nil, nil, proposeOn(successor))
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		successorLedger.RunRetryExpiry(workerCtx, time.Millisecond, nil, zap.NewNop())
	}()
	defer func() { stopWorker(); <-workerDone }()

	query := raft.DeadLetterQuery{Topic: "orders", Limit: 10}
	s.Require().Eventually(func() bool {
		value, err := successor.nh.SyncRead(ctx, replicaTestShard, query)
		return err == nil && len(value.(raft.DeadLetterResult).Page.Entries) == 1
	}, 5*time.Second, 20*time.Millisecond)
	c.start(leader.id, true)
	c.sync()
	for _, replica := range c.nodes {
		value, err := replica.nh.SyncRead(ctx, replicaTestShard, query)
		s.Require().NoError(err)
		page := value.(raft.DeadLetterResult).Page
		s.Require().Len(page.Entries, 1)
		s.Equal(pb.DeadLetterReason_DEAD_LETTER_REASON_TIMEOUT, page.Entries[0].Reason)
		s.Equal(subscription.Recipient, page.Entries[0].Generation)
	}
}

func TestQuorumAdmissionOfflineRecipientSurvivesReplicaRestart(t *testing.T) {
	c := newReplicaTestCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	node := c.nodes[1]
	service := subscriptions.New(node.db, 0, zap.NewNop())
	service.Propose = func(ctx context.Context, cmd []byte) (statemachine.Result, error) {
		return node.nh.SyncPropose(ctx, node.nh.GetNoOPSession(replicaTestShard), cmd)
	}
	require.NoError(t, service.CreateTopic(ctx, "orders", nil))
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
