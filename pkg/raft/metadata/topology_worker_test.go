package metadata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lni/dragonboat/v4/raftio"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestTopologyEventsCoalesceRetryAndStop(t *testing.T) {
	s := NewService(nil, nil, zap.NewNop())
	// Callbacks can arrive before NodeHost and the worker are ready.
	s.NodeReady(raftio.NodeInfo{ShardID: 1})
	type update struct {
		shardID uint64
		done    chan error
	}
	started := make(chan update, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runTopology(ctx, func(ctx context.Context, shardID uint64) error {
			call := update{shardID: shardID, done: make(chan error, 1)}
			started <- call
			select {
			case err := <-call.done:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("topology worker did not stop")
		}
	})
	next := func() update {
		t.Helper()
		select {
		case call := <-started:
			return call
		case <-time.After(2 * time.Second):
			t.Fatal("topology refresh did not run")
			return update{}
		}
	}
	first := next()
	require.Equal(t, uint64(1), first.shardID)
	first.done <- errors.New("no quorum")
	retry := next() // Retry must happen without another event.
	require.Equal(t, uint64(1), retry.shardID)

	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for i := 0; i < 1000; i++ {
			s.LeaderUpdated(raftio.LeaderInfo{ShardID: 1, LeaderID: 2})
			s.MembershipChanged(raftio.NodeInfo{ShardID: 1})
			s.NodeDeleted(raftio.NodeInfo{ShardID: 1})
			s.NodeReady(raftio.NodeInfo{ShardID: 1})
		}
	}()
	select {
	case <-eventsDone:
	case <-time.After(time.Second):
		t.Fatal("Raft event callbacks blocked on topology publication")
	}
	s.mu.Lock()
	pending := len(s.topologyPending)
	s.mu.Unlock()
	require.Equal(t, 1, pending, "event storms must coalesce by shard")
	require.Empty(t, started, "topology publications must stay serial")
	retry.done <- nil
	afterEvents := next() // Events arriving during an in-progress update survive.
	require.Equal(t, uint64(1), afterEvents.shardID)
	select {
	case <-started:
		t.Fatal("overlapping topology publications")
	case <-time.After(20 * time.Millisecond):
	}
	cancel() // Cancel the outstanding refresh; Run must wait for it to exit.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown left a topology refresh running")
	}
}
