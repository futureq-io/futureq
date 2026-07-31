package dispatcher

import (
	"testing"

	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

type HubSuite struct {
	suite.Suite
}

func TestHubSuite(t *testing.T) {
	suite.Run(t, new(HubSuite))
}

func (s *HubSuite) newHub(wakeCh chan struct{}) *Hub {
	return NewHub(NewRoundRobinStrategy(), zap.NewNop(), wakeCh)
}

func (s *HubSuite) newConsumer(id, topic, group string) *ConsumerEntry {
	return &ConsumerEntry{
		ID:    id,
		Topic: topic,
		Group: group,
		Ch:    make(chan *pb.QueueMessage, 16),
	}
}

// ─── ConsumerEntry ──────────────────────────────────────────────────────────

func (s *HubSuite) TestConsumerEntry_GroupKey_UniversalUnique() {
	require := s.Require()

	// Universal consumers (empty group) must each get a unique GroupKey.
	c1 := &ConsumerEntry{ID: "id-1", Topic: "t", Group: ""}
	c2 := &ConsumerEntry{ID: "id-2", Topic: "t", Group: ""}
	require.NotEqual(c1.GroupKey(), c2.GroupKey())
}

func (s *HubSuite) TestConsumerEntry_GroupKey_SameGroup_SameKey() {
	require := s.Require()

	c1 := &ConsumerEntry{ID: "a", Topic: "t", Group: "g1"}
	c2 := &ConsumerEntry{ID: "b", Topic: "t", Group: "g1"}
	require.Equal(c1.GroupKey(), c2.GroupKey())
}

func (s *HubSuite) TestConsumerEntry_IsUniversal() {
	require := s.Require()

	require.True((&ConsumerEntry{Group: ""}).IsUniversal())
	require.False((&ConsumerEntry{Group: "g1"}).IsUniversal())
}

// ─── Register / Unregister ──────────────────────────────────────────────────

func (s *HubSuite) TestRegister_SendsWakeSignal() {
	require := s.Require()

	wakeCh := make(chan struct{}, 1)
	h := s.newHub(wakeCh)

	h.Register("id-1", "topic", "group-1", make(chan *pb.QueueMessage, 1))

	select {
	case <-wakeCh:
		// OK
	default:
		require.Fail("expected wake signal on Register")
	}
}

func (s *HubSuite) TestRegister_TopicListedInActiveTopics() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))
	h.Register("id-1", "orders", "g1", make(chan *pb.QueueMessage, 1))

	topics := h.ActiveTopics()
	require.Contains(topics, "orders")
}

func (s *HubSuite) TestRegister_HasConsumers_True() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))
	require.False(h.HasConsumers())

	h.Register("id-1", "orders", "g1", make(chan *pb.QueueMessage, 1))
	require.True(h.HasConsumers())
}

func (s *HubSuite) TestUnregister_RemovesFromActiveTopics() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))
	h.Register("id-1", "orders", "g1", make(chan *pb.QueueMessage, 1))
	h.Unregister("id-1")

	require.Empty(h.ActiveTopics())
	require.False(h.HasConsumers())
}

func (s *HubSuite) TestUnregister_UnknownID_NoOp() {
	h := s.newHub(make(chan struct{}, 1))
	// Must not panic.
	h.Unregister("nonexistent")
}

// ─── DispatchToTopic ────────────────────────────────────────────────────────

func (s *HubSuite) TestDispatchToTopic_UnknownTopic_ReturnsZero() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))
	msg := &pb.QueueMessage{Topic: "nothing"}

	require.Equal(0, h.DispatchToTopic("nothing", msg, []byte("tag")))
}

func (s *HubSuite) TestDispatchToTopic_GroupedConsumer_ExactlyOneReceives() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))

	ch1 := make(chan *pb.QueueMessage, 1)
	ch2 := make(chan *pb.QueueMessage, 1)
	h.Register("c1", "orders", "g1", ch1)
	h.Register("c2", "orders", "g1", ch2)

	msg := &pb.QueueMessage{Topic: "orders", Payload: []byte("x")}
	sent := h.DispatchToTopic("orders", msg, []byte("tag"))
	require.Equal(1, sent, "only one consumer in the group should receive the message")

	// Exactly one of ch1, ch2 should have the message.
	got1 := len(ch1)
	got2 := len(ch2)
	require.Equal(1, got1+got2)
}

func (s *HubSuite) TestDispatchToTopic_UniversalConsumers_AllReceive() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))

	ch1 := make(chan *pb.QueueMessage, 1)
	ch2 := make(chan *pb.QueueMessage, 1)
	// Empty group → universal.
	h.Register("u1", "orders", "", ch1)
	h.Register("u2", "orders", "", ch2)

	msg := &pb.QueueMessage{Topic: "orders", Payload: []byte("x")}
	sent := h.DispatchToTopic("orders", msg, []byte("tag"))
	require.Equal(2, sent, "each universal consumer should receive a copy")
}

