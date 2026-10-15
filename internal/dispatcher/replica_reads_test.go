package dispatcher

import (
	"encoding/binary"
	"testing"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestReplicaGroupModuloAssignment(t *testing.T) {
	members := []metadata.ConsumerMember{{ID: "a", NodeID: 1}, {ID: "b", NodeID: 1}, {ID: "c", NodeID: 2}}
	source := func(_, _ string) []metadata.ConsumerMember { return members }
	node1 := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	node2 := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	node1.SetGroupMembership(source)
	node2.SetGroupMembership(source)
	for _, item := range []struct {
		hub *Hub
		id  string
	}{{node1, "a"}, {node1, "b"}, {node2, "c"}} {
		item.hub.Register(item.id, "orders", "workers", make(chan *pb.QueueMessage, 8))
	}

	dispatch := func(id uint64) string {
		tag := make([]byte, 24)
		binary.BigEndian.PutUint64(tag[16:], id)
		msg := &pb.QueueMessage{Topic: "orders", DeliveryTag: tag}
		node1.DispatchToTopic("orders", msg, tag)
		node2.DispatchToTopic("orders", msg, tag)
		for _, hub := range []*Hub{node1, node2} {
			for _, entry := range hub.byID {
				select {
				case <-entry.Ch:
					hub.RemoveInFlightForConsumer(entry.ID, tag)
					return entry.ID
				default:
				}
			}
		}
		return ""
	}
	for id, want := range []string{"a", "b", "c", "a", "b", "c"} {
		require.Equal(t, want, dispatch(uint64(id)))
	}

	// A new node-2 member owns remainders 2 and 3 after activation.
	node2.Register("d", "orders", "workers", make(chan *pb.QueueMessage, 8))
	members = []metadata.ConsumerMember{{ID: "a", NodeID: 1}, {ID: "b", NodeID: 1}, {ID: "c", NodeID: 2}, {ID: "d", NodeID: 2}}
	for id, want := range []string{"a", "b", "c", "d"} {
		require.Equal(t, want, dispatch(uint64(id)))
	}

	// A pending metadata barrier stops grouped delivery on every replica.
	members = nil
	require.Empty(t, dispatch(100))
}

func TestRoundRobinAdaptsToAddedConsumer(t *testing.T) {
	strategy := NewRoundRobinStrategy()
	a := &ConsumerEntry{ID: "a", Topic: "orders", Group: "workers"}
	b := &ConsumerEntry{ID: "b", Topic: "orders", Group: "workers"}
	c := &ConsumerEntry{ID: "c", Topic: "orders", Group: "workers"}
	require.Equal(t, "a", strategy.Select([]*ConsumerEntry{a, b}, nil).ID)
	require.Equal(t, "b", strategy.Select([]*ConsumerEntry{a, b}, nil).ID)
	require.Equal(t, "c", strategy.Select([]*ConsumerEntry{a, b, c}, nil).ID)
}

func TestReplicaUniversalConsumersKeepFanOutDuringGroupRebalance(t *testing.T) {
	for range 2 {
		hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
		// No named group can deliver while its metadata rebalance is pending.
		hub.SetGroupMembership(func(_, _ string) []metadata.ConsumerMember { return nil })
		first := make(chan *pb.QueueMessage, 1)
		second := make(chan *pb.QueueMessage, 1)
		grouped := make(chan *pb.QueueMessage, 1)
		hub.Register("first", "orders", "", first)
		hub.Register("second", "orders", "", second)
		hub.Register("worker", "orders", "workers", grouped)
		msg := &pb.QueueMessage{Topic: "orders"}
		require.Equal(t, []string{"", ""}, hub.DispatchToTopic("orders", msg, []byte("tag")))
		require.Equal(t, msg.Topic, (<-first).Topic)
		require.Equal(t, msg.Topic, (<-second).Topic)
		require.Empty(t, grouped)
		require.Zero(t, hub.GroupInFlightCount(), "universal deliveries do not block named group rebalances")
	}
}

func TestGroupDeliveryRemainsInFlightUntilReplicatedDelete(t *testing.T) {
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	hub.Register("c1", "orders", "workers", make(chan *pb.QueueMessage, 1))
	key := []byte("delivery-tag")
	require.Equal(t, []string{"workers"}, hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, key))
	require.True(t, hub.HasInFlightForConsumer("c1", key))
	require.False(t, hub.HasInFlightForConsumer("c1", []byte("other")))
	require.Equal(t, 1, hub.GroupInFlightCount())
	hub.RemoveDeletedBatch([][]byte{key})
	require.Equal(t, 0, hub.GroupInFlightCount())
}
