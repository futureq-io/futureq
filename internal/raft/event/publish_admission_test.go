package raft

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func admissionCommand(t *testing.T, id uint64, topics ...string) []byte {
	t.Helper()
	items := make([]StoreBatchItem, len(topics))
	for i, topic := range topics {
		msg, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, Payload: []byte("payload"), EnqueuedAtUnixMs: 1000})
		require.NoError(t, err)
		items[i] = StoreBatchItem{ID: id + uint64(i), Bucket: 1, TopicHash: utils.TopicHash(topic), Msg: msg}
	}
	cmd, err := MarshalPublishBatchCmd(items)
	require.NoError(t, err)
	return cmd
}
func removeInterest(t *testing.T, topic, group, subscriber string) []byte {
	t.Helper()
	cmd, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: SubscriptionRemove, NowMs: 2000, Mutations: []*deliverypb.SubscriptionMutation{{Topic: topic, GroupId: group, SubscriberId: subscriber}}})
	require.NoError(t, err)
	return cmd
}
func applyAdmissionSequence(t *testing.T, sm *EventStateMachine, commands ...[]byte) []statemachine.Entry {
	t.Helper()
	entries := make([]statemachine.Entry, len(commands))
	for i, cmd := range commands {
		entries[i] = statemachine.Entry{Index: sm.lastApplied + uint64(i+1), Cmd: cmd}
	}
	result, err := sm.Update(entries)
	require.NoError(t, err)
	return result
}
func TestQuorumAdmissionSeesOrderedSubscriptionsInSameApplyBatch(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(fmt.Sprint(persisted), func(t *testing.T) {
			sm, db := newDeliveryTestSM(t)
			createA, err := testSubscriptionCommand("orders", "a", "g:s:a")
			require.NoError(t, err)
			createB, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: SubscriptionCreate, NowMs: 1000, Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", SubscriberId: "audit", Recipient: "g:s:b"}}})
			require.NoError(t, err)
			recreateA, err := testSubscriptionCommand("orders", "a", "g:s:a-new")
			require.NoError(t, err)
			commands := [][]byte{}
			if persisted {
				applyAdmissionSequence(t, sm, createA)
			} else {
				commands = append(commands, createA)
			}
			commands = append(commands, admissionCommand(t, 1, "orders"), createB, admissionCommand(t, 2, "orders"), removeInterest(t, "orders", "a", ""), admissionCommand(t, 3, "orders"), removeInterest(t, "orders", "", "audit"), admissionCommand(t, 4, "orders"), recreateA, admissionCommand(t, 5, "orders"))
			results := applyAdmissionSequence(t, sm, commands...)
			for _, test := range []struct {
				id    uint64
				names []string
			}{{1, []string{"g:s:a"}}, {2, []string{"g:s:a", "g:s:b"}}, {3, []string{"g:s:b"}}, {5, []string{"g:s:a-new"}}} {
				state, err := ReadDeliveryState(db, utils.EventKey(1, utils.TopicHash("orders"), test.id))
				require.NoError(t, err)
				require.NotNil(t, state)
				require.Equal(t, test.names, state.Recipients)
				require.True(t, state.PublishBound)
			}
			rejectedIndex := len(results) - 3
			var rejection *PublishRejectedError
			require.ErrorAs(t, CheckPublishResult(results[rejectedIndex].Result, 1), &rejection)
			require.Equal(t, PublishNoDestinations, rejection.Reason)
			_, _, err = db.Get(utils.EventKey(1, utils.TopicHash("orders"), 4))
			require.ErrorIs(t, err, storage.ErrNotFound)
			require.NoError(t, CheckPublishResult(results[len(results)-1].Result, 1), "rejection must not stop later commands")
			frozen, err := sm.PrepareSnapshot()
			require.NoError(t, err)
			var snapshot bytes.Buffer
			require.NoError(t, sm.SaveSnapshot(frozen, &snapshot, nil))
			recovered, other := newDeliveryTestSM(t)
			require.NoError(t, recovered.RecoverFromSnapshot(&snapshot, nil))
			state, err := ReadDeliveryState(other, utils.EventKey(1, utils.TopicHash("orders"), 1))
			require.NoError(t, err)
			require.Equal(t, []string{"g:s:a"}, state.Recipients, "snapshot retains original destinations")
		})
	}
}
func TestQuorumAdmissionRejectsWholeBatchWithoutRevertingOtherEntries(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	createA, err := testSubscriptionCommand("orders", "a", "g:s:a")
	require.NoError(t, err)
	createB, err := testSubscriptionCommand("missing", "b", "g:s:b")
	require.NoError(t, err)
	result := applyAdmissionSequence(t, sm, createA, admissionCommand(t, 10, "orders", "missing"), createB, admissionCommand(t, 20, "orders", "missing"))
	var rejection *PublishRejectedError
	require.ErrorAs(t, CheckPublishResult(result[1].Result, 2), &rejection)
	require.Equal(t, "missing", rejection.Topic)
	require.NoError(t, CheckPublishResult(result[3].Result, 2))
	for i, topic := range []string{"orders", "missing"} {
		_, _, err := db.Get(utils.EventKey(1, utils.TopicHash(topic), 10+uint64(i)))
		require.ErrorIs(t, err, storage.ErrNotFound)
		_, _, err = db.Get(DeliveryStateKey(utils.EventKey(1, utils.TopicHash(topic), 10+uint64(i))))
		require.ErrorIs(t, err, storage.ErrNotFound)
		state, err := ReadDeliveryState(db, utils.EventKey(1, utils.TopicHash(topic), 20+uint64(i)))
		require.NoError(t, err)
		require.NotNil(t, state)
	}
	require.Equal(t, uint64(4), sm.lastApplied)
	require.Equal(t, uint64(21), sm.lastPersistedID)
}
func TestQuorumAdmissionCapacityRejectionDoesNotHaltStateMachine(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	var mutations []*deliverypb.SubscriptionMutation
	for i := 0; i <= MaxDeliveryRecipients; i++ {
		mutations = append(mutations, &deliverypb.SubscriptionMutation{Topic: "orders", GroupId: fmt.Sprint(i), Recipient: fmt.Sprintf("g:s:%04d", i)})
		if len(mutations) == 256 || i == MaxDeliveryRecipients {
			cmd, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: SubscriptionCreate, NowMs: 1000, Mutations: mutations})
			require.NoError(t, err)
			applyAdmissionSequence(t, sm, cmd)
			mutations = nil
		}
	}
	result := applyAdmissionSequence(t, sm, admissionCommand(t, 1, "orders"))
	var rejection *PublishRejectedError
	require.ErrorAs(t, CheckPublishResult(result[0].Result, 1), &rejection)
	require.Equal(t, PublishTooManyDestinations, rejection.Reason)
	_, _, err := db.Get(utils.EventKey(1, utils.TopicHash("orders"), 1))
	require.ErrorIs(t, err, storage.ErrNotFound)
	result = applyAdmissionSequence(t, sm, removeInterest(t, "orders", "0", ""), admissionCommand(t, 2, "orders"))
	require.NoError(t, CheckPublishResult(result[1].Result, 1))
	state, err := ReadDeliveryState(db, utils.EventKey(1, utils.TopicHash("orders"), 2))
	require.NoError(t, err)
	require.Len(t, state.Recipients, MaxDeliveryRecipients)
}
func TestQuorumAdmissionCodecRejectsMalformedAndPreservesInput(t *testing.T) {
	cmd := admissionCommand(t, 1, "orders")
	original := append([]byte(nil), cmd...)
	for n := 0; n < len(cmd); n++ {
		_, err := UnmarshalPublishBatchCmd(cmd[:n])
		require.Error(t, err, "prefix %d", n)
	}
	_, err := UnmarshalPublishBatchCmd(append(append([]byte(nil), cmd...), 0))
	require.Error(t, err)
	items, err := UnmarshalPublishBatchCmd(cmd)
	require.NoError(t, err)
	require.Equal(t, original, cmd)
	require.Empty(t, items[0].Receipt)
	_, err = MarshalPublishBatchCmd(nil)
	require.Error(t, err)
	items[0].Receipt = []byte("preselected")
	_, err = MarshalPublishBatchCmd(items)
	require.Error(t, err)
	require.Error(t, CheckPublishResult(statemachine.Result{Value: 1}, 2))
	require.Error(t, CheckPublishResult(statemachine.Result{Data: []byte{99, 'x'}}, 1))
	sm, _ := newDeliveryTestSM(t)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: cmd[:len(cmd)-1]}})
	require.Error(t, err)
	require.Zero(t, sm.lastApplied)
}
