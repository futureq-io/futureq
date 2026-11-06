package raft

import (
	"bytes"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSubscriptionCommandsOverlayAndSnapshot(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	first, err := testSubscriptionCommand("orders", "workers", "g:s:old")
	require.NoError(t, err)
	remove, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: SubscriptionRemove, NowMs: 1001, Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", GroupId: "workers"}}})
	require.NoError(t, err)
	second, err := testSubscriptionCommand("orders", "workers", "g:s:new")
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: first}, {Index: 2, Cmd: remove}, {Index: 3, Cmd: second}})
	require.NoError(t, err)
	current, err := ReadSubscription(db, "orders", "workers", "")
	require.NoError(t, err)
	require.True(t, current.Active)
	require.Equal(t, "g:s:new", current.Recipient)
	old, err := ReadSubscriptionRecipient(db, "g:s:old")
	require.NoError(t, err)
	require.False(t, old.Active)
	frozen, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(frozen, &snapshot, nil))
	recovered, other := newDeliveryTestSM(t)
	require.NoError(t, recovered.RecoverFromSnapshot(&snapshot, nil))
	current, err = ReadSubscription(other, "orders", "workers", "")
	require.NoError(t, err)
	require.Equal(t, "g:s:new", current.Recipient)
	old, err = ReadSubscriptionRecipient(other, "g:s:old")
	require.NoError(t, err)
	require.False(t, old.Active)
}

func TestSubscriptionDisconnectDoesNotBackdateIdleClock(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	create, err := testSubscriptionCommand("orders", "workers", "g:s:workers")
	require.NoError(t, err)
	entries := []statemachine.Entry{{Index: 1, Cmd: create}}
	for i, item := range []struct {
		op      uint32
		now     int64
		session string
		lease   int64
	}{{SubscriptionConnect, 1000, "expired", 10}, {SubscriptionConnect, 1000, "live", 100}, {SubscriptionDisconnect, 1050, "live", 0}, {SubscriptionExpire, 1055, "", 0}} {
		cmd, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: item.op, NowMs: item.now, LeaseMs: item.lease, IdleTimeoutMs: 20, Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", GroupId: "workers", Recipient: "g:s:workers", Session: item.session}}})
		require.NoError(t, err)
		entries = append(entries, statemachine.Entry{Index: uint64(i + 2), Cmd: cmd})
	}
	_, err = sm.Update(entries)
	require.NoError(t, err)
	record, err := ReadSubscription(db, "orders", "workers", "")
	require.NoError(t, err)
	require.True(t, record.Active, "idle clock starts when the last live worker disconnects, not the older expired lease")
	require.Equal(t, int64(1050), record.DisconnectedSinceMs)
}

func TestSubscriptionRetirementAndOldAckInOneRaftUpdate(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	oldCreate, err := testSubscriptionCommand("orders", "workers", "g:s:old")
	require.NoError(t, err)
	newCreate, err := testSubscriptionCommand("orders", "workers", "g:s:new")
	require.NoError(t, err)
	oldReceipt, err := MarshalPublishReceipt(0, []string{"g:s:old"})
	require.NoError(t, err)
	newReceipt, err := MarshalPublishReceipt(0, []string{"g:s:new"})
	require.NoError(t, err)
	oldStore, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: 10, Bucket: 1, TopicHash: 2, Msg: []byte("old"), Receipt: oldReceipt}})
	require.NoError(t, err)
	newStore, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: 11, Bucket: 1, TopicHash: 2, Msg: []byte("new"), Receipt: newReceipt}})
	require.NoError(t, err)
	oldKey, newKey := utils.EventKey(1, 2, 10), utils.EventKey(1, 2, 11)
	remove, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: SubscriptionRemove, NowMs: 1001, Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", GroupId: "workers"}}})
	require.NoError(t, err)
	lateAck, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: newKey, Recipient: "g:s:old"}})
	require.NoError(t, err)
	cleanup, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: SubscriptionCleanup, NowMs: 1002, Keys: [][]byte{oldKey, newKey}})
	require.NoError(t, err)
	commands := [][]byte{oldCreate, oldStore, remove, newCreate, newStore, lateAck, cleanup}
	entries := make([]statemachine.Entry, len(commands))
	for i, cmd := range commands {
		entries[i] = statemachine.Entry{Index: uint64(i + 1), Cmd: cmd}
	}
	_, err = sm.Update(entries)
	require.NoError(t, err)
	_, _, err = db.Get(oldKey)
	require.ErrorIs(t, err, storage.ErrNotFound)
	state, err := ReadDeliveryState(db, newKey)
	require.NoError(t, err)
	require.True(t, state.Needs("g:s:new"))
}
