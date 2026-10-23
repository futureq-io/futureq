package dispatcher

import (
	"context"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

func TestReviewRenewalAllowsLongPreparationBudget(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.Limits.PrepareTimeout = time.Second
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

func TestReviewReplacedDeadlineCannotFreezeRecipients(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, replaceAfterCollection := range []bool{false, true} {
			t.Run(engine+"/"+map[bool]string{false: "cached", true: "assigned"}[replaceAfterCollection], func(t *testing.T) {
				d, _ := newPreloadTest(t, engine)
				d.TimeBucket = time.Hour
				first, second := make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2)
				d.hub.Register("first", "orders", "first-group", first)
				// Keep every due time in the same bucket and avoid timing-dependent waits.
				now := time.Now().UnixMilli()
				oldDue := now - 1000
				laterDue := now + 5000
				if time.UnixMilli(oldDue).Truncate(time.Hour) != time.UnixMilli(laterDue).Truncate(time.Hour) {
					t.Skip("at a bucket boundary")
				}
				key := putPipelineEvent(t, d, "orders", 1, oldDue)
				view := d.hub.InterestView("orders")
				p := &topicPreload{epoch: view.Epoch}
				d.preloads = map[string]*topicPreload{"orders": p}
				d.preloadTopic("orders", p, view, oldDue-1000)
				require.Equal(t, 1, d.preloadCount)
				var chunk deliveryChunk
				if replaceAfterCollection {
					chunk = d.collectPreloaded("orders", d.preloads["orders"], d.hub.InterestView("orders"), now)
					require.Len(t, chunk.items, 1)
				}
				require.Equal(t, key, putPipelineEvent(t, d, "orders", 1, laterDue))
				if !replaceAfterCollection {
					chunk = d.collectPreloaded("orders", d.preloads["orders"], d.hub.InterestView("orders"), now)
				}
				_, err := d.prepareChunk(context.Background(), chunk)
				require.NoError(t, err)
				state, err := raft.ReadDeliveryState(d.db, key)
				require.NoError(t, err)
				require.Nil(t, state, "a replacement that is not due must not freeze recipients")
				d.hub.Register("second", "orders", "second-group", second)
				// Make the replacement due and verify cyclic repair sees both groups.
				require.Equal(t, key, putPipelineEvent(t, d, "orders", 1, oldDue))
				for range 4 {
					d.dispatchAll()
				}
				require.Len(t, first, 1)
				require.Len(t, second, 1)
			})
		}
	}
}
