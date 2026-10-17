package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
)

type ackSyncDB struct {
	storage.DB
	modes []storage.SyncMode
	fail  error
}

func (db *ackSyncDB) NewBatch() storage.Batch {
	return &ackSyncBatch{Batch: db.DB.NewBatch(), db: db}
}

type ackSyncBatch struct {
	storage.Batch
	db *ackSyncDB
}

func (batch *ackSyncBatch) Commit(mode storage.SyncMode) error {
	batch.db.modes = append(batch.db.modes, mode)
	if batch.db.fail != nil {
		return batch.db.fail
	}
	return batch.Batch.Commit(mode)
}

func TestAckNoSyncPreservesPreparationAndRecipientCompletion(t *testing.T) {
	for _, noSync := range []bool{false, true} {
		name, ackMode := "default", storage.Sync
		if noSync {
			name, ackMode = "no_sync", storage.NoSync
		}
		t.Run(name, func(t *testing.T) {
			d, ledger := newPreloadTest(t, "pebble")
			key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
			db := &ackSyncDB{DB: d.db}
			ledger.db, ledger.AckNoSync = db, noSync
			var deleted [][]byte
			ledger.OnDelete = func(keys [][]byte) { deleted = append(deleted, keys...) }
			_, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{{Key: key, Epoch: 1, Recipients: []string{"g:a", "g:b"}}})
			require.NoError(t, err)
			require.Equal(t, []storage.SyncMode{storage.Sync}, db.modes)
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
			state, err := raft.ReadDeliveryState(d.db, key)
			require.NoError(t, err)
			require.False(t, state.Needs("g:a"))
			require.True(t, state.Needs("g:b"))
			require.Empty(t, deleted)
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:b"}}))
			require.Equal(t, []storage.SyncMode{storage.Sync, ackMode, ackMode}, db.modes)
			require.Equal(t, [][]byte{key}, deleted)
			state, err = raft.ReadDeliveryState(d.db, key)
			require.NoError(t, err)
			require.Nil(t, state)
			_, closer, err := d.db.Get(key)
			if closer != nil {
				_ = closer.Close()
			}
			require.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
}

func TestAckNoSyncFailureRetainsReceiptAndRetries(t *testing.T) {
	d, ledger := newPreloadTest(t, "pebble")
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	_, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{{Key: key, Epoch: 1, Recipients: []string{"g:workers"}}})
	require.NoError(t, err)
	forced := errors.New("forced NoSync failure")
	db := &ackSyncDB{DB: d.db, fail: forced}
	ledger.db, ledger.AckNoSync = db, true
	notified := false
	ledger.OnDelete = func([][]byte) { notified = true }
	acks := []raft.DeliveryAck{{Key: key, Recipient: "g:workers"}}
	require.ErrorIs(t, ledger.Acknowledge(acks), forced)
	require.Equal(t, []storage.SyncMode{storage.NoSync}, db.modes)
	require.False(t, notified)
	state, err := raft.ReadDeliveryState(d.db, key)
	require.NoError(t, err)
	require.True(t, state.Needs("g:workers"))
	db.fail = nil
	require.NoError(t, ledger.Acknowledge(acks))
	require.True(t, notified)
}

func TestAckNoSyncLeavesExternalProposalPathUnchanged(t *testing.T) {
	calls := 0
	ledger := NewDeliveryLedger(nil, nil, nil, func([]byte) (statemachine.Result, error) {
		calls++
		return statemachine.Result{}, nil
	})
	ledger.AckNoSync = true
	require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: make([]byte, 24), Recipient: "g:workers"}}))
	require.Equal(t, 1, calls)
}
