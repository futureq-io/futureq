package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
)

func TestTypedAndWireDeliveryLedgerAgree(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			typedDB, typed := newPreloadTest(t, engine)
			wireDB, wire := newPreloadTest(t, engine)
			// Route through the real wire adapter for the comparison, including
			// command and result decoding, while keeping the backing stores independent.
			adapter := NewDeliveryLedger(wire.db, nil, nil, nil)
			wire.propose = adapter.apply
			epoch := uint64(1)
			recipients := []string{"g:a", "g:b", "u:old", "g:\xff\xfe", "g:<>&\u2028"}
			view := func(string) (uint64, []string, bool) { return epoch, recipients, true }
			typed.recipients, wire.recipients = view, view
			var typedDeleted, wireDeleted [][]byte
			typed.OnDelete = func(keys [][]byte) { typedDeleted = append(typedDeleted, keys...) }
			adapter.OnDelete = func(keys [][]byte) { wireDeleted = append(wireDeleted, keys...) }
			due := time.Now().Add(-time.Second).UnixMilli()
			a := putPipelineEvent(t, typedDB, "orders", 1, due)
			b := putPipelineEvent(t, typedDB, "orders", 2, due)
			empty := putPipelineEvent(t, typedDB, "orders", 3, due)
			for id := uint64(1); id <= 3; id++ {
				putPipelineEvent(t, wireDB, "orders", id, due)
			}
			missing := utils.EventKey(1, 2, 100)
			items := []raft.DeliveryPrepare{{Key: a, Epoch: epoch, Recipients: recipients}, {Key: b, Epoch: epoch, Recipients: recipients}, {Key: empty, Epoch: epoch}, {Key: missing, Epoch: epoch, Recipients: recipients}}
			compareBatch := func(items []raft.DeliveryPrepare) {
				t.Helper()
				left, err := typed.PrepareBatch(context.Background(), items)
				require.NoError(t, err)
				right, err := wire.PrepareBatch(context.Background(), items)
				require.NoError(t, err)
				require.Equal(t, right, left)
			}
			compareStores := func() {
				t.Helper()
				left, right := map[string][]byte{}, map[string][]byte{}
				require.NoError(t, typed.db.Scan(nil, func(k, v []byte) error { left[string(k)] = append([]byte(nil), v...); return nil }))
				require.NoError(t, wire.db.Scan(nil, func(k, v []byte) error { right[string(k)] = append([]byte(nil), v...); return nil }))
				require.Equal(t, right, left)
				require.Equal(t, wireDeleted, typedDeleted)
			}
			compareAck := func(acks []raft.DeliveryAck) {
				t.Helper()
				require.NoError(t, typed.Acknowledge(acks))
				require.NoError(t, wire.Acknowledge(acks))
				compareStores()
			}
			compareBatch(items)
			compareStores()
			compareBatch(items) // all hits, plus absent and recipient-free payloads
			compareAck([]raft.DeliveryAck{{Key: a, Recipient: "g:a"}, {Key: a, Recipient: "g:a"}, {Key: b, Recipient: "g:\xff\xfe"}, {Key: missing, Recipient: "unknown"}})
			epoch++
			recipients = []string{"g:new"}
			for i := range items {
				items[i].Epoch = epoch
				items[i].Recipients = recipients
			}
			compareBatch(items)
			compareStores()
			left, err := typed.Prepare("orders", a)
			require.NoError(t, err)
			right, err := wire.Prepare("orders", a)
			require.NoError(t, err)
			require.Equal(t, right, left)
			epoch++ // exercise the single-key miss/refresh path too
			left, err = typed.Prepare("orders", b)
			require.NoError(t, err)
			right, err = wire.Prepare("orders", b)
			require.NoError(t, err)
			require.Equal(t, right, left)
			compareStores()
			compareAck([]raft.DeliveryAck{{Key: a, Recipient: "g:b"}, {Key: a, Recipient: "g:\xff\xfe"}, {Key: a, Recipient: "g:<>&\u2028"}})
			compareAck([]raft.DeliveryAck{{Key: b, Recipient: "g:a"}, {Key: b, Recipient: "g:b"}, {Key: b, Recipient: "g:<>&\u2028"}})
			compareBatch(items) // final ACK deletion cannot resurrect payloads
			compareStores()
		})
	}
}

