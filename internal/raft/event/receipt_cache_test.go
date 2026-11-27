package raft

import (
	"bytes"
	"io"
	"testing"

	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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
	const slots = 2
	cache := newReceiptCache(1024, slots)
	firstKey := string(utils.EventKey(1, 2, 3))
	secondKey := string(utils.EventKey(1, 2, 3+slots))
	first := newDeliveryState(1, []string{"g:first"})
	second := newDeliveryState(1, []string{"g:second"})
	cache.store(firstKey, first)
	cache.store(secondKey, second)
	require.Nil(t, cache.get(firstKey))
	require.Same(t, second, cache.get(secondKey))
	require.Len(t, cache.blocks[0], slots)
	require.Equal(t, 1, cache.entries)
	require.Equal(t, 1, cache.slotEvictions)

	cache.store(firstKey, nil)
	require.Same(t, second, cache.get(secondKey))
	cache.store(secondKey, nil)
	require.Zero(t, cache.bytes)
}

func TestReceiptCacheByteBudget(t *testing.T) {
	cache := newReceiptCache(256, 2)
	key := string(utils.EventKey(1, 2, 1))
	colliding := string(utils.EventKey(1, 2, 3))
	other := string(utils.EventKey(1, 2, 2))
	small := newDeliveryState(1, []string{"g:a"})
	large := newDeliveryState(1, []string{"g:" + string(bytes.Repeat([]byte{'x'}, 256))})
	cache.store(key, small)
	retained := cache.bytes
	require.Positive(t, retained)
	require.LessOrEqual(t, retained, cache.maxBytes)

	cache.store(other, large)
	cache.store(colliding, large)
	require.Nil(t, cache.get(other))
	require.Nil(t, cache.get(colliding))
	require.Same(t, small, cache.get(key))
	require.Equal(t, retained, cache.bytes)
	require.Equal(t, 2, cache.budgetRejects)
	require.Zero(t, cache.slotEvictions)

	cache.store(key, large)
	require.Nil(t, cache.get(key), "evict the old version when the new one does not fit")
	require.Equal(t, 1, cache.byteEvictions)
	require.Equal(t, 3, cache.budgetRejects)
	require.Zero(t, cache.bytes)
	require.Zero(t, cache.entries)

	cache.reset()
	cache.store(key, small)
	require.Same(t, small, cache.get(key), "a reset retains the configured limits")
	require.EqualValues(t, 256, cache.maxBytes)
	require.Equal(t, 2, cache.maxEntries)
}

func TestDisabledReceiptCacheFallsBackToPebble(t *testing.T) {
	defaultSM, db := newDeliveryTestSM(t)
	observed := &applyReceiptReadDB{DB: db}
	sm := NewEventStateMachineFactoryWithReceiptCache(observed, defaultSM.repo, nil, nil, nil, defaultSM.logger, 0, 2)(1, 1).(*EventStateMachine)
	key := utils.EventKey(1, 2, 3)
	receipt, err := MarshalPublishReceipt(1, []string{"g:a"})
	require.NoError(t, err)
	store, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload"), Receipt: receipt}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}})
	require.NoError(t, err)
	require.Empty(t, sm.receipts.blocks)

	ack, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:a"}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 2, Cmd: ack}})
	require.NoError(t, err)
	require.Equal(t, 1, observed.reads)
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.Nil(t, state)
}

