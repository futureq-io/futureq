package dispatcher

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestStandaloneRestartRetiresUniversalReceiptsAndPreservesNamedGroups(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			root := t.TempDir()
			open := func() storage.DB {
				var db storage.DB
				var err error
				if engine == "pebble" {
					db, err = storage.NewPebble(config.Pebble{Mode: "disk", DataDir: root, WALEnabled: true}, zap.NewNop())
				} else {
					db, err = storage.NewBoltDB(config.Bolt{File: filepath.Join(root, "events.db")})
				}
				require.NoError(t, err)
				return db
			}
			setup := func(db storage.DB) (*Dispatcher, *DeliveryLedger) {
				wake := make(chan struct{}, 1)
				hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wake)
				require.NoError(t, hub.RestoreStandaloneEpoch(db))
				ledger := NewDeliveryLedger(db, hub.LocalDeliveryRecipients, NewDirectDeleteBackend(db, zap.NewNop()), nil)
				d := NewDispatcher(db, hub, NewDeleter(ledger, time.Hour, zap.NewNop()), time.Millisecond, time.Second, wake, zap.NewNop())
				d.TimeBucket = time.Millisecond
				d.PrepareBatch, d.MaintenanceRecipients = ledger.PrepareBatch, hub.LocalDeliveryRecipients
				return d, ledger
			}
			db := open()
			d, _ := setup(db)
			d.hub.registryEpoch = 100 // receipts from a process with substantial subscription churn
			d.hub.Register("old-universal", "universal", "", make(chan *pb.QueueMessage, 1))
			d.hub.Register("old-mixed", "mixed", "", make(chan *pb.QueueMessage, 1))
			d.hub.Register("old-worker", "mixed", "workers", make(chan *pb.QueueMessage, 1))
			due := time.Now().Add(-time.Second).UnixMilli()
			universalKey := putPipelineEvent(t, d, "universal", 1, due)
			mixedKey := putPipelineEvent(t, d, "mixed", 2, due)
			require.Equal(t, 2, d.dispatchAll())
			oldEpoch := d.hub.registryEpoch
			require.NoError(t, db.Close())

			db = open()
			defer db.Close() //nolint:errcheck
			d, ledger := setup(db)
			require.Greater(t, d.hub.registryEpoch, oldEpoch)
			chunk := d.collectMaintenance() // cleanup also works before any new subscription
			require.Len(t, chunk.items, 2)
			_, err := d.prepareChunk(context.Background(), chunk)
			require.NoError(t, err)
			_, _, err = db.Get(universalKey)
			require.ErrorIs(t, err, storage.ErrNotFound)
			state, err := raft.ReadDeliveryState(db, mixedKey)
			require.NoError(t, err)
			require.True(t, state.Needs("g:workers"))
			require.False(t, state.Needs("u:old-mixed"))
			worker, universal := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
			d.hub.Register("new-worker", "mixed", "workers", worker)
			d.hub.Register("new-universal", "mixed", "", universal)
			require.Equal(t, 1, d.dispatchAll())
			require.Len(t, worker, 1)
			require.Empty(t, universal, "new subscriptions do not join a prepared message")
			require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: mixedKey, Recipient: "g:workers"}}))
			_, _, err = db.Get(mixedKey)
			require.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
}

func TestStandaloneEpochDoesNotIgnoreCorruptReceipts(t *testing.T) {
	d, _ := newPipelineTest(t)
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	batch := d.db.NewBatch()
	defer batch.Close() //nolint:errcheck
	require.NoError(t, batch.Set(raft.DeliveryStateKey(key), []byte("invalid JSON")))
	require.NoError(t, batch.Commit(storage.Sync))
	require.Error(t, d.hub.RestoreStandaloneEpoch(d.db))
	data, err := json.Marshal(&raft.DeliveryState{Epoch: ^uint64(0)})
	require.NoError(t, err)
	batch = d.db.NewBatch()
	defer batch.Close() //nolint:errcheck
	require.NoError(t, batch.Set(raft.DeliveryStateKey(key), data))
	require.NoError(t, batch.Commit(storage.Sync))
	require.ErrorContains(t, d.hub.RestoreStandaloneEpoch(d.db), "exhausted")
}
