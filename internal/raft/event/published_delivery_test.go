package raft

import (
	"bytes"
	"testing"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
)

func TestPublishedDeliveryAtomicApplySnapshotAndAckIsolation(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	groupA, err := testSubscriptionCommand("orders", "a", "g:s:a")
	require.NoError(t, err)
	groupB, err := testSubscriptionCommand("orders", "b", "g:s:b")
	require.NoError(t, err)
	receipt, err := MarshalPublishReceipt(1, []string{"g:s:a", "g:s:b"})
	require.NoError(t, err)
	store, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload"), Receipt: receipt}})
	require.NoError(t, err)
	key := utils.EventKey(1, 2, 3)
	ack, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:s:a"}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: groupA}, {Index: 2, Cmd: groupB}, {Index: 3, Cmd: store}, {Index: 4, Cmd: ack}})
	require.NoError(t, err)
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.True(t, state.PublishBound)
	require.False(t, state.Needs("g:s:a"))
	require.True(t, state.Needs("g:s:b"))
	groups, err := ReadSubscriptionRecipients(db, "orders")
	require.NoError(t, err)
	require.Equal(t, []string{"g:s:a", "g:s:b"}, groups)

	ctx, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(ctx, &snapshot, nil))
	recovered, restoredDB := newDeliveryTestSM(t)
	require.NoError(t, recovered.RecoverFromSnapshot(&snapshot, nil))
	restored, err := ReadDeliveryState(restoredDB, key)
	require.NoError(t, err)
	require.Equal(t, state, restored)
	groups, err = ReadSubscriptionRecipients(restoredDB, "orders")
	require.NoError(t, err)
	require.Equal(t, []string{"g:s:a", "g:s:b"}, groups)

	// Malformed receipts abort all mutations, including earlier entries.
	_, err = sm.Update([]statemachine.Entry{{Index: 5, Cmd: groupA}, {Index: 6, Cmd: store[:len(store)-1]}})
	require.Error(t, err)
	require.Equal(t, uint64(4), sm.lastApplied)
}

func TestPublishCommandRejectsEveryTruncatedPrefix(t *testing.T) {
	receipt, err := MarshalPublishReceipt(0, []string{"g:s:a"})
	require.NoError(t, err)
	cmd, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 2, TopicHash: 3, Msg: []byte("payload"), Receipt: receipt}})
	require.NoError(t, err)
	for n := 0; n < len(cmd); n++ {
		_, err := UnmarshalStoreDeliveryBatchCmd(cmd[:n])
		require.Error(t, err, "prefix=%d", n)
	}
	_, err = UnmarshalStoreDeliveryBatchCmd(append(cmd, 0))
	require.Error(t, err)
	items, err := UnmarshalStoreDeliveryBatchCmd(cmd)
	require.NoError(t, err)
	require.Equal(t, receipt, items[0].Receipt)
	_, err = MarshalPublishReceipt(0, nil)
	require.Error(t, err)
	_, err = MarshalPublishReceipt(0, []string{"g:s:a", "g:s:a"})
	require.Error(t, err)
}

func TestDurableGroupKeyIsolation(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	for i, pair := range [][2]string{{"a", "b|c"}, {"a|b", "c"}, {"a", "workers"}} {
		cmd, err := testSubscriptionCommand(pair[0], pair[1], "g:s:"+pair[0]+":"+pair[1])
		require.NoError(t, err)
		_, err = sm.Update([]statemachine.Entry{{Index: uint64(i + 1), Cmd: cmd}})
		require.NoError(t, err)
	}
	groups, err := ReadSubscriptionRecipients(db, "a")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"g:s:a:b|c", "g:s:a:workers"}, groups)
	groups, err = ReadSubscriptionRecipients(db, "a|b")
	require.NoError(t, err)
	require.Equal(t, []string{"g:s:a|b:c"}, groups)
	groups, err = ReadSubscriptionRecipients(db, "missing")
	require.NoError(t, err)
	require.Empty(t, groups)
	_, _, err = db.Get([]byte("missing"))
	require.ErrorIs(t, err, storage.ErrNotFound)
}
func testSubscriptionCommand(topic, group, recipient string) ([]byte, error) {
	return MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: SubscriptionCreate, NowMs: 1000, Mutations: []*deliverypb.SubscriptionMutation{{Topic: topic, GroupId: group, Recipient: recipient}}})
}
