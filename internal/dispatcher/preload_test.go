package dispatcher

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func newPreloadTest(t *testing.T, engine string) (*Dispatcher, *DeliveryLedger) {
	t.Helper()
	d, ledger := newPipelineTest(t)
	if engine == "bolt" {
		db, err := storage.NewBoltDB(config.Bolt{File: filepath.Join(t.TempDir(), "events.db")})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		d.db = db
		ledger = NewDeliveryLedger(db, d.hub.LocalDeliveryRecipients, NewDirectDeleteBackend(db, zap.NewNop()), nil)
		d.deleter = NewDeleter(ledger, time.Hour, zap.NewNop())
		d.PrepareBatch = ledger.PrepareBatch
	}
	d.PreloadLookahead, d.PreloadMaxMessages = 10*time.Second, 4
	d.Limits.WorkTime = time.Hour
	return d, ledger
}

func TestPreloadBoundsExactHorizonAndDeduplication(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, _ := newPreloadTest(t, engine)
			d.PreloadMaxMessages = 3
			d.TimeBucket = time.Hour
			d.hub.Register("a", "a", "workers", make(chan *pb.QueueMessage, 8))
			d.hub.Register("b", "b", "workers", make(chan *pb.QueueMessage, 8))
			now := time.Now().UnixMilli()
			first := putPipelineEvent(t, d, "a", 1, now+2000)
			putPipelineEvent(t, d, "a", 2, now+4000)
			putPipelineEvent(t, d, "a", 3, now+10001) // same bucket, outside exact horizon
			putPipelineEvent(t, d, "b", 4, now+3000)
			putPipelineEvent(t, d, "b", 5, now+5000)
			for range 3 {
				require.Empty(t, d.collectScheduledTopic("a", now).items)
			}
			require.Equal(t, 2, d.preloadCount, "repeat scans deduplicate candidates")
			require.Equal(t, [24]byte(first), d.preloads["a"].items[0].key)
			require.Empty(t, d.collectScheduledTopic("b", now).items)
			require.Equal(t, 3, d.preloadCount, "cap applies across topics")
			state, err := raft.ReadDeliveryState(d.db, first)
			require.NoError(t, err)
			require.Nil(t, state, "preloading cannot freeze recipients or prepare early")
			d.prunePreloads(map[string]bool{"b": true})
			require.Equal(t, 1, d.preloadCount)
			d.prunePreloads(nil)
			require.Zero(t, d.preloadCount)
		})
	}
}

func TestPreloadCachedCollectionAvoidsDueIteratorAndStillChecksActualTime(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	ch := make(chan *pb.QueueMessage, 4)
	d.hub.Register("local", "orders", "workers", ch)
	now := time.Now().UnixMilli()
	key := putPipelineEvent(t, d, "orders", 1, now+5000)
	require.Zero(t, d.dispatchAll())
	require.Equal(t, 1, d.preloadCount)
	d.db = scanStartupDB{DB: d.db, beforeNewIter: func() { t.Fatal("cached due candidates must not need a range scan") }}
	chunk := d.collectScheduledTopic("orders", now+5000)
	require.Len(t, chunk.items, 1)
	require.Equal(t, key, chunk.items[0].Key)
	// A future caller timestamp is not delivery authorization, even for a cache hit.
	require.Zero(t, d.deliverChunk(chunk, map[string]*raft.DeliveryState{string(key): {Epoch: chunk.items[0].Epoch, Recipients: map[string]bool{"g:workers": false}}}))
	require.Empty(t, ch)
}

func TestPreloadFullBufferDoesNotHideNewOverdueArrivals(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, _ := newPreloadTest(t, engine)
			d.PreloadMaxMessages = 1
			d.interval = 10 * time.Millisecond
			ch := make(chan *pb.QueueMessage, 4)
			d.hub.Register("local", "orders", "workers", ch)
			putPipelineEvent(t, d, "orders", 1, time.Now().Add(5*time.Second).UnixMilli())
			require.Zero(t, d.dispatchAll())
			require.Equal(t, 1, d.preloadCount)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			// Deliberately omit a producer wake: the periodic live scan must find it.
			late := putPipelineEvent(t, d, "orders", 2, time.Now().Add(-time.Minute).UnixMilli())
			select {
			case msg := <-ch:
				require.Equal(t, late, msg.DeliveryTag)
			case <-time.After(time.Second):
				t.Fatal("a full future buffer hid an un-preloaded overdue message")
			}
		})
	}
}

func TestPreloadAlternatesLiveScansAndRetriesFailures(t *testing.T) {
	d, ledger := newPreloadTest(t, "pebble")
	d.Limits.Candidates = 1
	ch := make(chan *pb.QueueMessage, 8)
	d.hub.Register("local", "orders", "workers", ch)
	due := time.Now().Add(40 * time.Millisecond).UnixMilli()
	first := putPipelineEvent(t, d, "orders", 1, due)
	putPipelineEvent(t, d, "orders", 2, due)
	require.Zero(t, d.dispatchAll())
	time.Sleep(time.Until(time.UnixMilli(due)))
	d.PrepareBatch = func(context.Context, []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
		return nil, errors.New("uncertain preparation failure")
	}
	require.Zero(t, d.dispatchAll()) // pops a candidate whose preparation fails
	d.PrepareBatch = ledger.PrepareBatch
	late := putPipelineEvent(t, d, "orders", 3, due-60000)
	seen := make(map[string]bool)
	for range 12 {
		d.dispatchAll()
		for len(ch) > 0 {
			seen[string((<-ch).DeliveryTag)] = true
		}
	}
	require.Len(t, seen, 3)
	require.True(t, seen[string(first)], "failed preparation remains recoverable by live scanning")
	require.True(t, seen[string(late)], "an older insertion must be found while cached candidates drain")
}

