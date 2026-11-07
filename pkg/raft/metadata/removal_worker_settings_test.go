package metadata

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestConfiguredRemovalWorkersCancelAndJoin(t *testing.T) {
	s := NewService(nil, nil, zap.NewNop())
	s.ConsumerRemovalWorkers = 3
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan string, 4)
	var active atomic.Int32
	done := make(chan struct{})
	go func() {
		s.runConsumerRemovalPool(ctx, func(ctx context.Context, removal consumerRemoval) error {
			active.Add(1)
			defer active.Add(-1)
			entered <- removal.member.ID
			<-ctx.Done()
			return ctx.Err()
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	for i := range s.ConsumerRemovalWorkers {
		id := fmt.Sprint(i)
		s.RemoveConsumerAsync(1, "orders", "workers", ConsumerMember{ID: id, NodeID: 1})
		select {
		case got := <-entered:
			require.Equal(t, id, got)
		case <-time.After(time.Second):
			t.Fatal("configured removal worker did not start")
		}
	}
	s.RemoveConsumerAsync(1, "orders", "workers", ConsumerMember{ID: "extra", NodeID: 1})
	require.EqualValues(t, s.ConsumerRemovalWorkers, active.Load())
	require.Never(t, func() bool { return len(entered) > 0 }, 20*time.Millisecond, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("removal pool did not join")
	}
	require.Zero(t, active.Load())
}
