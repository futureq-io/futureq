package subscriptions

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newTestService(t *testing.T, engine string, timeout time.Duration) *Service {
	t.Helper()
	var db storage.DB
	var err error
	if engine == "pebble" {
		db, err = storage.NewPebble(config.Pebble{Mode: "disk", WALEnabled: true, DataDir: filepath.Join(t.TempDir(), "db")}, zap.NewNop())
	} else {
		db, err = storage.NewBoltDB(config.Bolt{File: filepath.Join(t.TempDir(), "db")})
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return New(db, timeout, zap.NewNop())
}
func putReceipt(t *testing.T, s *Service, id uint64, recipients ...string) []byte {
	t.Helper()
	key := utils.EventKey(1, 2, id)
	receipt, err := raft.MarshalPublishReceipt(0, recipients)
	require.NoError(t, err)
	b := s.DB.NewBatch()
	defer b.Close() //nolint:errcheck
	require.NoError(t, b.Set(key, []byte("payload")))
	require.NoError(t, b.Set(raft.DeliveryStateKey(key), receipt))
	require.NoError(t, b.Commit(storage.Sync))
	return key
}
func TestSubscriptionOfflineUnsubscribeAndRecreate(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			s := newTestService(t, engine, 0)
			ctx := context.Background()
			group, err := s.Create(ctx, "orders", "billing", "")
			require.NoError(t, err)
			universal, err := s.Create(ctx, "orders", "", "audit")
			require.NoError(t, err)
			repeated, err := s.Create(ctx, "orders", "", "audit")
			require.NoError(t, err)
			require.Equal(t, universal.Recipient, repeated.Recipient)
			names, err := raft.ReadSubscriptionRecipients(s.DB, "orders")
			require.NoError(t, err)
			require.ElementsMatch(t, []string{group.Recipient, universal.Recipient}, names, "offline interests receive publications")
			key := putReceipt(t, s, 1, group.Recipient, universal.Recipient)
			only := putReceipt(t, s, 2, universal.Recipient)
			bound, err := s.Connect(ctx, "orders", "", "audit", "connection")
			require.NoError(t, err)
			require.Equal(t, universal.Recipient, bound.Recipient)
			require.True(t, s.Alive(bound.Recipient, "connection"))
			require.NoError(t, s.Disconnect(ctx, "connection"))
			require.NoError(t, s.Maintain(ctx, time.Now().Add(100*365*24*time.Hour)))
			state, err := raft.ReadDeliveryState(s.DB, key)
			require.NoError(t, err)
			require.True(t, state.Needs(universal.Recipient), "forever preserves disconnected universal backlog")
			require.NoError(t, s.Unsubscribe(ctx, "orders", "", "audit"))
			newer, err := s.Create(ctx, "orders", "", "audit")
			require.NoError(t, err)
			require.NotEqual(t, universal.Recipient, newer.Recipient)
			// A late disconnect from the old generation cannot affect the new one.
			_, err = s.command(ctx, raft.SubscriptionDisconnect, &deliverypb.SubscriptionMutation{Topic: "orders", SubscriberId: "audit", Recipient: universal.Recipient, Session: "connection"})
			require.NoError(t, err)
			require.NoError(t, s.Maintain(ctx, time.Now()))
			state, err = raft.ReadDeliveryState(s.DB, key)
			require.NoError(t, err)
			require.False(t, state.Needs(universal.Recipient))
			require.True(t, state.Needs(group.Recipient), "unsubscribe never drops another interest")
			require.False(t, state.Needs(newer.Recipient), "recreate must not inherit old backlog")
			_, _, err = s.DB.Get(only)
			require.ErrorIs(t, err, storage.ErrNotFound)
			require.NoError(t, s.Unsubscribe(ctx, "orders", "", "audit"))
			require.NoError(t, s.Unsubscribe(ctx, "orders", "", "audit"), "unsubscribe is idempotent")
			_, err = s.Connect(ctx, "orders", "", "audit", "new-connection")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}
func TestSubscriptionExpiryOnlyWhenAllWorkersDisconnected(t *testing.T) {
	for _, universal := range []bool{false, true} {
		t.Run(map[bool]string{false: "group", true: "universal"}[universal], func(t *testing.T) {
			s := newTestService(t, "pebble", time.Second)
			ctx := context.Background()
			group, subscriber := "workers", ""
			if universal {
				group, subscriber = "", "audit"
			}
			record, err := s.Create(ctx, "orders", group, subscriber)
			require.NoError(t, err)
			key := putReceipt(t, s, 1, record.Recipient)
			_, err = s.Connect(ctx, "orders", group, subscriber, "one")
			require.NoError(t, err)
			_, err = s.Connect(ctx, "orders", group, subscriber, "two")
			require.NoError(t, err)
			require.NoError(t, s.Disconnect(ctx, "one"))
			require.NoError(t, s.Maintain(ctx, time.Now().Add(2*time.Second)))
			current, err := raft.ReadSubscription(s.DB, "orders", group, subscriber)
			require.NoError(t, err)
			require.True(t, current.Active, "a connected idle worker prevents expiry")
			require.NoError(t, s.Disconnect(ctx, "two"))
			require.NoError(t, s.Maintain(ctx, time.Now().Add(2*time.Second)))
			current, err = raft.ReadSubscription(s.DB, "orders", group, subscriber)
			require.NoError(t, err)
			require.False(t, current.Active)
			_, _, err = s.DB.Get(key)
			require.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
}
func TestSubscriptionCrashLeaseAndExpiryRevalidation(t *testing.T) {
	s := newTestService(t, "pebble", time.Second)
	ctx := context.Background()
	old, err := s.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	_, err = s.Connect(ctx, "orders", "workers", "", "lost-broker")
	require.NoError(t, err)
	require.NoError(t, s.Maintain(ctx, time.Now().Add(SessionLease+2*time.Second)))
	current, err := raft.ReadSubscription(s.DB, "orders", "workers", "")
	require.NoError(t, err)
	require.False(t, current.Active, "crashed brokers need no graceful disconnect")
	newer, err := s.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	// A cleanup candidate selected before recreate must be conditional on generation.
	_, err = s.apply(ctx, &deliverypb.SubscriptionCommand{Operation: raft.SubscriptionExpire, NowMs: time.Now().Add(time.Hour).UnixMilli(), IdleTimeoutMs: 1, Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", GroupId: "workers", Recipient: old.Recipient}}})
	require.NoError(t, err)
	current, err = raft.ReadSubscription(s.DB, "orders", "workers", "")
	require.NoError(t, err)
	require.True(t, current.Active)
	require.Equal(t, newer.Recipient, current.Recipient)
}
func TestSubscriptionCleanupPagesAndReconnectRace(t *testing.T) {
	s := newTestService(t, "pebble", time.Second)
	ctx := context.Background()
	old, err := s.Create(ctx, "orders", "", "audit")
	require.NoError(t, err)
	for id := uint64(1); id <= 300; id++ {
		putReceipt(t, s, id, old.Recipient)
	}
	require.NoError(t, s.Unsubscribe(ctx, "orders", "", "audit"))
	require.NoError(t, s.Maintain(ctx, time.Now()))
	require.NoError(t, s.Maintain(ctx, time.Now()))
	require.NoError(t, s.DB.Scan(&storage.IterOptions{LowerBound: []byte("metadata/delivery/"), UpperBound: []byte("metadata/delivery0")}, func(_, _ []byte) error { t.Fatal("retired receipt survived bounded cleanup"); return nil }))
	current, err := s.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	_, err = s.Connect(ctx, "orders", "workers", "", "live")
	require.NoError(t, err)
	// Even a stale expiry candidate is revalidated against the connected session.
	_, err = s.apply(ctx, &deliverypb.SubscriptionCommand{Operation: raft.SubscriptionExpire, NowMs: time.Now().UnixMilli(), IdleTimeoutMs: 1, Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", GroupId: "workers", Recipient: current.Recipient}}})
	require.NoError(t, err)
	require.True(t, s.Alive(current.Recipient, "live"))
}
