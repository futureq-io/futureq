package dispatcher

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

func TestStandaloneGroupReservesMessageAcrossMembersAndScans(t *testing.T) {
	d, _ := newPipelineTest(t)
	first, second := make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2)
	d.hub.Register("first", "orders", "workers", first)
	d.hub.Register("second", "orders", "workers", second)
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	require.Equal(t, 1, d.dispatchAll())
	msg := <-first
	require.Equal(t, key, msg.DeliveryTag)
	require.True(t, d.hub.BeginSend("first", msg))
	for range 5 {
		require.Zero(t, d.dispatchAll(), "another group member must not receive an in-flight key")
	}
	// A membership epoch change must preserve the current local reservation.
	third := make(chan *pb.QueueMessage, 1)
	d.hub.Register("third", "orders", "workers", third)
	require.Zero(t, d.dispatchAll())
	require.Empty(t, first)
	require.Empty(t, second)
	require.Empty(t, third)
}

func TestStandaloneGroupHoldsReservationUntilAckPersistence(t *testing.T) {
	d, ledger := newPipelineTest(t)
	first, second := make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2)
	other := make(chan *pb.QueueMessage, 2)
	d.hub.Register("first", "orders", "workers", first)
	d.hub.Register("second", "orders", "workers", second)
	d.hub.Register("other", "orders", "archive", other)
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	require.Equal(t, 1, d.dispatchAll())
	require.True(t, d.hub.BeginSend("first", <-first))
	<-other
	require.True(t, d.hub.MarkAcknowledgedSent("first", key))
	d.deleter.OnAcknowledged = d.hub.RemoveAcknowledgedBatch
	fail := true
	d.deleter.AcknowledgeBatch = func(acks []raft.DeliveryAck) error {
		if fail {
			return errors.New("temporary persistence failure")
		}
		return ledger.Acknowledge(acks)
	}
	require.True(t, d.deleter.TryMarkAcknowledged(key, "g:workers"))
	require.Zero(t, d.dispatchAll(), "an ACK waiting for a flush still reserves the group")
	d.deleter.flush()
	require.Zero(t, d.dispatchAll(), "a failed ACK flush must retain the reservation")
	require.Len(t, d.hub.groupInFlight, 2)
	fail = false
	d.deleter.flush()
	require.Len(t, d.hub.groupInFlight, 1, "only the completed group releases its reservation")
	d.hub.ExpireInFlight(time.Now().Add(time.Minute))
	require.Equal(t, 1, d.dispatchAll(), "the other group still needs the shared payload")
	require.Len(t, other, 1)
	require.Empty(t, first)
	require.Empty(t, second, "a durably completed group must not receive a retry")
}

func TestStandaloneGroupReservationReleasePaths(t *testing.T) {
	for _, path := range []string{"timeout", "nack", "disconnect", "rejected_queue", "delete", "remove", "ack"} {
		t.Run(path, func(t *testing.T) {
			d, _ := newPipelineTest(t)
			h := d.hub
			first, second := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
			h.Register("first", "orders", "workers", first)
			h.Register("second", "orders", "workers", second)
			key := []byte("key")
			require.NotEmpty(t, h.DispatchToTopic("orders", &pb.QueueMessage{}, key))
			msg := <-first
			if path == "nack" || path == "ack" {
				require.True(t, h.BeginSend("first", msg))
			}
			for range 2 {
				switch path {
				case "timeout":
					h.ExpireInFlight(time.Now().Add(time.Minute))
				case "nack":
					h.AcknowledgeSent("first", key)
				case "disconnect":
					h.Unregister("first")
				case "rejected_queue":
					h.RejectQueuedAttempt("first", msg)
				case "delete":
					h.RemoveDeletedBatch([][]byte{key})
				case "remove":
					h.RemoveInFlightForConsumer("first", key)
				case "ack":
					h.RemoveAcknowledgedBatch([]raft.DeliveryAck{{Key: key, Recipient: "g:workers"}})
				}
			}
			require.Empty(t, h.groupInFlight)
			// Direct Hub use has no durable manifest: verify that a released
			// reservation can be acquired by another member, exactly once.
			require.NotEmpty(t, h.DispatchToTopic("orders", &pb.QueueMessage{}, key))
			retry := <-second
			h.RejectQueuedAttempt("first", msg)
			require.True(t, h.CanSend("second", retry), "a stale attempt cannot erase the new owner's reservation")
			require.Empty(t, h.DispatchToTopic("orders", &pb.QueueMessage{}, key))
			require.Len(t, h.groupInFlight, 1)
		})
	}
}

