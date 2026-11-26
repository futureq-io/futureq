package raft

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/futureq-io/stormoar/v4/statemachine"
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
			exists, err := TopicExists(other, "orders")
			require.NoError(t, err)
			require.True(t, exists, "snapshot retains the topic catalog")
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
	require.Equal(t, PublishTopicNotFound, rejection.Reason)
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
			cmd, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: TopicCreate, Topic: "orders", NowMs: 1000, Mutations: mutations})
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
func TestPublishGroupBudgetsExpandedReceiptsAndRejectsWholeClientBatch(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	createA, err := testSubscriptionCommand("small", "a", "g:s:a")
	require.NoError(t, err)
	applyAdmissionSequence(t, sm, createA)
	var mutations []*deliverypb.SubscriptionMutation
	var names []string
	for i := range MaxDeliveryRecipients {
		name := fmt.Sprintf("g:s:worker-%04d", i)
		names = append(names, name)
		mutations = append(mutations, &deliverypb.SubscriptionMutation{Topic: "orders", GroupId: fmt.Sprint(i), Recipient: name})
		if len(mutations) == 256 {
			cmd, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{Operation: TopicCreate, Topic: "orders", NowMs: 1000, Mutations: mutations})
			require.NoError(t, err)
			applyAdmissionSequence(t, sm, cmd)
			mutations = nil
		}
	}

	makeFrame := func(id uint64, topics ...string) []StoreBatchItem {
		items, err := UnmarshalPublishBatchCmd(admissionCommand(t, id, topics...))
		require.NoError(t, err)
		return items
	}
	first := makeFrame(1, "small")
	oversized := makeFrame(100, "orders", "orders")
	oversized[0].Indexes = [][]byte{[]byte("reject-index")}
	last := makeFrame(2, "orders")
	var frames [][]byte
	for _, items := range [][]StoreBatchItem{first, oversized, last} {
		frame, err := MarshalPublishFrame(items, false)
		require.NoError(t, err)
		frames = append(frames, frame)
	}
	first[0].Receipt, err = MarshalPublishReceipt(0, []string{"g:s:a"})
	require.NoError(t, err)
	last[0].Receipt, err = MarshalPublishReceipt(0, names)
	require.NoError(t, err)
	budget := PublishItemStorageBytes(first[0]) + PublishItemStorageBytes(last[0])
	cmd, err := MarshalPublishGroupCmd(frames, false, budget)
	require.NoError(t, err)
	require.Less(t, len(cmd), budget/10, "encoded size must not predict receipt expansion")
	results := applyAdmissionSequence(t, sm, cmd)
	individual, err := UnmarshalPublishGroupResults(results[0].Result, 3)
	require.NoError(t, err)
	require.NoError(t, CheckPublishResult(individual[0], 1))
	var rejection *PublishRejectedError
	require.ErrorAs(t, CheckPublishResult(individual[1], 2), &rejection)
	require.Equal(t, PublishStorageBudgetExceeded, rejection.Reason)
	require.NoError(t, CheckPublishResult(individual[2], 1))
	require.Equal(t, uint64(2), sm.lastPersistedID)
	for _, id := range []uint64{100, 101} {
		key := utils.EventKey(1, utils.TopicHash("orders"), id)
		_, _, err := db.Get(key)
		require.ErrorIs(t, err, storage.ErrNotFound)
		_, _, err = db.Get(DeliveryStateKey(key))
		require.ErrorIs(t, err, storage.ErrNotFound)
	}
	_, _, err = db.Get([]byte("reject-index"))
	require.ErrorIs(t, err, storage.ErrNotFound)
	state, err := ReadDeliveryState(db, utils.EventKey(1, utils.TopicHash("orders"), 2))
	require.NoError(t, err)
	require.Equal(t, names, state.Recipients)
}

func TestPublishTransactionsBoundDeclaredBudgetsAcrossEntries(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	create, err := testSubscriptionCommand("orders", "a", "g:s:a")
	require.NoError(t, err)
	wrapped := &publishTransactionDB{DB: db}
	sm.db = wrapped
	var cmds [][]byte
	for i := range 3 {
		items, err := UnmarshalPublishBatchCmd(admissionCommand(t, uint64(i+1), "orders"))
		require.NoError(t, err)
		frame, err := MarshalPublishFrame(items, false)
		require.NoError(t, err)
		command, err := MarshalPublishGroupCmd([][]byte{frame}, false, 8<<20)
		require.NoError(t, err)
		cmds = append(cmds, command)
	}
	results := applyAdmissionSequence(t, sm, append([][]byte{create}, cmds...)...)
	require.Equal(t, []uint64{1, 3, 4}, wrapped.committedIndexes)
	for _, result := range results[1:] {
		parts, err := UnmarshalPublishGroupResults(result.Result, 1)
		require.NoError(t, err)
		require.NoError(t, CheckPublishResult(parts[0], 1))
	}
}

type publishTransactionDB struct {
	storage.DB
	committedIndexes []uint64
}

func (db *publishTransactionDB) NewBatch() storage.Batch {
	return &publishTransactionBatch{Batch: db.DB.NewBatch(), db: db}
}

type publishTransactionBatch struct {
	storage.Batch
	db    *publishTransactionDB
	index uint64
}

func (b *publishTransactionBatch) Set(key, value []byte) error {
	if bytes.Equal(key, appliedIndexKey) {
		b.index = binary.BigEndian.Uint64(value)
	}
	return b.Batch.Set(key, value)
}

func (b *publishTransactionBatch) Commit(mode storage.SyncMode) error {
	if err := b.Batch.Commit(mode); err != nil {
		return err
	}
	b.db.committedIndexes = append(b.db.committedIndexes, b.index)
	return nil
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
