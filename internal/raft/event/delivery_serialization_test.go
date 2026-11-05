package raft

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
)

func TestDeliveryPrepareSizeMatchesJSON(t *testing.T) {
	names := []string{"", "g:plain", "u:\"\\\b\f\n\r\t", "g:<>&", "g:\x00\x01\x1f", "g:فارسی日本語😀", "g:\u2028\u2029", "g:\xff\xfe", "g:\ufffd"}
	for _, key := range [][]byte{nil, {}, make([]byte, 1), make([]byte, 23), make([]byte, 24), make([]byte, 25)} {
		for _, epoch := range []uint64{0, 9, 10, 99, 100, math.MaxUint64} {
			for _, recipients := range [][]string{nil, {}, names} {
				item := DeliveryPrepare{Key: key, Epoch: epoch, Recipients: recipients}
				encoded, err := json.Marshal(item)
				require.NoError(t, err)
				require.Equal(t, len(encoded), DeliveryPrepareSize(item), "%s", encoded)
			}
		}
	}
	// Exercise every byte, including all short escapes and invalid UTF-8.
	for i := 0; i < 256; i++ {
		item := DeliveryPrepare{Key: make([]byte, 24), Recipients: []string{"g:" + string([]byte{byte(i)})}}
		encoded, err := json.Marshal(item)
		require.NoError(t, err)
		require.Equal(t, len(encoded), DeliveryPrepareSize(item))
	}
}

func FuzzDeliveryPrepareSize(f *testing.F) {
	f.Add([]byte("key"), uint64(1), "g:<>&\"\n\u2028\xff")
	f.Add([]byte{}, uint64(math.MaxUint64), "")
	f.Fuzz(func(t *testing.T, key []byte, epoch uint64, name string) {
		item := DeliveryPrepare{Key: key, Epoch: epoch, Recipients: []string{name}}
		data, err := json.Marshal(item)
		require.NoError(t, err)
		require.Equal(t, len(data), DeliveryPrepareSize(item))
	})
}

func TestDeliveryPrepareValidationExactByteBoundary(t *testing.T) {
	item := DeliveryPrepare{Key: make([]byte, 24), Epoch: math.MaxUint64, Recipients: []string{"g:x"}}
	padding := MaxDeliveryPrepareBytes - 3 - DeliveryPrepareSize(item)
	item.Recipients[0] += strings.Repeat("x", padding)
	require.NoError(t, ValidateDeliveryPrepares([]DeliveryPrepare{item}))
	cmd, err := MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{item})
	require.NoError(t, err)
	require.Len(t, cmd, MaxDeliveryPrepareBytes)
	item.Recipients[0] += "x"
	require.Error(t, ValidateDeliveryPrepares([]DeliveryPrepare{item}))
	_, err = MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{item})
	require.Error(t, err)
	item.Recipients = []string{"g:\xff", "g:\xfe"}
	require.Error(t, ValidateDeliveryPrepares([]DeliveryPrepare{item}), "normalization cannot introduce duplicate recipients")
}

type receiptWriteDB struct {
	storage.DB
	sets    map[string]int
	fail    error
	commits int
}

func (db *receiptWriteDB) NewBatch() storage.Batch {
	return &receiptWriteBatch{Batch: db.DB.NewBatch(), db: db}
}

type receiptWriteBatch struct {
	storage.Batch
	db *receiptWriteDB
}

func (b *receiptWriteBatch) Set(key, value []byte) error {
	if bytes.HasPrefix(key, []byte("metadata/delivery/")) {
		if b.db.fail != nil {
			return b.db.fail
		}
		b.db.sets[string(key)]++
	}
	return b.Batch.Set(key, value)
}
func (b *receiptWriteBatch) Commit(mode storage.SyncMode) error {
	b.db.commits++
	return b.Batch.Commit(mode)
}

