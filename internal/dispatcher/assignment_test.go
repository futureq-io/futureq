package dispatcher

import (
	"testing"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestShardAssignmentCacheTracksCommittedAndLocalMembership(t *testing.T) {
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	hub.SetLocalReplica(1)
	const epoch = 4
	hub.SetDeliveryFence(func() (uint64, bool) { return epoch, true }, time.Second)
	members := []metadata.ConsumerMember{{ID: "first", NodeID: 1}}
	builds := 0
	hub.SetGroupMembership(func(_, _ string) []metadata.ConsumerMember { return members })
	hub.SetDeliveryView(func(string) metadata.TopicDeliveryView {
		builds++
		return metadata.TopicDeliveryView{
			Active: true, Epoch: epoch, Recipients: []string{"g:workers"},
			Groups: map[string][]metadata.ConsumerMember{"workers": members},
		}
	})
	hub.GrantDeliveryPermit(epoch, time.Now())
	first, joining := make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2)
	hub.Register("first", "orders", "workers", first)
	hub.Register("joining", "orders", "workers", joining)
	key := func(id uint64) []byte { return utils.EventKey(utils.ShardStorageBucket(2, 1), 1, id) }
	for id := uint64(1); id <= 2; id++ {
		require.True(t, hub.InterestView("orders").hasLocalOwner(key(id)))
		require.Equal(t, []string{"workers"}, hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, key(id)))
		<-first
	}
	require.Empty(t, joining)
	require.Equal(t, 1, builds, "discovery and sends reuse the same assignment")

	// A restored metadata snapshot can change membership at the same epoch.
	hub.WithConsumerBarrier(func() bool {
		members = []metadata.ConsumerMember{{ID: "joining", NodeID: 1}}
		return true
	})
	require.Empty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, key(3)))
	hub.GrantDeliveryPermit(epoch, time.Now())
	require.Equal(t, []string{"workers"}, hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, key(3)))
	require.Len(t, joining, 1)
	require.Empty(t, first)
	require.Equal(t, 2, builds)

	hub.Unregister("joining")
	require.Empty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, key(4)))
	require.Equal(t, 3, builds)
}
