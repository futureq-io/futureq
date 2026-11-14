package dispatcher

import (
	"context"
	"testing"
	"time"

	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

func TestReviewRenewalAllowsLongPreparationBudget(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.Limits.ValidationTimeout = time.Second
	d.hub.SetDeliveryFence(func() (uint64, bool) { return 1, true }, time.Second)
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	d.ReadBarrier = func() error {
		d.hub.GrantDeliveryPermit(1, time.Now())
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case msg := <-ch:
		require.Equal(t, key, msg.DeliveryTag)
		require.True(t, d.hub.BeginSend("local", msg))
	case <-time.After(500 * time.Millisecond):
		t.Fatal("successful permit renewals never admitted delivery")
	}
}

func TestReviewOlderBarrierCannotShortenPermit(t *testing.T) {
	d, _ := newPipelineTest(t)
	epoch := uint64(1)
	d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, true }, time.Second)
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	newer := time.Now()
	d.hub.GrantDeliveryPermit(epoch, newer)
	key := putPipelineEvent(t, d, "orders", 1, newer.Add(-time.Second).UnixMilli())
	require.Equal(t, 1, d.dispatchAll())
	msg := <-ch
	d.hub.GrantDeliveryPermit(epoch, newer.Add(-2*ConsumerReadPermit))
	require.Equal(t, newer.Add(ConsumerReadPermit), d.hub.permitUntil)
	require.True(t, d.hub.BeginSend("local", msg), "an older read must not revoke a newer quorum grant")
	require.Equal(t, key, msg.DeliveryTag)
	// A new epoch must not inherit the old epoch's deadline.
	epoch++
	older := newer.Add(-ConsumerReadPermit / 2)
	d.hub.GrantDeliveryPermit(epoch, older)
	require.Equal(t, older.Add(ConsumerReadPermit), d.hub.permitUntil)
	// Expiry is still measured from read start, not completion.
	epoch++
	d.hub.GrantDeliveryPermit(epoch, newer.Add(-2*ConsumerReadPermit))
	require.False(t, d.hub.DeliveryPermitted())
}
