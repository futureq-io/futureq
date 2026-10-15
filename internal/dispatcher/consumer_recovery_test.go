package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestConsumerReconciliationRepairsUncertainRegistrationAndRemoval(t *testing.T) {
	c := newReplicaTestCluster(t)
	c.activate() // every coordinator reconciles an empty registry successfully
	n := c.nodes[1]
	change := func(svc *metadata.Service, id string, add bool) error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return svc.ChangeConsumer(ctx, replicaTestShard, "orders", "workers", metadata.ConsumerMember{ID: id, NodeID: 1}, add)
	}
	lostResponse := metadata.NewService(n.nh, func(ctx context.Context, cmd []byte) error {
		_, err := n.nh.SyncPropose(ctx, n.nh.GetNoOPSession(metadata.MetadataShardID), cmd)
		if err != nil {
			return err
		}
		return context.DeadlineExceeded // committed add, response lost to the subscriber
	}, zap.NewNop())
	failedRemoval := metadata.NewService(n.nh, func(context.Context, []byte) error {
		return context.DeadlineExceeded // removal never commits
	}, zap.NewNop())
	for _, id := range []string{"uncertain-add", "uncertain-remove"} {
		n.hub.Register(id, "orders", "workers", make(chan *pb.QueueMessage, 1))
		if id == "uncertain-add" {
			require.ErrorIs(t, change(lostResponse, id, true), context.DeadlineExceeded)
		} else {
			require.NoError(t, change(n.svc, id, true))
			require.ErrorIs(t, change(failedRemoval, id, false), context.DeadlineExceeded)
		}
		n.hub.Unregister(id)
	}
	c.sync()
	require.Empty(t, n.hub.LocalConsumerIDs())
	require.True(t, n.sm.ConsumerPresent("orders", "workers", "uncertain-add"))
	require.True(t, n.sm.ConsumerPresent("orders", "workers", "uncertain-remove"))
	// Force the normal reconciliation interval due without sleeping. The local
	// registry is still exactly the successful empty snapshot from c.activate.
	n.coordinator.lastReconcile = time.Now().Add(-6 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, n.coordinator.Tick(ctx, time.Now()))
	cancel()
	c.activate()
	require.False(t, n.sm.ConsumerPresent("orders", "workers", "uncertain-add"))
	require.False(t, n.sm.ConsumerPresent("orders", "workers", "uncertain-remove"))
}

func TestConsumerCrashReassignsWithoutSubscriptionOrRaftMembershipChanges(t *testing.T) {
	c := newReplicaTestCluster(t)
	local := c.subscribe(1, "local", "workers")
	c.subscribe(3, "crashed", "workers")
	c.activate()
	key := c.publish(1) // modulo owner is node 3 while both subscriptions exist
	before, _ := c.nodes[1].sm.ConsumerVersion()
	c.stop(3)
	require.Eventually(t, func() bool {
		for _, id := range []uint64{1, 2} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := c.nodes[id].coordinator.Tick(ctx, time.Now())
			cancel()
			if err != nil {
				return false
			}
		}
		members := c.nodes[1].sm.ConsumerGroup("orders", "workers")
		return len(members) == 1 && members[0].ID == "local"
	}, 10*time.Second, 50*time.Millisecond)
	after, active := c.nodes[1].sm.ConsumerVersion()
	require.True(t, active)
	require.Greater(t, after, before)
	require.Eventually(t, func() bool {
		return c.nodes[1].dispatcher.dispatchAll() == 1
	}, 3*time.Second, 20*time.Millisecond)
	require.Equal(t, key, receiveReplicaMessage(t, local).DeliveryTag)
	// Fencing changes delivery ownership, not Dragonboat membership.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	membership, err := c.nodes[1].nh.SyncGetShardMembership(ctx, replicaTestShard)
	cancel()
	require.NoError(t, err)
	require.Len(t, membership.Nodes, 3)

	// Losing quorum cannot authorize another liveness transition.
	c.stop(2)
	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	require.Error(t, c.nodes[1].coordinator.Tick(ctx, time.Now()))
	cancel()
	current, _ := c.nodes[1].sm.ConsumerVersion()
	require.Equal(t, after, current)
}
