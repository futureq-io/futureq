package subscriptions

import (
	"context"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSlowCleanupDoesNotBlockSessionRenewal(t *testing.T) {
	s := newTestService(t, "pebble", 0)
	ctx, cancel := context.WithCancel(context.Background())
	active, err := s.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	_, err = s.Connect(ctx, "orders", "workers", "", "live")
	require.NoError(t, err)
	before, err := raft.ReadSubscription(s.DB, "orders", "workers", "")
	require.NoError(t, err)
	retired, err := s.Create(ctx, "orders", "", "audit")
	require.NoError(t, err)
	key := putReceipt(t, s, 1, active.Recipient, retired.Recipient)
	require.NoError(t, s.Unsubscribe(ctx, "orders", "", "audit"))

	// This service applies mock Raft proposals without recursively proposing.
	local := New(s.DB, 0, s.Logger)
	cleanupStarted := make(chan struct{}, 8)
	releaseCleanup := make(chan struct{})
	renewed := make(chan struct{}, 8)
	s.Propose = func(ctx context.Context, data []byte) (statemachine.Result, error) {
		command := new(deliverypb.SubscriptionCommand)
		if err := proto.Unmarshal(data[1:], command); err != nil {
			return statemachine.Result{}, err
		}
		if command.Operation == raft.SubscriptionCleanup {
			cleanupStarted <- struct{}{}
			select {
			case <-releaseCleanup:
			case <-ctx.Done():
				return statemachine.Result{}, ctx.Err()
			}
		}
		result, err := local.apply(ctx, command)
		if command.Operation == raft.SubscriptionRenew && err == nil {
			renewed <- struct{}{}
		}
		return result, err
	}
	done := make(chan struct{})
	started := time.Now()
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("maintenance worker did not stop with the service")
		}
	})
	select {
	case <-cleanupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not start")
	}
	select {
	case <-renewed:
	case <-time.After(time.Until(started.Add(6 * time.Second))):
		t.Fatal("slow cleanup delayed session renewal")
	}
	current, err := raft.ReadSubscription(s.DB, "orders", "workers", "")
	require.NoError(t, err)
	require.Greater(t, current.Sessions["live"], before.Sessions["live"])
	require.True(t, current.Active)
	state, err := raft.ReadDeliveryState(s.DB, key)
	require.NoError(t, err)
	require.True(t, state.Needs(retired.Recipient), "failed cleanup must leave the receipt intact")

	close(releaseCleanup)
	require.Eventually(t, func() bool {
		state, err := raft.ReadDeliveryState(s.DB, key)
		return err == nil && state != nil && !state.Needs(retired.Recipient) && state.Needs(active.Recipient)
	}, 2*time.Second, 10*time.Millisecond, "cleanup retries must preserve the live recipient's backlog")
}
