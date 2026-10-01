package dispatcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func newPipelineTest(t *testing.T) (*Dispatcher, *DeliveryLedger) {
	t.Helper()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	wake := make(chan struct{}, 1)
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wake)
	ledger := NewDeliveryLedger(db, hub.LocalDeliveryRecipients, NewDirectDeleteBackend(db, zap.NewNop()), nil)
	d := NewDispatcher(db, hub, NewDeleter(ledger, time.Hour, zap.NewNop()), time.Millisecond, time.Second, wake, zap.NewNop())
	d.TimeBucket = time.Millisecond
	d.PrepareBatch = ledger.PrepareBatch
	d.MaintenanceRecipients = hub.LocalDeliveryRecipients
	return d, ledger
}

func putPipelineEvent(t *testing.T, d *Dispatcher, topic string, id uint64, due int64) []byte {
	t.Helper()
	msg := &storagepb.StoredMessage{Topic: topic, Payload: []byte("payload"), EnqueuedAtUnixMs: due - 1000, DelayMs: 1000}
	value, err := proto.Marshal(msg)
	require.NoError(t, err)
	key := utils.EventKey(utils.CalculateBucket(due, d.TimeBucket), utils.TopicHash(topic), id)
	batch := d.db.NewBatch()
	require.NoError(t, batch.Set(key, value))
	require.NoError(t, batch.Commit(storage.Sync))
	require.NoError(t, batch.Close())
	return key
}

func TestPreparationExpiryRenewsButPendingEpochAndMinorityReject(t *testing.T) {
	for _, fault := range []string{"expired", "pending", "minority"} {
		t.Run(fault, func(t *testing.T) {
			d, ledger := newPipelineTest(t)
			epoch, active := uint64(1), true
			d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, active }, time.Second)
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
			barriers := 0
			d.ReadBarrier = func() error {
				barriers++
				if fault == "minority" && barriers > 1 {
					return errors.New("no quorum")
				}
				d.hub.GrantDeliveryPermit(epoch, time.Now())
				return nil
			}
			d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
				d.hub.GrantDeliveryPermit(epoch, time.Now().Add(-2*ConsumerReadPermit))
				if fault == "pending" {
					d.hub.WithRebalanceBarrier(func() { epoch++; active = false })
				}
				return ledger.PrepareBatch(ctx, items)
			}
			if fault == "expired" {
				require.Equal(t, 1, d.dispatchAll())
				require.Len(t, ch, 1)
			} else {
				require.Zero(t, d.dispatchAll())
				require.Empty(t, ch)
			}
			require.Equal(t, 2, barriers, "preparation is followed by a fresh quorum/epoch check")
		})
	}
}

