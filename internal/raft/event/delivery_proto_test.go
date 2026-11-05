package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestProtoReceiptLegacyMigrationAndBitmap(t *testing.T) {
	legacy := []byte(`{"epoch":17,"recipients":{"u:old":false,"g:b":true,"g:a":false}}`)
	state, err := UnmarshalDeliveryState(legacy)
	require.NoError(t, err)
	require.Equal(t, []string{"g:a", "g:b", "u:old"}, state.Recipients)
	require.Equal(t, 2, state.remaining)
	require.False(t, state.acknowledge("g:b"))
	require.Equal(t, 2, state.remaining)
	require.True(t, state.acknowledge("g:a"))
	require.Equal(t, 1, state.remaining)
	data, err := marshalDeliveryState(state)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(data, receiptPrefix))
	decoded, err := UnmarshalDeliveryState(data)
	require.NoError(t, err)
	require.Equal(t, state, decoded)
	epoch, err := DeliveryStateEpoch(data)
	require.NoError(t, err)
	require.Equal(t, uint64(17), epoch)
	require.True(t, decoded.acknowledge("u:old"))
	require.True(t, decoded.complete())
	for _, p := range []*deliverypb.DeliveryReceipt{
		{Recipients: []string{"g:a"}},
		{Recipients: []string{"g:a"}, Acknowledged: []byte{0x80}},
		{Recipients: []string{"g:b", "g:a"}, Acknowledged: []byte{0}},
		{Recipients: []string{"g:a", "g:a"}, Acknowledged: []byte{0}},
	} {
		body, err := proto.Marshal(p)
		require.NoError(t, err)
		_, err = UnmarshalDeliveryState(append(append([]byte(nil), receiptPrefix...), body...))
		require.Error(t, err)
	}
}

func TestProtoPreparationSharedViewsAndExactSize(t *testing.T) {
	for _, count := range []int{1, 2, 4, 127, 128, 256} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			names := []string{"g:a", "g:فارسی", "u:<>&", "g:\xff\xfe"}
			items := make([]DeliveryPrepare, count)
			var size DeliveryPrepareSizer
			for i := range items {
				items[i] = DeliveryPrepare{Key: utils.EventKey(1, 2, uint64(i)), Epoch: 128, Recipients: names}
				size.Add(items[i])
			}
			cmd, err := MarshalPrepareDeliveryBatchCmd(items)
			require.NoError(t, err)
			require.Equal(t, len(cmd), size.size)
			var p deliverypb.DeliveryPrepareBatch
			require.NoError(t, proto.Unmarshal(cmd[1:], &p))
			require.Len(t, p.Groups, 1)
			require.Len(t, p.Groups[0].Keys, count)
			decoded, err := UnmarshalPrepareDeliveryBatchCmd(cmd)
			require.NoError(t, err)
			require.Len(t, decoded, count)
			for i, item := range decoded {
				require.Equal(t, items[i].Key, item.Key)
				require.Equal(t, normalizedRecipients(names), item.Recipients)
			}
		})
	}
}

func TestProtoPreparationRejectsBeforeMutation(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 1)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}})
	require.NoError(t, err)
	for _, groups := range [][]*deliverypb.DeliveryPrepareGroup{
		{{Recipients: []string{"g:a"}, Keys: [][]byte{key, key}}},
		{{Recipients: []string{"g:a"}, Keys: [][]byte{key}}, {Recipients: []string{"g:b"}, Keys: [][]byte{key}}},
		{{Recipients: []string{"g:a"}, Keys: [][]byte{key, []byte("short")}}},
		{{Recipients: []string{"bad"}, Keys: [][]byte{key}}},
		{{Recipients: []string{"g:a"}, Keys: make([][]byte, MaxDeliveryPrepareItems+1)}},
	} {
		data, err := proto.Marshal(&deliverypb.DeliveryPrepareBatch{Groups: groups})
		require.NoError(t, err)
		_, err = sm.Update([]statemachine.Entry{{Index: 2, Cmd: append([]byte{byte(ProtoPrepareDeliveryBatchCmd)}, data...)}})
		require.Error(t, err)
		state, err := ReadDeliveryState(db, key)
		require.NoError(t, err)
		require.Nil(t, state)
		require.Equal(t, uint64(1), sm.lastApplied)
	}
}

func TestLegacyJSONCommandsAndResultsRemainReplayable(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 1)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	body, err := json.Marshal([]DeliveryPrepare{{Key: key, Epoch: 1, Recipients: []string{"g:a", "g:b"}}})
	require.NoError(t, err)
	prepare := append([]byte{byte(PrepareDeliveryBatchCmd)}, body...)
	ackBody, err := json.Marshal([]DeliveryAck{{Key: key, Recipient: "g:a"}})
	require.NoError(t, err)
	entries, err := sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}, {Index: 3, Cmd: append([]byte{byte(AckDeliveryBatchCmd)}, ackBody...)}})
	require.NoError(t, err)
	require.Equal(t, byte('['), entries[1].Result.Data[0], "legacy log entries retain JSON results")
	var oldResults []DeliveryPrepared
	require.NoError(t, json.Unmarshal(entries[1].Result.Data, &oldResults))
	require.True(t, oldResults[0].State.Needs("g:a"))
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.False(t, state.Needs("g:a"))
	require.True(t, state.Needs("g:b"))
	data, closer, err := db.Get(DeliveryStateKey(key))
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(data, receiptPrefix))
	require.NoError(t, closer.Close())
}