func TestStandaloneGroupReservationIsAtomic(t *testing.T) {
	d, _ := newPipelineTest(t)
	for _, id := range []string{"first", "second", "third"} {
		d.hub.Register(id, "orders", "workers", make(chan *pb.QueueMessage, 32))
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	var sent atomic.Int32
	for range 32 {
		workers.Go(func() {
			<-start
			sent.Add(int32(len(d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, []byte("key")))))
		})
	}
	close(start)
	workers.Wait()
	require.EqualValues(t, 1, sent.Load(), "concurrent dispatches must share one group reservation")
	require.Len(t, d.hub.groupInFlight, 1)
}

func TestStandaloneReservationsKeepGroupsTopicsAndUniversalConsumersIndependent(t *testing.T) {
	d, _ := newPipelineTest(t)
	h := d.hub
	// The first two subscriptions also collide if topic and group are joined
	// with an unescaped delimiter rather than stored as separate fields.
	subscriptions := []struct{ id, topic, group string }{
		{"a", "orders|workers", "workers"},
		{"b", "orders", "workers|workers"},
		{"c", "orders", "workers"},
		{"d", "orders", "workers"},
		{"u", "orders", ""},
		{"v", "orders", ""},
	}
	for _, sub := range subscriptions {
		h.Register(sub.id, sub.topic, sub.group, make(chan *pb.QueueMessage, 2))
	}
	key := []byte("same-key")
	require.Len(t, h.DispatchToTopic("orders|workers", &pb.QueueMessage{}, key), 1)
	require.Len(t, h.DispatchToTopic("orders", &pb.QueueMessage{}, key), 4)
	require.Empty(t, h.DispatchToTopic("orders", &pb.QueueMessage{}, key))
	require.Len(t, h.groupInFlight, 3)
}

func TestStandaloneFullQueueDoesNotReserveGroup(t *testing.T) {
	d, _ := newPipelineTest(t)
	first, second := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
	first <- &pb.QueueMessage{}
	d.hub.Register("first", "orders", "workers", first)
	d.hub.Register("second", "orders", "workers", second)
	require.Empty(t, d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, []byte("key")))
	require.Empty(t, d.hub.groupInFlight)
	require.NotEmpty(t, d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, []byte("key")))
	require.Len(t, second, 1)
}

func TestBeginSendGrantsFullAckWindowAfterQueueWait(t *testing.T) {
	d, _ := newPipelineTest(t)
	h := d.hub
	h.SetDeliveryTimeout(time.Minute)
	ch := make(chan *pb.QueueMessage, 1)
	h.Register("only", "orders", "workers", ch)
	key := []byte("key")
	require.NotEmpty(t, h.DispatchToTopic("orders", &pb.QueueMessage{}, key))
	msg := <-ch
	// Model an attempt that spent most of its queue lifetime waiting behind a
	// slow Send, without wall-clock sleeps in the regression test.
	h.inFlightMu.Lock()
	record := h.deliveryRecords["only"][string(key)]
	record.expires = time.Now().Add(10 * time.Second)
	queueDeadline := record.expires
	h.deliveryRecords["only"][string(key)] = record
	h.inFlightMu.Unlock()
	started := time.Now()
	require.True(t, h.BeginSend("only", msg))
	require.False(t, h.BeginSend("only", msg), "one attempt cannot repeatedly extend its ACK deadline")
	h.ExpireInFlight(queueDeadline.Add(time.Second))
	require.True(t, h.HasInFlightForConsumer("only", key), "queue time must not shorten the sent attempt's ACK window")
	require.Empty(t, h.DispatchToTopic("orders", &pb.QueueMessage{}, key))
	h.inFlightMu.Lock()
	sendDeadline := h.deliveryRecords["only"][string(key)].expires
	h.inFlightMu.Unlock()
	require.False(t, sendDeadline.Before(started.Add(time.Minute)))
	h.ExpireInFlight(sendDeadline.Add(time.Nanosecond))
	require.Empty(t, h.groupInFlight, "sent attempts must still time out")
}

// Inject at the storage commit boundary, after the ledger has built its real
// ACK/delete batch. Successful attempts still commit to the underlying DB.
type standaloneAckCommitDB struct {
	storage.DB
	beforeCommit func(storage.SyncMode) error
}

func (db *standaloneAckCommitDB) NewBatch() storage.Batch {
	return &standaloneAckCommitBatch{Batch: db.DB.NewBatch(), beforeCommit: db.beforeCommit}
}

type standaloneAckCommitBatch struct {
	storage.Batch
	beforeCommit func(storage.SyncMode) error
}

func (b *standaloneAckCommitBatch) Commit(mode storage.SyncMode) error {
	if err := b.beforeCommit(mode); err != nil {
		return err
	}
	return b.Batch.Commit(mode)
}