func (s *HubSuite) TestDispatchToTopic_MultipleGroups_EachGroupReceivesOne() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))

	chG1A := make(chan *pb.QueueMessage, 1)
	chG1B := make(chan *pb.QueueMessage, 1)
	chG2 := make(chan *pb.QueueMessage, 1)

	h.Register("c1", "orders", "g1", chG1A)
	h.Register("c2", "orders", "g1", chG1B)
	h.Register("c3", "orders", "g2", chG2)

	msg := &pb.QueueMessage{Topic: "orders"}
	sent := h.DispatchToTopic("orders", msg, []byte("tag"))
	require.Equal(2, sent, "one consumer per group → 2 groups → 2 sends")
}

func (s *HubSuite) TestDispatchToTopic_FullChannel_Skipped() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))

	// Buffer of 1, fill it before dispatch.
	ch := make(chan *pb.QueueMessage, 1)
	ch <- &pb.QueueMessage{Topic: "other"}

	h.Register("c1", "orders", "g1", ch)

	msg := &pb.QueueMessage{Topic: "orders"}
	sent := h.DispatchToTopic("orders", msg, []byte("tag"))
	require.Equal(0, sent, "full channel should be skipped without blocking")
}

// ─── RemoveInFlightForConsumer ──────────────────────────────────────────────

func (s *HubSuite) TestRemoveInFlightForConsumer_RemovesMatchingKey() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))

	ch := make(chan *pb.QueueMessage, 2)
	h.Register("c1", "orders", "g1", ch)

	// Two successful dispatches → two keys in flight.
	msg := &pb.QueueMessage{Topic: "orders"}
	key1 := []byte("key-1")
	key2 := []byte("key-2")
	h.DispatchToTopic("orders", msg, key1)
	h.DispatchToTopic("orders", msg, key2)

	require.Len(h.inFlightByConsumer["c1"], 2)

	h.RemoveInFlightForConsumer("c1", key1)
	require.Len(h.inFlightByConsumer["c1"], 1)
	require.Equal(key2, h.inFlightByConsumer["c1"][0])
}

func (s *HubSuite) TestRemoveInFlightForConsumer_UnknownConsumer_NoOp() {
	h := s.newHub(make(chan struct{}, 1))
	h.RemoveInFlightForConsumer("nobody", []byte("key"))
	// Must not panic.
}

// ─── GroupsForTopic ─────────────────────────────────────────────────────────

func (s *HubSuite) TestGroupsForTopic_OnlyGroupedConsumers() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))
	h.Register("c1", "orders", "g1", make(chan *pb.QueueMessage, 1))
	h.Register("c2", "orders", "g2", make(chan *pb.QueueMessage, 1))
	h.Register("u1", "orders", "", make(chan *pb.QueueMessage, 1)) // universal

	groups := h.GroupsForTopic("orders")
	require.Len(groups, 2)
	require.Contains(groups, "g1")
	require.Contains(groups, "g2")
}

func (s *HubSuite) TestGroupsForTopic_UnknownTopic_ReturnsNil() {
	require := s.Require()

	h := s.newHub(make(chan struct{}, 1))
	require.Nil(h.GroupsForTopic("missing"))
}

// ─── RoundRobinStrategy ──────────────────────────────────────────────────────

func (s *HubSuite) TestRoundRobinStrategy_EmptyCandidates_ReturnsNil() {
	require := s.Require()

	strategy := NewRoundRobinStrategy()
	require.Nil(strategy.Select(nil, &pb.QueueMessage{}))
}

func (s *HubSuite) TestRoundRobinStrategy_CyclesThroughConsumers() {
	require := s.Require()

	strategy := NewRoundRobinStrategy()

	c1 := &ConsumerEntry{ID: "c1", Topic: "t", Group: "g"}
	c2 := &ConsumerEntry{ID: "c2", Topic: "t", Group: "g"}
	c3 := &ConsumerEntry{ID: "c3", Topic: "t", Group: "g"}

	candidates := []*ConsumerEntry{c1, c2, c3}

	// Call Select 6 times — each candidate should be picked exactly twice.
	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		sel := strategy.Select(candidates, &pb.QueueMessage{})
		require.NotNil(sel)
		counts[sel.ID]++
	}

	require.Equal(2, counts["c1"])
	require.Equal(2, counts["c2"])
	require.Equal(2, counts["c3"])
}

func (s *HubSuite) TestRoundRobinStrategy_PerGroupCountersAreIndependent() {
	require := s.Require()

	strategy := NewRoundRobinStrategy()

	g1a := &ConsumerEntry{ID: "g1a", Topic: "t", Group: "g1"}
	g1b := &ConsumerEntry{ID: "g1b", Topic: "t", Group: "g1"}

	g2a := &ConsumerEntry{ID: "g2a", Topic: "t", Group: "g2"}
	g2b := &ConsumerEntry{ID: "g2b", Topic: "t", Group: "g2"}

	// First call in g1 → g1a; first call in g2 → g2a.
	require.Equal("g1a", strategy.Select([]*ConsumerEntry{g1a, g1b}, nil).ID)
	require.Equal("g2a", strategy.Select([]*ConsumerEntry{g2a, g2b}, nil).ID)

	// Second call in g1 → g1b; second in g2 → g2b.
	require.Equal("g1b", strategy.Select([]*ConsumerEntry{g1a, g1b}, nil).ID)
	require.Equal("g2b", strategy.Select([]*ConsumerEntry{g2a, g2b}, nil).ID)
}
