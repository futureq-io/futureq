package raft

import (
	"bytes"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newDeliveryTestSM(t *testing.T) (*EventStateMachine, storage.DB) {
	t.Helper()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := repository.NewEventRepository(db, zap.NewNop(), time.Second)
	require.NoError(t, err)
	sm := NewEventStateMachineFactory(db, repo, nil, zap.NewNop())(1, 1).(*EventStateMachine)
	return sm, db
}

func TestDeliveryReceiptsSurviveSnapshotAndAckBatch(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 3)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	prepare, err := MarshalPrepareDeliveryCmd(key, 1, []string{"g:a", "g:b", "u:first", "u:second"})
	require.NoError(t, err)
	ack, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:a"}, {Key: key, Recipient: "unknown"}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}, {Index: 3, Cmd: ack}})
	require.NoError(t, err, "prepare sees a store from the same Update")
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.False(t, state.Needs("g:a"))
	require.True(t, state.Needs("g:b"))
	ctx, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(ctx, &snapshot, nil))
	recovered, recoveredDB := newDeliveryTestSM(t)
	require.NoError(t, recovered.RecoverFromSnapshot(&snapshot, nil))
	state, err = ReadDeliveryState(recoveredDB, key)
	require.NoError(t, err)
	require.False(t, state.Needs("g:a"))
	require.True(t, state.Needs("g:b"))
	ackB, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:b"}})
	require.NoError(t, err)
	ackU, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "u:first"}, {Key: key, Recipient: "u:second"}, {Key: key, Recipient: "u:second"}})
	require.NoError(t, err)
	var deleted [][]byte
	recovered.OnDeleteKeys = func(keys [][]byte) { deleted = append(deleted, keys...) }
	_, err = recovered.Update([]statemachine.Entry{{Index: 4, Cmd: ackB}, {Index: 5, Cmd: ackU}})
	require.NoError(t, err, "ACK entries in one Update must share the pending state")
	require.Equal(t, [][]byte{key}, deleted)
	_, _, err = recoveredDB.Get(key)
	require.ErrorIs(t, err, storage.ErrNotFound)
	state, err = ReadDeliveryState(recoveredDB, key)
	require.NoError(t, err)
	require.Nil(t, state)
	_, err = recovered.Update([]statemachine.Entry{{Index: 6, Cmd: prepare}})
	require.NoError(t, err)
	state, err = ReadDeliveryState(recoveredDB, key)
	require.NoError(t, err)
	require.Nil(t, state, "a late snapshot scan cannot recreate receipts for a deleted payload")
}

func TestDeliveryManifestFreezesGroupsAndReleasesDisconnectedUniversal(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 3)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	prepare, err := MarshalPrepareDeliveryCmd(key, 10, []string{"g:offline", "u:disconnected"})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}})
	require.NoError(t, err)
	newEpoch, err := MarshalPrepareDeliveryCmd(key, 11, []string{"g:new", "u:new"})
	require.NoError(t, err)
	staleEpoch, err := MarshalPrepareDeliveryCmd(key, 9, nil)
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 3, Cmd: newEpoch}, {Index: 4, Cmd: staleEpoch}})
	require.NoError(t, err)
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.True(t, state.Needs("g:offline"), "a disconnected named group retains its incomplete delivery")
	require.False(t, state.Needs("u:disconnected"))
	require.False(t, state.Needs("g:new"), "new interests do not join an already prepared delivery")
	require.Equal(t, uint64(11), state.Epoch)
	forceDelete, err := MarshalDeleteBatchCmd([][]byte{key})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 5, Cmd: forceDelete}, {Index: 6, Cmd: prepare}})
	require.NoError(t, err)
	state, err = ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.Nil(t, state, "explicit deletes/TTL override receipt retention atomically")
}
