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
