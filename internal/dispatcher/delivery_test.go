package dispatcher

import (
	"errors"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestStandaloneRecipientAckFailureRetainsPayloadAndRetries(t *testing.T) {
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	a := make(chan *pb.QueueMessage, 1)
	b := make(chan *pb.QueueMessage, 1)
	hub.Register("a", "orders", "a", a)
	hub.Register("b", "orders", "b", b)
	ledger := NewDeliveryLedger(db, hub.LocalDeliveryRecipients, NewDirectDeleteBackend(db, zap.NewNop()), nil)
	deleter := NewDeleter(ledger, time.Hour, zap.NewNop())
	fail := true
	deleter.AcknowledgeBatch = func(acks []raft.DeliveryAck) error {
		if fail {
			fail = false
			return errors.New("temporary failure")
		}
		return ledger.Acknowledge(acks)
	}
	key := utils.EventKey(1, utils.TopicHash("orders"), 1)
	batch := db.NewBatch()
	require.NoError(t, batch.Set(key, []byte("payload")))
	require.NoError(t, batch.Commit(storage.Sync))
	require.NoError(t, batch.Close())
	manifest, err := ledger.Prepare("orders", key)
	require.NoError(t, err)
	require.Len(t, hub.DispatchPrepared("orders", &pb.QueueMessage{}, key, manifest), 2)
	<-a
	<-b
	require.True(t, hub.RemoveInFlightForConsumer("a", key))
	deleter.MarkAcknowledged(key, "g:a")
	deleter.flush()
	require.Len(t, deleter.acks, 1)
	deleter.flush()
	require.Empty(t, deleter.acks)
	manifest, err = ledger.Prepare("orders", key)
	require.NoError(t, err)
	require.False(t, manifest.Needs("g:a"))
	require.True(t, manifest.Needs("g:b"))
	hub.ExpireInFlight(time.Now().Add(time.Minute))
	require.Equal(t, []string{"b"}, hub.DispatchPrepared("orders", &pb.QueueMessage{}, key, manifest))
	require.Empty(t, a, "an ACKed group is skipped even after local in-flight timeout")
	<-b
	require.True(t, hub.RemoveInFlightForConsumer("b", key))
	deleter.MarkAcknowledged(key, "g:b")
	deleter.flush()
	_, _, err = db.Get(key)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestPersistedAckReleasesOnlyCompletedRecipients(t *testing.T) {
	d, ledger := newPipelineTest(t)
	hub := d.hub
	groups := map[string]string{"a": "a", "b": "b", "u": "", "v": ""}
	queues := make(map[string]chan *pb.QueueMessage)
	for id, group := range groups {
		queues[id] = make(chan *pb.QueueMessage, 1)
		hub.Register(id, "orders", group, queues[id])
	}
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	require.Equal(t, 1, d.dispatchAll())
	for id, queue := range queues {
		require.True(t, hub.BeginSend(id, <-queue))
	}
	d.deleter.AcknowledgeBatch = ledger.Acknowledge
	d.deleter.OnAcknowledged = hub.RemoveAcknowledgedBatch
	for _, ack := range []raft.DeliveryAck{{Key: key, Recipient: "g:a"}, {Key: key, Recipient: "u:u"}} {
		id := "a"
		if ack.Recipient == "u:u" {
			id = "u"
		}
		require.True(t, hub.MarkAcknowledgedSent(id, key))
		require.True(t, d.deleter.TryMarkAcknowledged(key, ack.Recipient))
		require.True(t, hub.HasInFlightForConsumer(id, key), "retain tracking before commit")
	}
	d.deleter.flush()
	require.False(t, hub.HasInFlightForConsumer("a", key))
	require.False(t, hub.HasInFlightForConsumer("u", key))
	require.True(t, hub.HasInFlightForConsumer("b", key), "another group's delivery is still pending")
	require.True(t, hub.HasInFlightForConsumer("v", key), "another universal recipient is still pending")
	manifest, err := raft.ReadDeliveryState(d.db, key)
	require.NoError(t, err)
	require.False(t, manifest.Needs("g:a"))
	require.False(t, manifest.Needs("u:u"))
	require.True(t, manifest.Needs("g:b"))
	require.True(t, manifest.Needs("u:v"))
	_, closer, err := d.db.Get(key)
	require.NoError(t, err, "shared payload is retained for incomplete recipients")
	require.NoError(t, closer.Close())
	require.Zero(t, d.dispatchAll(), "committed recipients must not be redelivered")
}