func TestReceiptCacheMetrics(t *testing.T) {
	read := func(metric prometheus.Metric) *dto.Metric {
		t.Helper()
		value := new(dto.Metric)
		require.NoError(t, metric.Write(value))
		return value
	}
	slotEvictions := metrics.DeliveryApplyReceiptCacheEvictionsTotal.WithLabelValues("slot_collision")
	byteEvictions := metrics.DeliveryApplyReceiptCacheEvictionsTotal.WithLabelValues("byte_limit")
	budgetRejects := metrics.DeliveryApplyReceiptCacheBudgetRejectionsTotal
	beforeSlot := read(slotEvictions).GetCounter().GetValue()
	beforeBytes := read(byteEvictions).GetCounter().GetValue()
	beforeReject := read(budgetRejects).GetCounter().GetValue()

	cache := newReceiptCache(256, 1)
	t.Cleanup(cache.reset)
	first := string(utils.EventKey(1, 2, 1))
	second := string(utils.EventKey(1, 2, 2))
	small := newDeliveryState(1, []string{"g:a"})
	large := newDeliveryState(1, []string{"g:" + string(bytes.Repeat([]byte{'x'}, 256))})
	cache.commit(&DeliveryBatch{states: map[string]*DeliveryState{first: small}})
	cache.commit(&DeliveryBatch{states: map[string]*DeliveryState{second: small}})
	require.Equal(t, beforeSlot+1, read(slotEvictions).GetCounter().GetValue())
	require.Equal(t, float64(cache.bytes), read(metrics.DeliveryApplyReceiptCacheRetainedBytes).GetGauge().GetValue())
	require.EqualValues(t, 1, read(metrics.DeliveryApplyReceiptCacheEntries).GetGauge().GetValue())

	cache.commit(&DeliveryBatch{states: map[string]*DeliveryState{second: large}})
	require.Equal(t, beforeBytes+1, read(byteEvictions).GetCounter().GetValue())
	require.Equal(t, beforeReject+1, read(budgetRejects).GetCounter().GetValue())
	require.Zero(t, read(metrics.DeliveryApplyReceiptCacheRetainedBytes).GetGauge().GetValue())
	require.Zero(t, read(metrics.DeliveryApplyReceiptCacheEntries).GetGauge().GetValue())
}

func TestReceiptCacheAdmitsNewSlotsWhenBudgetIsFull(t *testing.T) {
	cache := newReceiptCache(256, 8)
	first := string(utils.EventKey(1, 2, 1))
	second := string(utils.EventKey(1, 2, 2))
	third := string(utils.EventKey(1, 2, 3))
	fourth := string(utils.EventKey(1, 2, 4))
	small := newDeliveryState(1, []string{"g:a"})
	cache.store(first, small)
	cache.store(second, small)
	cache.store(first, small) // Refreshing an existing receipt keeps its age.
	cache.store(third, small)

	require.Nil(t, cache.get(first))
	require.Same(t, small, cache.get(second))
	require.Same(t, small, cache.get(third))
	require.Equal(t, 1, cache.byteEvictions)
	require.Zero(t, cache.slotEvictions)
	require.Zero(t, cache.budgetRejects)

	large := newDeliveryState(1, []string{"g:" + string(bytes.Repeat([]byte{'x'}, 100))})
	cache.store(fourth, large)
	require.Nil(t, cache.get(second))
	require.Nil(t, cache.get(third))
	require.Same(t, large, cache.get(fourth))
	require.Equal(t, 3, cache.byteEvictions)
	require.Equal(t, 1, cache.entries)
	require.LessOrEqual(t, cache.bytes, cache.maxBytes)
}

func TestReceiptCacheCommitFreesDeletedSlotsBeforeAdmission(t *testing.T) {
	oldest := string(utils.EventKey(1, 2, 1))
	acknowledged := string(utils.EventKey(1, 2, 2))
	published := string(utils.EventKey(1, 2, 3))
	state := newDeliveryState(1, []string{"g:a"})

	// Map iteration order varies. The outcome must be the same for either order.
	for range 64 {
		cache := newReceiptCache(256, 8)
		cache.store(oldest, state)
		cache.store(acknowledged, state)
		cache.commit(&DeliveryBatch{states: map[string]*DeliveryState{published: state, acknowledged: nil}})

		require.Same(t, state, cache.get(oldest))
		require.Nil(t, cache.get(acknowledged))
		require.Same(t, state, cache.get(published))
		require.Equal(t, 2, cache.entries)
	}
}
