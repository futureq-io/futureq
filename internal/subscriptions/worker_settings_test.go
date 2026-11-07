package subscriptions

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
)

func TestConfiguredRenewalWorkerLimit(t *testing.T) {
	for _, limit := range []int{1, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s := newTestService(t, "pebble", 0)
			s.RenewalWorkers = limit
			for i := range (limit + 1) * 256 {
				id := fmt.Sprint(i)
				s.sessions[id] = &deliverypb.SubscriptionMutation{Topic: "orders", GroupId: "workers", Session: id}
			}
			ctx, cancel := context.WithCancel(context.Background())
			entered := make(chan struct{}, limit+1)
			release := make(chan struct{})
			var active, peak atomic.Int32
			s.Propose = func(ctx context.Context, _ []byte) (statemachine.Result, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				entered <- struct{}{}
				select {
				case <-release:
					return statemachine.Result{}, nil
				case <-ctx.Done():
					return statemachine.Result{}, ctx.Err()
				}
			}
			done := make(chan struct{})
			var err error
			go func() { err = s.renew(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			for range limit {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("configured renewal workers did not start")
				}
			}
			require.EqualValues(t, limit, active.Load())
			require.Never(t, func() bool { return len(entered) > 0 }, 20*time.Millisecond, time.Millisecond)
			close(release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("renewal workers did not finish")
			}
			require.NoError(t, err)
			require.EqualValues(t, limit, peak.Load())
		})
	}
}

func TestConfiguredDisconnectWorkersCancelAndJoin(t *testing.T) {
	s := newTestService(t, "pebble", 0)
	s.DisconnectWorkers = 3
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 4)
	var active atomic.Int32
	s.Propose = func(ctx context.Context, _ []byte) (statemachine.Result, error) {
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		<-ctx.Done()
		return statemachine.Result{}, ctx.Err()
	}
	done := make(chan struct{})
	go func() { s.runDisconnectPool(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := range s.DisconnectWorkers {
		id := fmt.Sprint(i)
		s.sessions[id] = &deliverypb.SubscriptionMutation{Topic: "orders", GroupId: "workers", Session: id}
		s.DisconnectAsync(id)
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("configured disconnect worker did not start")
		}
	}
	s.sessions["extra"] = &deliverypb.SubscriptionMutation{Topic: "orders", GroupId: "workers", Session: "extra"}
	s.DisconnectAsync("extra")
	require.EqualValues(t, s.DisconnectWorkers, active.Load())
	require.Never(t, func() bool { return len(entered) > 0 }, 20*time.Millisecond, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disconnect pool did not join")
	}
	require.Zero(t, active.Load())
}
