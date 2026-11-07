package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSessionRenewalBoundsConcurrencyAndAttemptsEveryBatch(t *testing.T) {
	s := newTestService(t, "pebble", 0)
	for i := range 256*6 + 1 {
		id := fmt.Sprint(i)
		s.sessions[id] = &deliverypb.SubscriptionMutation{Topic: "orders", GroupId: "workers", Session: id, Recipient: "g:s:original"}
	}
	var active, peak, calls atomic.Int32
	var mu sync.Mutex
	seen := make(map[string]int)
	entered := make(chan struct{}, renewalWorkers)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Propose = func(ctx context.Context, data []byte) (statemachine.Result, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		call := calls.Add(1)
		command := new(deliverypb.SubscriptionCommand)
		if err := proto.Unmarshal(data[1:], command); err != nil {
			return statemachine.Result{}, err
		}
		mu.Lock()
		for _, m := range command.Mutations {
			seen[m.Session]++
		}
		mu.Unlock()
		if call <= renewalWorkers {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return statemachine.Result{}, ctx.Err()
			}
		}
		if call == 1 {
			return statemachine.Result{}, errors.New("first batch unavailable")
		}
		return statemachine.Result{}, nil
	}
	done := make(chan error, 1)
	go func() { done <- s.renew(ctx) }()
	for range renewalWorkers {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("renewals did not run concurrently")
		}
	}
	require.EqualValues(t, renewalWorkers, peak.Load())
	close(release)
	require.ErrorContains(t, <-done, "first batch unavailable")
	require.EqualValues(t, 7, calls.Load(), "one failed batch must not prevent later independent renewals")
	for _, count := range seen {
		require.Equal(t, 1, count)
	}
	require.Len(t, seen, 256*6+1)
}

func TestAsyncDisconnectRetriesWithoutResurrectingOrRemovingNewSession(t *testing.T) {
	s := newTestService(t, "pebble", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := s.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	_, err = s.Connect(ctx, "orders", "workers", "", "old")
	require.NoError(t, err)
	local := New(s.DB, 0, s.Logger)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	s.Propose = func(ctx context.Context, data []byte) (statemachine.Result, error) {
		cmd := new(deliverypb.SubscriptionCommand)
		if err := proto.Unmarshal(data[1:], cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == raft.SubscriptionDisconnect && calls.Add(1) == 1 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return statemachine.Result{}, ctx.Err()
			}
			// Simulate an applied command whose response was lost.
			if _, err := local.apply(ctx, cmd); err != nil {
				return statemachine.Result{}, err
			}
			return statemachine.Result{}, errors.New("lost response")
		}
		return local.apply(ctx, cmd)
	}
	s.DisconnectAsync("old")
	require.Empty(t, s.sessions, "disconnected sessions must immediately stop renewing")
	done := make(chan struct{})
	go func() { s.runDisconnects(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("disconnect worker did not start")
	}
	_, err = s.Connect(ctx, "orders", "workers", "", "new")
	require.NoError(t, err)
	require.NoError(t, s.renew(ctx), "blocked disconnect must not hold the renewal mutex")
	close(release)
	require.Eventually(t, func() bool { return calls.Load() >= 2 }, 2*time.Second, time.Millisecond)
	record, err := raft.ReadSubscription(s.DB, "orders", "workers", "")
	require.NoError(t, err)
	require.NotContains(t, record.Sessions, "old")
	require.Contains(t, record.Sessions, "new")
	require.True(t, record.Active)
}

func TestAsyncDisconnectQueueIsBoundedAndStopsAllRenewal(t *testing.T) {
	s := newTestService(t, "pebble", 0)
	for i := range disconnectQueueSize + 1 {
		id := fmt.Sprint(i)
		s.sessions[id] = &deliverypb.SubscriptionMutation{Session: id}
		s.DisconnectAsync(id)
	}
	require.Empty(t, s.sessions)
	require.Len(t, s.disconnects, disconnectQueueSize)
}
