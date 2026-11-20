package dispatcher

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	key := func(id uint64) []byte { return utils.ShardStorageBucket(2, 1).EventKey(1, id) }
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

func TestAssignedGroupFallsBackWhenSelectedWorkerQueueIsFull(t *testing.T) {
	d, _ := newPipelineTest(t)
	h := d.hub
	const topic, group = "orders", "workers"
	full, healthy := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
	h.Register("a-full", topic, group, full)
	h.Register("b-healthy", topic, group, healthy)
	members := []metadata.ConsumerMember{{ID: "a-full", NodeID: 1}, {ID: "b-healthy", NodeID: 1}}
	h.SetLocalReplica(1)
	h.SetGroupMembership(func(string, string) []metadata.ConsumerMember { return members })
	h.SetDeliveryFence(func() (uint64, bool) { return 9, true }, time.Second)
	h.SetDeliveryView(func(string) metadata.TopicDeliveryView {
		return metadata.TopicDeliveryView{Active: true, Epoch: 9, Groups: map[string][]metadata.ConsumerMember{group: members}}
	})
	h.GrantDeliveryPermit(9, time.Now())
	full <- &pb.QueueMessage{}
	msg := &pb.QueueMessage{Topic: topic}
	bucket := utils.ShardStorageBucket(2, 1)
	for i := range 10 {
		key := bucket.EventKey(utils.TopicHash(topic), uint64(i+1))
		require.Equal(t, []string{group}, h.DispatchPrepared(topic, msg, key, nil))
		require.Equal(t, key, receiveReplicaMessage(t, healthy).DeliveryTag)
		require.Empty(t, h.DispatchPrepared(topic, msg, key, nil), "the group's reservation prevents a duplicate")
		require.Empty(t, healthy)
		require.True(t, h.RemoveInFlightForConsumer("b-healthy", key))
	}
	require.Len(t, full, 1)
}

func TestSaturatedGroupReportsOneWarningAndCanFallBack(t *testing.T) {
	const topic, group = "orders", "workers"
	var logs bytes.Buffer
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&logs), zap.WarnLevel)
	h := NewHub(NewRoundRobinStrategy(), zap.New(core), make(chan struct{}, 1))
	var lastQueue chan *pb.QueueMessage
	for i := range 1000 {
		queue := make(chan *pb.QueueMessage, 1)
		queue <- &pb.QueueMessage{}
		h.Register(fmt.Sprintf("worker-%04d", i), topic, group, queue)
		lastQueue = queue
	}
	key := utils.EventKey(0, utils.TopicHash(topic), 1)
	msg := &pb.QueueMessage{Topic: topic}
	require.Empty(t, h.DispatchPrepared(topic, msg, key, nil))
	require.Empty(t, h.groupInFlight)
	require.Empty(t, h.deliveryRecords, "full queues never reserve a delivery")
	require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte(`"level":"warn"`)))

	<-lastQueue
	require.Equal(t, []string{group}, h.DispatchPrepared(topic, msg, key, nil))
	require.Equal(t, key, receiveReplicaMessage(t, lastQueue).DeliveryTag)
	require.Len(t, h.groupInFlight, 1)
	require.Empty(t, h.DispatchPrepared(topic, msg, key, nil))
	require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte(`"level":"warn"`)), "fallback success and an existing reservation are quiet")
}
