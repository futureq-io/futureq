package dispatcher

import (
	"testing"
	"time"

	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestDeliveryPermitRejectsExpiredReadsAndOldQueuedAttempts(t *testing.T) {
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	epoch, active := uint64(1), true
	hub.SetDeliveryFence(func() (uint64, bool) { return epoch, active }, time.Second)
	ch := make(chan *pb.QueueMessage, 4)
	hub.Register("consumer", "orders", "workers", ch)
	key := []byte("key")
	msg := &pb.QueueMessage{Topic: "orders", DeliveryTag: key}
	hub.GrantDeliveryPermit(epoch, time.Now().Add(-2*ConsumerReadPermit))
	require.Empty(t, hub.DispatchToTopic("orders", msg, key), "delayed quorum responses cannot renew old permits")
	hub.GrantDeliveryPermit(epoch, time.Now())
	require.NotEmpty(t, hub.DispatchToTopic("orders", msg, key))
	old := <-ch
	require.True(t, hub.CanSend("consumer", old))
	hub.WithRebalanceBarrier(func() { epoch++; active = false })
	require.False(t, hub.CanSend("consumer", old))
	require.Empty(t, hub.DispatchToTopic("orders", msg, key))
	hub.ExpireInFlight(time.Now().Add(2 * time.Second))
	require.Zero(t, hub.GroupInFlightCount())
	active = true
	hub.GrantDeliveryPermit(epoch, time.Now())
	require.NotEmpty(t, hub.DispatchToTopic("orders", msg, key))
	current := <-ch
	require.False(t, hub.CanSend("consumer", old), "an old buffered attempt cannot impersonate a retry of the same key")
	require.True(t, hub.CanSend("consumer", current))
}