func TestPreloadTimerWakesBeforePollAndDoesNotPrepareEarly(t *testing.T) {
	d, ledger := newPreloadTest(t, "pebble")
	d.interval = 2 * time.Second
	ch := make(chan *pb.QueueMessage, 4)
	d.hub.Register("local", "orders", "workers", ch)
	due := time.Now().Add(100 * time.Millisecond).UnixMilli()
	key := putPipelineEvent(t, d, "orders", 1, due)
	var early atomic.Bool
	d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
		if time.Now().UnixMilli() < due {
			early.Store(true)
		}
		return ledger.PrepareBatch(ctx, items)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case msg := <-ch:
		require.Equal(t, key, msg.DeliveryTag)
		require.GreaterOrEqual(t, time.Now().UnixMilli(), due)
		require.False(t, early.Load())
	case <-time.After(750 * time.Millisecond):
		t.Fatal("preloaded candidate waited for the 2-second poll instead of its due timer")
	}
}

func TestPreloadRevalidatesPayloadAndRecipientsAtDelivery(t *testing.T) {
	for _, fault := range []string{"deleted", "expired", "new-subscriber", "full-queue"} {
		t.Run(fault, func(t *testing.T) {
			d, _ := newPreloadTest(t, "pebble")
			ch, added := make(chan *pb.QueueMessage, 4), make(chan *pb.QueueMessage, 4)
			d.hub.Register("local", "orders", "", ch)
			due := time.Now().Add(40 * time.Millisecond).UnixMilli()
			key := putPipelineEvent(t, d, "orders", 1, due)
			if fault == "expired" {
				value, closer, err := d.db.Get(key)
				require.NoError(t, err)
				var msg storagepb.StoredMessage
				require.NoError(t, proto.Unmarshal(value, &msg))
				require.NoError(t, closer.Close())
				msg.TtlMs = 990 // expires 10ms before its due time
				value, err = proto.Marshal(&msg)
				require.NoError(t, err)
				batch := d.db.NewBatch()
				require.NoError(t, batch.Set(key, value))
				require.NoError(t, batch.Commit(storage.Sync))
				require.NoError(t, batch.Close())
			}
			require.Zero(t, d.dispatchAll())
			require.Equal(t, 1, d.preloadCount)
			if fault == "deleted" {
				batch := d.db.NewBatch()
				require.NoError(t, batch.Delete(key))
				require.NoError(t, batch.Commit(storage.Sync))
				require.NoError(t, batch.Close())
			} else if fault == "new-subscriber" {
				d.hub.Register("added", "orders", "", added)
			} else if fault == "full-queue" {
				for range cap(ch) {
					ch <- &pb.QueueMessage{}
				}
			}
			time.Sleep(time.Until(time.UnixMilli(due)))
			if fault == "new-subscriber" {
				require.Equal(t, 1, d.dispatchAll())
				require.Len(t, ch, 1)
				require.Len(t, added, 1, "preloading must not freeze the old recipient list")
			} else {
				require.Zero(t, d.dispatchAll())
				if fault == "full-queue" {
					for len(ch) > 0 {
						<-ch
					}
					require.Equal(t, 1, d.dispatchAll(), "queue failure must remain eligible for retry")
				} else {
					require.Empty(t, ch)
				}
			}
		})
	}
}

func TestPreloadCannotBypassPostPreparationBarrier(t *testing.T) {
	for _, fault := range []string{"minority", "pending", "renew"} {
		t.Run(fault, func(t *testing.T) {
			d, ledger := newPreloadTest(t, "pebble")
			epoch, active := uint64(9), true
			d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, active }, time.Second)
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			prepared := false
			d.ReadBarrier = func() error {
				if fault == "minority" && prepared {
					return errors.New("no quorum")
				}
				d.hub.GrantDeliveryPermit(epoch, time.Now())
				return nil
			}
			due := time.Now().Add(40 * time.Millisecond).UnixMilli()
			putPipelineEvent(t, d, "orders", 1, due)
			require.Zero(t, d.dispatchAll())
			require.Equal(t, 1, d.preloadCount)
			d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
				prepared = true
				d.hub.GrantDeliveryPermit(epoch, time.Now().Add(-2*ConsumerReadPermit))
				if fault == "pending" {
					d.hub.WithRebalanceBarrier(func() { epoch++; active = false })
				}
				return ledger.PrepareBatch(ctx, items)
			}
			time.Sleep(time.Until(time.UnixMilli(due)))
			if fault == "renew" {
				require.Equal(t, 1, d.dispatchAll())
				require.Len(t, ch, 1)
			} else {
				require.Zero(t, d.dispatchAll())
				require.Empty(t, ch)
			}
			require.True(t, prepared)
		})
	}
}