func TestPrepareBatchValidatesManifestHitsWithoutEncoding(t *testing.T) {
	d, ledger := newPipelineTest(t)
	key := putPipelineEvent(t, d, "orders", 1, time.Now().UnixMilli())
	valid := raft.DeliveryPrepare{Key: key, Epoch: 1, Recipients: []string{"g:a"}}
	_, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{valid})
	require.NoError(t, err)
	ledger.ProposeContext = func(context.Context, []byte) (statemachine.Result, error) {
		t.Fatal("manifest hits must not propose")
		return statemachine.Result{}, nil
	}
	for _, items := range [][]raft.DeliveryPrepare{
		nil, {valid, valid}, {{Key: key, Epoch: 1, Recipients: []string{"bad"}}}, {{Key: key, Epoch: 1, Recipients: []string{"g:" + strings.Repeat("<", raft.MaxDeliveryPrepareBytes)}}},
	} {
		_, err = ledger.PrepareBatch(context.Background(), items)
		require.Error(t, err)
	}
	_, err = ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{valid})
	require.NoError(t, err)
}

type ledgerWriteFailureDB struct {
	storage.DB
	fail error
}

func (db *ledgerWriteFailureDB) NewBatch() storage.Batch {
	return &ledgerWriteFailureBatch{Batch: db.DB.NewBatch(), db: db}
}

type ledgerWriteFailureBatch struct {
	storage.Batch
	db *ledgerWriteFailureDB
}

func (b *ledgerWriteFailureBatch) Set(k, v []byte) error {
	if strings.HasPrefix(string(k), "metadata/delivery/") && b.db.fail != nil {
		return b.db.fail
	}
	return b.Batch.Set(k, v)
}

func TestStandaloneReceiptFlushFailureDoesNotPublishState(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, single := range []bool{false, true} {
			t.Run(engine+"/single="+map[bool]string{true: "true", false: "false"}[single], func(t *testing.T) {
				d, ledger := newPreloadTest(t, engine)
				key := putPipelineEvent(t, d, "orders", 1, time.Now().UnixMilli())
				failure := errors.New("receipt write failed")
				observed := &ledgerWriteFailureDB{DB: d.db, fail: failure}
				ledger.db = observed
				ledger.recipients = func(string) (uint64, []string, bool) { return 1, []string{"g:a", "g:b"}, true }
				prepare := func() error {
					if single {
						state, err := ledger.Prepare("orders", key)
						if err != nil {
							require.Nil(t, state)
						}
						return err
					}
					states, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{{Key: key, Epoch: 1, Recipients: []string{"g:a", "g:b"}}})
					if err != nil {
						require.Nil(t, states)
					}
					return err
				}
				require.ErrorIs(t, prepare(), failure)
				state, err := raft.ReadDeliveryState(d.db, key)
				require.NoError(t, err)
				require.Nil(t, state)
				observed.fail = nil
				require.NoError(t, prepare())
				observed.fail = failure
				require.ErrorIs(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}), failure)
				state, err = raft.ReadDeliveryState(d.db, key)
				require.NoError(t, err)
				require.True(t, state.Needs("g:a"))
				observed.fail = nil
				require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
				state, err = raft.ReadDeliveryState(d.db, key)
				require.NoError(t, err)
				require.False(t, state.Needs("g:a"))
				require.True(t, state.Needs("g:b"))
			})
		}
	}
}

func TestPreparationSizingHonorsExactChunkBoundary(t *testing.T) {
	for _, path := range []string{"scan", "preload", "maintenance"} {
		t.Run(path, func(t *testing.T) {
			d, ledger := newPipelineTest(t)
			d.hub.Register("local", "orders", "workers", nil)
			now := time.Now().UnixMilli()
			a := putPipelineEvent(t, d, "orders", 1, now-1000)
			putPipelineEvent(t, d, "orders", 2, now-1000)
			view := d.hub.InterestView("orders")
			item := raft.DeliveryPrepare{Key: a, Epoch: view.Epoch, Recipients: view.Recipients}
			if path == "maintenance" {
				item.Epoch = 0
				_, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{item})
				require.NoError(t, err)
				item.Epoch = view.Epoch
			}
			data, err := raft.MarshalPrepareDeliveryBatchCmd([]raft.DeliveryPrepare{item})
			require.NoError(t, err)
			d.Limits.Bytes = len(data)
			var chunk deliveryChunk
			switch path {
			case "scan":
				chunk = d.collectTopic("orders", now)
			case "preload":
				p := &topicPreload{epoch: view.Epoch, keys: map[[24]byte]bool{[24]byte(a): true}, items: candidateHeap{{key: [24]byte(a), due: now - 1000}}}
				d.preloadCount = 1
				chunk = d.collectPreloaded("orders", p, view, now)
			case "maintenance":
				chunk = d.collectMaintenance()
			}
			require.Len(t, chunk.items, 1, "a command fitting exactly must be admitted")
			cmd, err := raft.MarshalPrepareDeliveryBatchCmd(chunk.items)
			require.NoError(t, err)
			require.Len(t, cmd, d.Limits.Bytes)
		})
	}
}