func TestDeliveryCoalescesReceiptsAcrossRaftEntries(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("complete=%t", complete), func(t *testing.T) {
			sm, db := newDeliveryTestSM(t)
			observed := &receiptWriteDB{DB: db, sets: map[string]int{}}
			sm.db = observed
			key := utils.EventKey(1, 2, 1)
			store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
			require.NoError(t, err)
			prepare, err := MarshalPrepareDeliveryBatchCmd([]DeliveryPrepare{{Key: key, Epoch: 1, Recipients: []string{"g:a", "g:b", "g:c"}}})
			require.NoError(t, err)
			ackA, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:a"}, {Key: key, Recipient: "g:a"}, {Key: key, Recipient: "unknown"}})
			require.NoError(t, err)
			refresh, err := MarshalPrepareDeliveryCmd(key, 2, []string{"g:new"})
			require.NoError(t, err)
			ackB, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:b"}})
			require.NoError(t, err)
			entries := []statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}, {Index: 3, Cmd: ackA}, {Index: 4, Cmd: refresh}, {Index: 5, Cmd: ackB}}
			if complete {
				ackC, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:c"}})
				require.NoError(t, err)
				entries = append(entries, statemachine.Entry{Index: 6, Cmd: ackC}, statemachine.Entry{Index: 7, Cmd: prepare})
			}
			results, err := sm.Update(entries)
			require.NoError(t, err)
			var initial []DeliveryPrepared
			require.NoError(t, json.Unmarshal(results[1].Result.Data, &initial))
			require.True(t, initial[0].State.Needs("g:a"), "earlier results must describe their own log position")
			var refreshed DeliveryState
			require.NoError(t, json.Unmarshal(results[3].Result.Data, &refreshed))
			require.False(t, refreshed.Needs("g:a"))
			require.True(t, refreshed.Needs("g:b"), "later ACKs cannot change an earlier preparation result")
			state, err := ReadDeliveryState(db, key)
			require.NoError(t, err)
			if complete {
				require.Nil(t, state)
				require.Zero(t, observed.sets[string(DeliveryStateKey(key))], "completed receipts need no storage encoding")
			} else {
				require.Equal(t, 1, observed.sets[string(DeliveryStateKey(key))], "one final receipt write across all commands")
				require.Equal(t, uint64(2), state.Epoch)
				require.False(t, state.Needs("g:a"))
				require.False(t, state.Needs("g:b"))
				require.True(t, state.Needs("g:c"))
			}
		})
	}
}

func TestDeliveryFlushFailureAbortsRaftBatch(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	failure := errors.New("receipt write failed")
	observed := &receiptWriteDB{DB: db, sets: map[string]int{}, fail: failure}
	sm.db = observed
	key := utils.EventKey(1, 2, 1)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	prepare, err := MarshalPrepareDeliveryCmd(key, 1, []string{"g:a"})
	require.NoError(t, err)
	entries := []statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}}
	_, err = sm.Update(entries)
	require.ErrorIs(t, err, failure)
	require.Zero(t, observed.commits)
	require.Zero(t, sm.lastApplied)
	_, _, err = db.Get(key)
	require.ErrorIs(t, err, storage.ErrNotFound)
	observed.fail = nil
	_, err = sm.Update(entries)
	require.NoError(t, err, "same entries can be retried after failed flush")
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.True(t, state.Needs("g:a"))
}

func TestDeliveryDeleteStorePrepareDoesNotFlushOldReceipt(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 1)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	old, err := MarshalPrepareDeliveryCmd(key, 1, []string{"g:old"})
	require.NoError(t, err)
	deletion, err := MarshalDeleteBatchCmd([][]byte{key})
	require.NoError(t, err)
	fresh, err := MarshalPrepareDeliveryCmd(key, 2, []string{"g:fresh"})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: old}, {Index: 3, Cmd: deletion}, {Index: 4, Cmd: store}, {Index: 5, Cmd: fresh}})
	require.NoError(t, err)
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.Equal(t, &DeliveryState{Epoch: 2, Recipients: map[string]bool{"g:fresh": false}}, state)
}