func FuzzProtoPreparationDecode(f *testing.F) {
	valid, _ := MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{{Key: utils.EventKey(1, 2, 1), Recipients: []string{"g:a"}}})
	f.Add(valid)
	f.Add([]byte{byte(ProtoPrepareDeliveryBatchCmd), 0x0a, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || CommandType(data[0]) != ProtoPrepareDeliveryBatchCmd {
			return
		}
		items, err := UnmarshalPrepareDeliveryBatchCmd(data)
		if err == nil {
			require.NoError(t, validatePrepares(items))
			require.LessOrEqual(t, len(data), MaxDeliveryPrepareBytes)
		}
	})
}

func TestProtoPreparationRejectsExcessFieldsBeforeDecode(t *testing.T) {
	group := protowire.AppendTag(nil, 3, protowire.BytesType)
	group = protowire.AppendBytes(group, make([]byte, 24))
	group = bytes.Repeat(group, MaxDeliveryPrepareItems+1)
	data := protowire.AppendTag(nil, 1, protowire.BytesType)
	data = protowire.AppendBytes(data, group)
	_, err := UnmarshalPrepareDeliveryBatchCmd(append([]byte{byte(ProtoPrepareDeliveryBatchCmd)}, data...))
	require.Error(t, err)
}

func FuzzStoredReceiptDecode(f *testing.F) {
	state := newDeliveryState(17, []string{"g:a", "g:b", "u:old"})
	state.acknowledge("g:b")
	seed, _ := proto.Marshal(receiptProto(state))
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		var p deliverypb.DeliveryReceipt
		referenceErr := deliveryUnmarshal.Unmarshal(data, &p)
		var reference *DeliveryState
		if referenceErr == nil {
			reference, referenceErr = receiptState(&p)
		}
		ownedInput := append([]byte(nil), data...)
		got, err := decodeStoredReceipt(ownedInput)
		require.Equal(t, referenceErr == nil, err == nil)
		if err != nil {
			return
		}
		require.Equal(t, reference.Epoch, got.Epoch)
		require.Equal(t, len(reference.Recipients), len(got.Recipients))
		for i := range reference.Recipients {
			require.Equal(t, reference.Recipients[i], got.Recipients[i])
		}
		require.Equal(t, reference.remaining, got.remaining)
		require.True(t, bytes.Equal(reference.Acknowledged, got.Acknowledged))
		for i := range ownedInput {
			ownedInput[i] = 0
		}
		for i := range reference.Recipients {
			require.Equal(t, reference.Recipients[i], got.Recipients[i])
		}
	})
}

func TestProtoACKCodecMatchesGeneratedTypes(t *testing.T) {
	acks := []DeliveryAck{{Key: utils.EventKey(1, 2, 3), Recipient: "g:a"}, {Recipient: "g:\xff\xfe"}, {}}
	cmd, err := MarshalAckDeliveryBatchCmd(acks)
	require.NoError(t, err)
	var p deliverypb.DeliveryAckBatch
	require.NoError(t, proto.Unmarshal(cmd[1:], &p))
	require.Len(t, p.Acks, len(acks))
	for i, a := range acks {
		require.Equal(t, a.Key, p.Acks[i].Key)
		require.Equal(t, deliveryRecipient(a.Recipient), p.Acks[i].Recipient)
	}
	wire, err := proto.Marshal(&p)
	require.NoError(t, err)
	require.Equal(t, wire, cmd[1:])
	got, err := unmarshalProtoAcks(wire)
	require.NoError(t, err)
	for i, a := range p.Acks {
		require.Equal(t, a.Key, got[i].Key)
		require.Equal(t, a.Recipient, got[i].Recipient)
	}
}
func FuzzProtoACKDecode(f *testing.F) {
	cmd, _ := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: utils.EventKey(1, 2, 3), Recipient: "g:a"}})
	f.Add(cmd[1:])
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		expected, referenceErr := unmarshalProtoAcksGeneral(data)
		got, err := unmarshalProtoAcks(data)
		require.Equal(t, referenceErr == nil, err == nil)
		if err != nil {
			return
		}
		require.Equal(t, len(expected), len(got))
		for i, a := range expected {
			require.True(t, bytes.Equal(a.Key, got[i].Key))
			require.Equal(t, a.Recipient, got[i].Recipient)
		}
	})
}
