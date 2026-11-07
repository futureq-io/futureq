package metadata

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAsyncConsumerRemovalsRetryInOrderAndJoinOnCancel(t *testing.T) {
	s := NewService(nil, nil, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan string, 8)
	release := make(chan struct{})
	var active, calls atomic.Int32
	done := make(chan struct{})
	go func() {
		s.runConsumerRemovals(ctx, func(ctx context.Context, removal consumerRemoval) error {
			if active.Add(1) != 1 {
				panic("concurrent removal")
			}
			defer active.Add(-1)
			n := calls.Add(1)
			entered <- removal.member.ID
			if n == 1 {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				return errors.New("uncertain proposal")
			}
			if removal.member.ID == "second" {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	s.RemoveConsumerAsync(1, "orders", "workers", ConsumerMember{ID: "first", NodeID: 1})
	select {
	case id := <-entered:
		require.Equal(t, "first", id)
	case <-time.After(time.Second):
		t.Fatal("removal did not start")
	}
	// Enqueueing another removal must not wait for the blocked proposal.
	s.RemoveConsumerAsync(1, "orders", "workers", ConsumerMember{ID: "second", NodeID: 1})
	close(release)
	for _, want := range []string{"first", "second"} {
		select {
		case id := <-entered:
			require.Equal(t, want, id)
		case <-time.After(2 * time.Second):
			t.Fatal("removal retry did not finish")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("removal worker did not join")
	}
	require.Zero(t, active.Load())
}

func TestAsyncConsumerRemovalQueueIsBounded(t *testing.T) {
	s := NewService(nil, nil, zap.NewNop())
	for i := range consumerRemovalQueueSize + 1 {
		s.RemoveConsumerAsync(1, "orders", "workers", ConsumerMember{ID: fmt.Sprint(i), NodeID: 1})
	}
	require.Len(t, s.consumerRemovals, consumerRemovalQueueSize)
}
