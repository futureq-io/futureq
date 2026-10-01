package raft

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
)

func TestBatchPrepareOverlayAckDeleteEpochAndSnapshot(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	a, b, missing := utils.EventKey(1, 2, 1), utils.EventKey(1, 2, 2), utils.EventKey(1, 2, 3)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 2, Msg: []byte("a")}, {ID: 2, Bucket: 1, TopicHash: 2, Msg: []byte("b")}})
	require.NoError(t, err)
	prepare, err := MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{{Key: a, Epoch: 10, Recipients: []string{"g:a", "g:b", "u:old"}}, {Key: b, Epoch: 10, Recipients: []string{"g:a"}}, {Key: missing, Epoch: 10, Recipients: []string{"g:a"}}})
	require.NoError(t, err)
	ack, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: a, Recipient: "g:a"}, {Key: b, Recipient: "g:a"}})
	require.NoError(t, err)
	refresh, err := MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{{Key: a, Epoch: 11, Recipients: []string{"g:new"}}, {Key: b, Epoch: 11, Recipients: []string{"g:new"}}})
	require.NoError(t, err)
	entries, err := sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}, {Index: 3, Cmd: ack}, {Index: 4, Cmd: refresh}})
	require.NoError(t, err)
	var prepared []DeliveryPrepared
	require.NoError(t, json.Unmarshal(entries[3].Result.Data, &prepared))
	require.Len(t, prepared, 2)
	require.Equal(t, a, prepared[0].Key)
	require.Nil(t, prepared[1].State)
	state, err := ReadDeliveryState(db, a)
	require.NoError(t, err)
	require.Equal(t, uint64(11), state.Epoch)
	require.False(t, state.Needs("g:a"))
	require.True(t, state.Needs("g:b"))
	require.False(t, state.Needs("u:old"))
	require.False(t, state.Needs("g:new"))
	stale, err := MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{{Key: a, Epoch: 9, Recipients: []string{"g:a", "u:old"}}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 5, Cmd: stale}})
	require.NoError(t, err)
	ctx, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(ctx, &snapshot, nil))
	recovered, recoveredDB := newDeliveryTestSM(t)
	require.NoError(t, recovered.RecoverFromSnapshot(&snapshot, nil))
	state, err = ReadDeliveryState(recoveredDB, a)
	require.NoError(t, err)
	require.True(t, state.Needs("g:b"))
	require.False(t, state.Needs("g:a"))
	deleteCmd, err := MarshalDeleteBatchCmd([][]byte{a})
	require.NoError(t, err)
	_, err = recovered.Update([]statemachine.Entry{{Index: 6, Cmd: deleteCmd}, {Index: 7, Cmd: prepare}})
	require.NoError(t, err)
	state, err = ReadDeliveryState(recoveredDB, a)
	require.NoError(t, err)
	require.Nil(t, state, "deleted payload cannot regain a manifest")
}

func TestBatchPrepareRejectsMalformedBeforeMutating(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 1)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 2, Msg: []byte("a")}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}})
	require.NoError(t, err)
	valid := DeliveryPrepare{Key: key, Epoch: 1, Recipients: []string{"g:a"}}
	for _, items := range [][]DeliveryPrepare{{valid, valid}, {valid, {Key: []byte("short"), Recipients: []string{"g:a"}}}, {valid, {Key: utils.EventKey(1, 2, 2), Recipients: []string{"bad"}}}} {
		data, err := json.Marshal(items)
		require.NoError(t, err)
		batch := db.NewBatch()
		_, _, err = NewDeliveryBatch(db, batch).Apply(append([]byte{byte(PrepareDeliveryBatchCmd)}, data...))
		require.Error(t, err)
		require.NoError(t, batch.Commit(storage.Sync))
		require.NoError(t, batch.Close())
		state, err := ReadDeliveryState(db, key)
		require.NoError(t, err)
		require.Nil(t, state, "even earlier valid items must not mutate on malformed input")
	}
	items := make([]DeliveryPrepare, MaxDeliveryPrepareItems+1)
	data, _ := json.Marshal(items)
	_, err = UnmarshalPrepareDeliveryBatchCmd(append([]byte{byte(PrepareDeliveryBatchCmd)}, data...))
	require.Error(t, err)
	_, err = UnmarshalPrepareDeliveryBatchCmd(make([]byte, MaxDeliveryPrepareBytes+1))
	require.Error(t, err)
	cmd, err := MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{valid})
	require.NoError(t, err)
	_, err = UnmarshalPrepareDeliveryBatchCmd(append(cmd, []byte(" {}")...))
	require.Error(t, err)
	require.Equal(t, CommandType(0), StoreBatchCmd)
	require.Equal(t, CommandType(3), AckDeliveryBatchCmd)
	require.Equal(t, CommandType(4), PrepareDeliveryBatchCmd)
}
