package dispatcher

import (
	"context"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

type ledgerCommitModeDB struct {
	storage.DB
	modes []storage.SyncMode
}

func (db *ledgerCommitModeDB) NewBatch() storage.Batch {
	return &ledgerCommitModeBatch{Batch: db.DB.NewBatch(), db: db}
}

type ledgerCommitModeBatch struct {
	storage.Batch
	db *ledgerCommitModeDB
}

func (b *ledgerCommitModeBatch) Commit(mode storage.SyncMode) error {
	b.db.modes = append(b.db.modes, mode)
	return b.Batch.Commit(mode)
}

func TestStandaloneLedgerSyncsPreparationOnly(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, preparation := range []string{"single", "batch"} {
			t.Run(engine+"/"+preparation, func(t *testing.T) {
				d, ledger := newPreloadTest(t, engine)
				d.hub.Register("a", "orders", "a", make(chan *pb.QueueMessage, 1))
				d.hub.Register("b", "orders", "b", make(chan *pb.QueueMessage, 1))
				key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
				observed := &ledgerCommitModeDB{DB: d.db}
				ledger.db = observed
				if preparation == "single" {
					state, err := ledger.Prepare("orders", key)
					require.NoError(t, err)
					require.NotNil(t, state)
				} else {
					view := d.hub.InterestView("orders")
					states, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{{Key: key, Epoch: view.Epoch, Recipients: view.Recipients}})
					require.NoError(t, err)
					require.NotNil(t, states[string(key)])
				}
				require.Equal(t, []storage.SyncMode{storage.Sync}, observed.modes, "preparation must sync before delivery")
				require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
				require.Equal(t, []storage.SyncMode{storage.Sync, storage.NoSync}, observed.modes)
				state, err := raft.ReadDeliveryState(d.db, key)
				require.NoError(t, err)
				require.False(t, state.Needs("g:a"))
				require.True(t, state.Needs("g:b"))
				require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:b"}}))
				require.Equal(t, []storage.SyncMode{storage.Sync, storage.NoSync, storage.NoSync}, observed.modes, "final ACK deletion must also remain NoSync")
				_, _, err = d.db.Get(key)
				require.ErrorIs(t, err, storage.ErrNotFound)
			})
		}
	}
}
