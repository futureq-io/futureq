package raft

import (
	"bytes"
	"io"
	"testing"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"github.com/stretchr/testify/require"
)

type applyReceiptReadDB struct {
	storage.DB
	reads int
}

func (db *applyReceiptReadDB) Get(key []byte) ([]byte, io.Closer, error) {
	if bytes.HasPrefix(key, []byte("metadata/delivery/")) {
		db.reads++
	}
	return db.DB.Get(key)
}

func TestApplyReceiptCacheCommitAndRecovery(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	observed := &applyReceiptReadDB{DB: db}
	sm.db = observed
	key := utils.EventKey(1, 2, 3)
	receipt, err := MarshalPublishReceipt(1, []string{"g:a", "g:b"})
	require.NoError(t, err)
	store, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload"), Receipt: receipt}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}})
	require.NoError(t, err)
	ackA, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:a"}})
	require.NoError(t, err)
	ackB, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:b"}})
	require.NoError(t, err)

	sm.db = failingCommitDB{DB: observed}
	_, err = sm.Update([]statemachine.Entry{{Index: 2, Cmd: ackA}})
	require.Error(t, err)
	sm.db = observed
	require.True(t, sm.receipts.get(string(key)).Needs("g:a"), "failed commits must leave the cached bitmap intact")
	_, err = sm.Update([]statemachine.Entry{{Index: 2, Cmd: ackA}})
	require.NoError(t, err)
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.False(t, state.Needs("g:a"))
	require.True(t, state.Needs("g:b"))
	require.Zero(t, observed.reads)

	var snapshot bytes.Buffer
	handle, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	require.NoError(t, sm.SaveSnapshot(handle, &snapshot, nil))
	_, err = sm.Update([]statemachine.Entry{{Index: 3, Cmd: ackB}})
	require.NoError(t, err)
	require.Nil(t, sm.receipts.get(string(key)))
	require.Zero(t, observed.reads)

	// Restore over a cached receipt for another message. Its old state must
	// be discarded even though recovery installed a receipt with an earlier ID.
	other, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: 4, Bucket: 1, TopicHash: 2, Msg: []byte("other"), Receipt: receipt}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 4, Cmd: other}})
	require.NoError(t, err)
	otherKey := utils.EventKey(1, 2, 4)
	require.NotNil(t, sm.receipts.get(string(otherKey)))
	require.NoError(t, sm.RecoverFromSnapshot(&snapshot, nil))
	require.Nil(t, sm.receipts.get(string(otherKey)))

	_, err = sm.Update([]statemachine.Entry{{Index: 3, Cmd: ackB}})
	require.NoError(t, err)
	require.Equal(t, 1, observed.reads, "recovered receipts must fall back to Pebble")
	state, err = ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.Nil(t, state)
}

func TestReceiptCacheSlotCollisionRequiresExactKey(t *testing.T) {
	var cache receiptCache
	firstKey := string(utils.EventKey(1, 2, 3))
	secondKey := string(utils.EventKey(1, 2, 3+receiptCacheSlots))
	first := newDeliveryState(1, []string{"g:first"})
	second := newDeliveryState(1, []string{"g:second"})
	cache.store(firstKey, first)
	cache.store(secondKey, second)
	require.Nil(t, cache.get(firstKey))
	require.Same(t, second, cache.get(secondKey))

	cache.store(firstKey, nil)
	require.Same(t, second, cache.get(secondKey))
	cache.store(secondKey, nil)
	require.Zero(t, cache.bytes)
}