func TestSlowPreparationDoesNotStarveAnotherTopicAndCancels(t *testing.T) {
	d, ledger := newPipelineTest(t)
	d.Limits.PrepareTimeout = time.Minute
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("slow", "a-slow", "workers", make(chan *pb.QueueMessage, 1))
	d.hub.Register("fast", "b-fast", "workers", ch)
	putPipelineEvent(t, d, "a-slow", 1, time.Now().Add(-time.Second).UnixMilli())
	putPipelineEvent(t, d, "b-fast", 2, time.Now().Add(-time.Second).UnixMilli())
	slowStarted := make(chan struct{})
	var started atomic.Bool
	d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
		if string(items[0].Key[:8]) == string(utils.TopicLowerBound(utils.TopicHash("a-slow"))) {
			if started.CompareAndSwap(false, true) {
				close(slowStarted)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return ledger.PrepareBatch(ctx, items)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow job did not start")
	}
	select {
	case msg := <-ch:
		require.Equal(t, "b-fast", msg.Topic)
	case <-time.After(time.Second):
		t.Fatal("slow preparation starved fast topic")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("preparation cancellation did not stop workers")
	}
}

func TestCyclicCursorBoundsFilteredWorkAndRevisitsOlderKeys(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.Limits.Candidates, d.Limits.Examined, d.Limits.WorkTime = 2, 3, time.Hour
	ch := make(chan *pb.QueueMessage, 20)
	d.hub.Register("local", "orders", "workers", ch)
	d.hub.SetGroupMembership(func(string, string) []metadata.ConsumerMember {
		return []metadata.ConsumerMember{{ID: "local"}, {ID: "remote"}}
	})
	due := time.Now().Add(-time.Second).UnixMilli()
	for id := uint64(1); id <= 10; id++ {
		putPipelineEvent(t, d, "orders", id, due)
	}
	for i := 0; i < 12; i++ {
		require.LessOrEqual(t, d.dispatchAll(), 2)
	}
	require.Len(t, ch, 5)
	for id := uint64(1); id <= 10; id += 2 {
		state, err := raft.ReadDeliveryState(d.db, utils.EventKey(utils.CalculateBucket(due, d.TimeBucket), utils.TopicHash("orders"), id))
		require.NoError(t, err)
		require.Nil(t, state, "remote-only keys must not prepare")
	}
	older := putPipelineEvent(t, d, "orders", 12, due-1000)
	for i := 0; i < 12 && len(ch) < 6; i++ {
		d.dispatchAll()
	}
	require.Len(t, ch, 6)
	var found bool
	for len(ch) > 0 {
		if string((<-ch).DeliveryTag) == string(older) {
			found = true
		}
	}
	require.True(t, found, "insertions behind a cursor must be revisited")
}

func TestFullQueueRetriesOtherInterestAndOldAttemptCannotEraseRetry(t *testing.T) {
	d, _ := newPipelineTest(t)
	full, other := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
	full <- &pb.QueueMessage{}
	d.hub.Register("a", "orders", "a", full)
	d.hub.Register("b", "orders", "b", other)
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	require.Equal(t, 1, d.dispatchAll())
	firstB := <-other
	<-full
	require.Equal(t, 1, d.dispatchAll(), "one recipient's in-flight record must not suppress a full recipient")
	firstA := <-full
	d.hub.ExpireInFlight(time.Now().Add(time.Hour))
	require.Equal(t, 1, d.dispatchAll())
	retryA, retryB := <-full, <-other
	d.hub.RejectQueuedAttempt("a", firstA)
	d.hub.RejectQueuedAttempt("b", firstB)
	require.True(t, d.hub.CanSend("a", retryA))
	require.True(t, d.hub.CanSend("b", retryB))
	require.False(t, d.hub.AcknowledgeSent("a", key), "a queued attempt cannot authorize an ACK")
	require.True(t, d.hub.BeginSend("a", retryA))
	require.True(t, d.hub.AcknowledgeSent("a", key))
}

func TestExactDueTimeInCoarseBucketAndMaintenanceWithoutConsumers(t *testing.T) {
	d, ledger := newPipelineTest(t)
	d.TimeBucket = 10 * time.Second
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("universal", "orders", "", ch)
	now := time.Now().UnixMilli()
	key := putPipelineEvent(t, d, "orders", 1, now+10000)
	require.Empty(t, d.collectTopic("orders", now+20000).items, "a coarse range upper bound cannot authorize an early delivery")
	// Prepare a separate already due universal-only interest, then disconnect
	// the last local subscriber. Maintenance must still release its receipt.
	key = putPipelineEvent(t, d, "orders", 2, now-1000)
	state, err := ledger.Prepare("orders", key)
	require.NoError(t, err)
	require.True(t, state.Needs("u:universal"))
	d.hub.Unregister("universal")
	chunk := d.collectMaintenance()
	require.Len(t, chunk.items, 1)
	_, err = d.prepareChunk(context.Background(), chunk)
	require.NoError(t, err)
	_, _, err = d.db.Get(key)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestCompletionQueuesDeduplicateBoundAndRetainFailedBatches(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.deleter.AcknowledgeBatch = func([]raft.DeliveryAck) error { return errors.New("no quorum") }
	for i := 0; i < MaxPendingCompletions; i++ {
		key := utils.EventKey(1, 2, uint64(i))
		require.True(t, d.deleter.TryMarkAcknowledged(key, "g:a"))
		require.True(t, d.deleter.TryMarkAcknowledged(key, "g:a"))
	}
	require.Len(t, d.deleter.acks, MaxPendingCompletions)
	require.False(t, d.deleter.TryMarkAcknowledged(utils.EventKey(1, 2, MaxPendingCompletions), "g:a"))
	d.deleter.flush()
	require.Len(t, d.deleter.acks, MaxPendingCompletions)
	d.deleter.AcknowledgeBatch = func([]raft.DeliveryAck) error { return nil }
	d.deleter.flush()
	require.Empty(t, d.deleter.acks)
	require.Empty(t, d.deleter.ackKeys)
}

func TestImmutableOwnershipViewIncludesEveryGroupAndUniversalInterest(t *testing.T) {
	d, _ := newPipelineTest(t)
	h := d.hub
	h.Register("a-local", "orders", "a", make(chan *pb.QueueMessage, 1))
	h.Register("b-local", "orders", "b", make(chan *pb.QueueMessage, 1))
	h.Register("u-local", "orders", "", make(chan *pb.QueueMessage, 1))
	h.SetDeliveryFence(func() (uint64, bool) { return 9, true }, time.Second)
	h.SetDeliveryView(func(string) metadata.TopicDeliveryView {
		return metadata.TopicDeliveryView{Epoch: 9, Active: true, Recipients: []string{"g:a", "g:b", "u:remote", "u:u-local"}, Groups: map[string][]metadata.ConsumerMember{
			"a": {{ID: "a-local"}, {ID: "a-remote"}},
			"b": {{ID: "b-remote"}, {ID: "b-local"}},
			"":  {{ID: "u-local"}, {ID: "remote"}},
		}}
	})
	h.GrantDeliveryPermit(9, time.Now())
	view := h.InterestView("orders")
	require.True(t, view.Active)
	require.Equal(t, []string{"g:a", "g:b", "u:remote", "u:u-local"}, view.Recipients, "local filtering retains global interests")
	ownerIDs := func(id uint64) []string {
		var ids []string
		for _, owner := range view.localOwners(utils.EventKey(1, 2, id)) {
			ids = append(ids, owner.ID)
		}
		return ids
	}
	require.ElementsMatch(t, []string{"a-local", "u-local"}, ownerIDs(2))
	require.ElementsMatch(t, []string{"b-local", "u-local"}, ownerIDs(3))
	h.WithRebalanceBarrier(func() {})
	require.False(t, h.InterestView("orders").Active, "an immutable ownership decision cannot renew its own permit")
}

type observedIteratorDB struct {
	storage.DB
	open atomic.Int32
}
type observedIterator struct {
	storage.Iterator
	owner *observedIteratorDB
}

func (db *observedIteratorDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	iter, err := db.DB.NewIter(opts)
	if err != nil {
		return nil, err
	}
	db.open.Add(1)
	return &observedIterator{iter, db}, nil
}
func (iter *observedIterator) Close() error { iter.owner.open.Add(-1); return iter.Iterator.Close() }

func TestScanClosesSnapshotBeforePreparationAndBatchUsesOneProposal(t *testing.T) {
	d, ledger := newPipelineTest(t)
	observed := &observedIteratorDB{DB: d.db}
	d.db = observed
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 64))
	d.Limits.WorkTime = time.Hour
	for id := uint64(1); id <= 64; id++ {
		putPipelineEvent(t, d, "orders", id, time.Now().Add(-time.Second).UnixMilli())
	}
	var proposals int
	ledger.ProposeContext = func(ctx context.Context, cmd []byte) (statemachine.Result, error) {
		require.Zero(t, observed.open.Load(), "no snapshot can span a Raft wait")
		proposals++
		return ledger.apply(cmd)
	}
	require.Equal(t, 64, d.dispatchAll())
	require.Equal(t, 1, proposals)
	d.hub.ExpireInFlight(time.Now().Add(time.Hour))
	for i := 0; i < 64; i++ {
		<-d.hub.byID["local"].Ch
	}
	require.Equal(t, 64, d.dispatchAll())
	require.Equal(t, 1, proposals, "existing manifests require no preparation proposal")
}

func TestStalledACKsHaveBoundedTrackingEvenWithFastSender(t *testing.T) {
	d, _ := newPipelineTest(t)
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	for id := uint64(0); id < 1024; id++ {
		key := utils.EventKey(1, 2, id)
		require.NotEmpty(t, d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, key))
		<-ch
	}
	require.Empty(t, d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, utils.EventKey(1, 2, 1024)))
	require.Len(t, d.hub.deliveryRecords["local"], 1024)
	d.hub.ExpireInFlight(time.Now().Add(time.Hour))
	require.Empty(t, d.hub.deliveryRecords["local"])
	require.NotEmpty(t, d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, utils.EventKey(1, 2, 1024)))
}