func TestStandaloneFinalAckCommitsBeforeDeletingTracking(t *testing.T) {
	d, ledger := newPipelineTest(t)
	h := d.hub
	topic := t.Name()
	h.SetDeliveryTimeout(time.Minute)
	first, second := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
	h.Register("first", topic, "workers", first)
	h.Register("second", topic, "workers", second)
	t.Cleanup(func() { h.Unregister("first"); h.Unregister("second") })
	key := putPipelineEvent(t, d, topic, 1, time.Now().Add(-time.Second).UnixMilli())
	require.Equal(t, 1, d.dispatchAll())
	require.True(t, h.BeginSend("first", <-first))
	require.True(t, h.MarkAcknowledgedSent("first", key))

	assertStored := func(present bool) {
		t.Helper()
		for _, storedKey := range [][]byte{key, raft.DeliveryStateKey(key)} {
			_, closer, err := d.db.Get(storedKey)
			if present {
				require.NoError(t, err)
				require.NoError(t, closer.Close())
			} else {
				require.ErrorIs(t, err, storage.ErrNotFound)
			}
		}
	}
	assertTracked := func() {
		t.Helper()
		require.True(t, h.HasInFlightForConsumer("first", key))
		require.Len(t, h.groupInFlight, 1)
		require.Equal(t, float64(1), inFlightGaugeValue(t, topic, "workers"))
	}
	assertCleared := func() {
		t.Helper()
		require.False(t, h.HasInFlightForConsumer("first", key))
		require.Empty(t, h.deliveryRecords["first"])
		require.Empty(t, h.groupInFlight)
		require.Zero(t, inFlightGaugeValue(t, topic, "workers"))
	}
	var callbacks []string
	// Match startRun's production wiring, with observations around the calls.
	ledger.OnDelete = func(keys [][]byte) {
		require.Equal(t, []string{"commit"}, callbacks)
		require.Equal(t, [][]byte{key}, keys)
		assertStored(false) // Both deletes must be visible before this callback.
		assertTracked()
		d.RemoveInFlightBatch(keys)
		h.RemoveDeletedBatch(keys)
		assertCleared()
		callbacks = append(callbacks, "delete")
	}
	d.deleter.AcknowledgeBatch = ledger.Acknowledge
	d.deleter.OnAcknowledged = func(acks []raft.DeliveryAck) {
		require.Equal(t, []string{"commit", "delete"}, callbacks)
		require.Equal(t, []raft.DeliveryAck{{Key: key, Recipient: "g:workers"}}, acks)
		assertStored(false)
		assertCleared()
		h.RemoveAcknowledgedBatch(acks)
		assertCleared() // The second cleanup must not decrement the gauge again.
		callbacks = append(callbacks, "acknowledged")
	}
	d.deleter.OnDelete = func(key []byte) {
		d.RemoveInFlight(key)
		h.RemoveDeletedBatch([][]byte{key})
	}
	commitAttempts := 0
	ledger.db = &standaloneAckCommitDB{DB: d.db, beforeCommit: func(mode storage.SyncMode) error {
		commitAttempts++
		require.Equal(t, storage.Sync, mode)
		assertStored(true)
		assertTracked()
		if commitAttempts == 1 {
			return errors.New("injected storage batch commit failure")
		}
		callbacks = append(callbacks, "commit")
		return nil
	}}
	require.True(t, d.deleter.TryMarkAcknowledged(key, "g:workers"))
	d.deleter.flush()
	require.Equal(t, 1, commitAttempts)
	require.Empty(t, callbacks, "failed storage commits must not run completion callbacks")
	assertStored(true)
	assertTracked()
	state, err := raft.ReadDeliveryState(d.db, key)
	require.NoError(t, err)
	require.True(t, state.Needs("g:workers"), "the failed final ACK must not persist a completion")
	require.Len(t, d.deleter.acks, 1, "the final ACK must remain queued for retry")
	require.Zero(t, d.dispatchAll(), "the second member cannot acquire the reserved message")
	require.Empty(t, second)

	d.deleter.flush()
	require.Equal(t, 2, commitAttempts)
	require.Equal(t, []string{"commit", "delete", "acknowledged"}, callbacks)
	assertStored(false)
	assertCleared()
	require.Empty(t, d.deleter.acks)
	require.Empty(t, d.deleter.ackKeys)
	d.deleter.flush()
	require.Equal(t, 2, commitAttempts, "completed ACKs must not commit again")
	require.Zero(t, d.dispatchAll())
	require.Empty(t, second)
	assertCleared()
}
