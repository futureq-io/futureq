package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type RetryExpirySuite struct{ suite.Suite }

func TestRetryExpirySuite(t *testing.T) {
	suite.Run(t, new(RetryExpirySuite))
}

func (s *RetryExpirySuite) TestSweepDrainsMultipleBatchesAcrossTopics() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	topics := []string{"orders", "emails"}
	receipt, err := raft.MarshalPublishReceipt(0, []string{"g:workers"})
	s.Require().NoError(err)
	token := bytes.Repeat([]byte{1}, raft.AttemptTokenSize)
	for i := range 96 {
		topic := topics[i%len(topics)]
		key := utils.EventKey(1, utils.TopicHash(topic), uint64(i+1))
		message, err := proto.Marshal(&storagepb.StoredMessage{
			Topic: topic, Payload: []byte("timeout"), EnqueuedAtUnixMs: 100, MaxRetries: new(uint32(0)),
		})
		s.Require().NoError(err)
		batch := db.NewBatch()
		s.Require().NoError(batch.Set(key, message))
		s.Require().NoError(batch.Set(raft.DeliveryStateKey(key), receipt))
		s.Require().NoError(batch.Commit(storage.Sync))
		s.Require().NoError(batch.Close())
		target := &deliverypb.DeliveryAttemptTarget{Key: key, Recipient: "g:workers", Token: token, DeadlineMs: 110}
		result, err := ledger.ApplyRetry(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_START, NowMs: 100, Attempts: []*deliverypb.DeliveryAttemptTarget{target},
		})
		s.Require().NoError(err)
		s.Require().Equal(uint64(1), result)
		target.DeadlineMs = 0
		_, err = ledger.ApplyRetry(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_SENT, NowMs: 100, Attempts: []*deliverypb.DeliveryAttemptTarget{target},
		})
		s.Require().NoError(err)
	}

	s.Require().NoError(ledger.expireDueRetryAttempts(context.Background(), 110, nil))
	due, err := raft.DueDeliveryAttempts(db, 110)
	s.Require().NoError(err)
	s.Empty(due)
	for _, topic := range topics {
		page, err := (raft.DeadLetterQuery{Topic: topic, Limit: 100}).Read(db)
		s.Require().NoError(err)
		s.Len(page.Page.Entries, 48)
	}
}

func (s *RetryExpirySuite) TestReceivedACKSurvivesExpiryWhileOutcomeWorkersAreBlocked() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	local := NewDeliveryLedger(db, nil, nil, nil)
	started := make(chan struct{}, 2)
	unblock := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_ACK {
			started <- struct{}{}
			<-unblock
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	})
	ledger.ConfigureRetryOutcomes(4)
	done := make(chan struct{})
	go func() { defer close(done); ledger.RunRetryOutcomes(ctx, 2, logger) }()
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
		cancel()
		<-done
	}()

	topic := "orders"
	receipt, err := raft.MarshalPublishReceipt(0, []string{"g:workers"})
	s.Require().NoError(err)
	body, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, EnqueuedAtUnixMs: 100, MaxRetries: new(uint32(0))})
	s.Require().NoError(err)
	committed := make(chan struct{}, 3)
	for i := range 3 {
		key := utils.EventKey(1, utils.TopicHash(topic), uint64(i+1))
		batch := db.NewBatch()
		s.Require().NoError(batch.Set(key, body))
		s.Require().NoError(batch.Set(raft.DeliveryStateKey(key), receipt))
		s.Require().NoError(batch.Commit(storage.Sync))
		s.Require().NoError(batch.Close())
		target := &deliverypb.DeliveryAttemptTarget{
			Key: key, Recipient: "g:workers", Token: bytes.Repeat([]byte{byte(i + 1)}, raft.AttemptTokenSize), DeadlineMs: 110,
		}
		_, err := ledger.ApplyRetry(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_START, NowMs: 100, Attempts: []*deliverypb.DeliveryAttemptTarget{target},
		})
		s.Require().NoError(err)
		target.DeadlineMs = 0
		value, err := ledger.ApplyRetry(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_SENT, NowMs: 101, Attempts: []*deliverypb.DeliveryAttemptTarget{target},
		})
		s.Require().NoError(err)
		s.Require().Equal(uint64(1), value)
		isAccepted := ledger.TryRetryOutcome(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_ACK, NowMs: 109, Attempts: []*deliverypb.DeliveryAttemptTarget{target},
		}, func() { committed <- struct{}{} })
		s.Require().True(isAccepted)
	}
	s.Require().Eventually(func() bool { return len(started) == 2 }, time.Second, time.Millisecond)
	s.Require().NoError(ledger.expireDueRetryAttempts(ctx, 110, nil))
	page, err := (raft.DeadLetterQuery{Topic: topic, Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
	due, err := raft.DueDeliveryAttempts(db, 110)
	s.Require().NoError(err)
	s.Len(due, 3)

	<-started
	<-started
	close(unblock)
	s.Require().Eventually(func() bool { return len(committed) == 3 }, time.Second, time.Millisecond)
	due, err = raft.DueDeliveryAttempts(db, 110)
	s.Require().NoError(err)
	s.Empty(due)
}

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
