package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestConsumerBarrierReadsConcurrentlyAndRequiresBothQuorums(t *testing.T) {
	const noFailure = ^uint64(0)
	for _, failingShard := range []uint64{noFailure, metadata.MetadataShardID, 99} {
		t.Run(fmt.Sprint(failingShard), func(t *testing.T) {
			h := NewHub(NewRoundRobinStrategy(), zap.NewNop(), nil)
			version := func() (uint64, bool) { return 1, true }
			h.SetDeliveryFence(version, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan uint64, 2)
			release := make(chan struct{})
			barrier := newConsumerReadBarrier(ctx, func(ctx context.Context, shard uint64) error {
				entered <- shard
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				if shard == failingShard {
					return errors.New("no quorum")
				}
				return nil
			}, version, h, 99)
			done := make(chan error, 1)
			go func() { done <- barrier() }()
			for range 2 {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("shard reads were serialized")
				}
			}
			require.False(t, h.DeliveryPermitted())
			close(release)
			if failingShard == noFailure {
				require.NoError(t, <-done)
				require.True(t, h.DeliveryPermitted())
			} else {
				require.Error(t, <-done)
				require.False(t, h.DeliveryPermitted())
			}
		})
	}
}

func TestPreparedPublishedJobWaitsForSharedRenewalAndRechecksAck(t *testing.T) {
	for _, ackWhileWaiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "deliver", true: "already-acked"}[ackWhileWaiting], func(t *testing.T) {
			d, ledger := newPipelineTest(t)
			d.Limits.Workers = 1
			version := func() (uint64, bool) { return 1, true }
			d.hub.SetDeliveryFence(version, time.Second)
			ch := make(chan *pb.QueueMessage, 2)
			d.hub.Register("a", "orders", "a", ch)
			key := putPublishedEvent(t, d, 1, time.Now().Add(-time.Second).UnixMilli(), []string{"g:a"})
			ctx, cancel := context.WithCancel(context.Background())
			var barriers, preparations atomic.Int32
			renewing := make(chan struct{})
			release := make(chan struct{})
			d.ReadBarrier = func() error {
				n := barriers.Add(1)
				if n > 1 {
					if n == 2 {
						close(renewing)
					}
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				d.hub.GrantDeliveryPermit(1, time.Now())
				return nil
			}
			d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
				states, err := ledger.PublishedBatch(ctx, items)
				if preparations.Add(1) == 1 {
					// Invalidate the permit during validation, as a rebalance or
					// slow local read can do, without changing the assignment.
					d.hub.WithRebalanceBarrier(func() {})
				}
				return states, err
			}
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			select {
			case <-renewing:
			case <-time.After(time.Second):
				t.Fatal("renewal did not start")
			}
			// Neither the delivery worker nor repeated scheduler ticks may
			// start another read while the shared refresh is blocked.
			require.Never(t, func() bool { return barriers.Load() > 2 || len(ch) > 0 }, 30*time.Millisecond, time.Millisecond)
			if ackWhileWaiting {
				require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
			}
			close(release)
			if ackWhileWaiting {
				require.Never(t, func() bool { return len(ch) > 0 }, 40*time.Millisecond, time.Millisecond)
			} else {
				select {
				case msg := <-ch:
					require.Equal(t, key, msg.DeliveryTag)
				case <-time.After(time.Second):
					t.Fatal("prepared batch did not resume")
				}
				require.EqualValues(t, 1, preparations.Load(), "resume must retain validated work")
			}
		})
	}
}

func TestPublishBoundAuthorizationDoesNotReadWhenPermitExpired(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.hub.SetDeliveryFence(func() (uint64, bool) { return 1, true }, time.Second)
	d.ReadBarrier = func() error { t.Fatal("delivery worker performed a quorum read"); return nil }
	require.False(t, d.authorizePrepared(context.Background(), map[string]*raft.DeliveryState{"key": {PublishBound: true}}))
}
