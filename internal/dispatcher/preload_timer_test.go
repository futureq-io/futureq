package dispatcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

func TestPreloadFailedRenewalDoesNotSpinOnOverdueTimer(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.interval = time.Second
	d.Limits.WorkTime = time.Millisecond
	d.Limits.PrepareTimeout = time.Second
	epoch := uint64(9)
	d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, true }, time.Second)
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
	d.ReadBarrier = func() error {
		d.hub.GrantDeliveryPermit(epoch, time.Now())
		return nil
	}
	putPipelineEvent(t, d, "orders", 1, time.Now().Add(25*time.Millisecond).UnixMilli())
	require.Zero(t, d.dispatchAll())
	require.Equal(t, 1, d.preloadCount)
	// Drain the registration wake so only the due timer could trigger a retry.
	select {
	case <-d.wakeCh:
	default:
	}
	var calls atomic.Int64
	d.ReadBarrier = func() error {
		calls.Add(1)
		return errors.New("renewal unavailable while old permit is still valid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	d.Run(ctx)
	require.EqualValues(t, 1, calls.Load(), "a failed renewal must wait for the normal poll or an external wake, not spin on a cached deadline")
}
