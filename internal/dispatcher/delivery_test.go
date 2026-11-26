package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
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

func (s *RetryExpirySuite) storeRetryFixture(db storage.DB, eventID uint64, deadline time.Time) *deliverypb.RetryCommand {
	topic, recipient := "orders", "g:workers"
	key := utils.EventKey(1, utils.TopicHash(topic), eventID)
	receipt, err := raft.MarshalPublishReceipt(0, []string{recipient})
	s.Require().NoError(err)
	body, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, EnqueuedAtUnixMs: 100, MaxRetries: new(uint32(0))})
	s.Require().NoError(err)
	batch := db.NewBatch()
	defer batch.Close() //nolint:errcheck
	s.Require().NoError(batch.Set(key, body))
	s.Require().NoError(batch.Set(raft.DeliveryStateKey(key), receipt))
	s.Require().NoError(batch.Commit(storage.Sync))
	return &deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_START, NowMs: time.Now().UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: key, Recipient: recipient, Token: bytes.Repeat([]byte{byte(eventID)}, raft.AttemptTokenSize),
			DeadlineMs: deadline.UnixMilli(),
		}},
	}
}

func (s *RetryExpirySuite) TestAuthorizationCapacityIsCheckedBeforeReservation() {
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	deadline := time.Now().Add(time.Minute)
	first, second := s.storeRetryFixture(db, 1, deadline), s.storeRetryFixture(db, 2, deadline)
	entered, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	ready := func() (time.Time, error) { return deadline, nil }
	go func() {
		_, err := ledger.AuthorizeRetrySend(first, time.Minute, func(uint64) (bool, error) {
			close(entered)
			<-finish
			return true, nil
		}, ready)
		done <- err
	}()
	defer func() {
		if finish != nil {
			close(finish)
			s.NoError(<-done)
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		s.FailNow("first authorization did not reserve capacity")
	}
	allowed, err := ledger.AuthorizeRetrySend(second, time.Minute, func(uint64) (bool, error) {
		s.Fail("second authorization should not reserve an attempt")
		return true, nil
	}, ready)
	s.Require().NoError(err)
	s.False(allowed)
	due, err := raft.DueDeliveryAttempts(db, deadline.UnixMilli())
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	s.Equal(first.Attempts[0].Key, due[0].Key)
	state, err := raft.ReadDeliveryState(db, second.Attempts[0].Key)
	s.Require().NoError(err)
	s.True(state.Ready(second.Attempts[0].Recipient))
	close(finish)
	s.NoError(<-done)
	finish = nil
	allowed, err = ledger.AuthorizeRetrySend(second, time.Minute, func(attempt uint64) (bool, error) {
		s.Equal(uint64(1), attempt)
		return true, nil
	}, ready)
	s.Require().NoError(err)
	s.True(allowed)
}

func (s *RetryExpirySuite) TestStalledRefundDoesNotBlockUnrelatedExpiry() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	local := NewDeliveryLedger(db, nil, nil, nil)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var cancelCalls atomic.Int32
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_CANCEL_RESERVATION {
			if cancelCalls.Add(1) == 1 {
				return statemachine.Result{}, errors.New("initial refund failed")
			}
			close(entered)
			<-unblock
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	})
	deadline := time.Now().Add(600 * time.Millisecond)
	first, second := s.storeRetryFixture(db, 1, deadline), s.storeRetryFixture(db, 2, deadline)
	prepared := func(uint64) (bool, error) { return true, nil }
	allowed, err := ledger.AuthorizeRetrySend(first, 600*time.Millisecond, prepared, func() (time.Time, error) { return time.Time{}, nil })
	s.Require().NoError(err)
	s.False(allowed)
	allowed, err = ledger.AuthorizeRetrySend(second, 600*time.Millisecond, prepared, func() (time.Time, error) { return deadline, nil })
	s.Require().NoError(err)
	s.Require().True(allowed)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ledger.RunRetryExpiry(ctx, time.Millisecond, nil, logger)
	}()
	defer func() { close(unblock); cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		s.FailNow("refund worker did not start")
	}
	s.Require().Eventually(func() bool {
		page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(db)
		return err == nil && len(page.Page.Entries) == 1 && bytes.HasPrefix(page.Page.Entries[0].Id, second.Attempts[0].Key)
	}, 3*time.Second, 10*time.Millisecond)
}

func (s *RetryExpirySuite) TestFullOutcomeQueueDoesNotDropAnUnsentRefund() {
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	local := NewDeliveryLedger(db, nil, nil, nil)
	cancelFailures := 2
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_CANCEL_RESERVATION && cancelFailures > 0 {
			cancelFailures--
			return statemachine.Result{}, errors.New("transient cancellation failure")
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	})
	ledger.ConfigureRetryOutcomes(1)
	topic, recipient := "orders", "g:workers"
	key := utils.EventKey(1, utils.TopicHash(topic), 1)
	token := bytes.Repeat([]byte{1}, raft.AttemptTokenSize)
	now := time.Now()
	s.Require().True(ledger.TryRetryOutcome(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_ACK, NowMs: 101,
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: utils.EventKey(1, utils.TopicHash(topic), 2), Recipient: recipient, Token: token,
		}},
	}, func() {}))

	receipt, err := raft.MarshalPublishReceipt(0, []string{recipient})
	s.Require().NoError(err)
	body, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, EnqueuedAtUnixMs: 100, MaxRetries: new(uint32(0))})
	s.Require().NoError(err)
	batch := db.NewBatch()
	s.Require().NoError(batch.Set(key, body))
	s.Require().NoError(batch.Set(raft.DeliveryStateKey(key), receipt))
	s.Require().NoError(batch.Commit(storage.Sync))
	s.Require().NoError(batch.Close())
	deadline := now.Add(time.Second).UnixMilli()
	target := &deliverypb.DeliveryAttemptTarget{Key: key, Recipient: recipient, Token: token, DeadlineMs: deadline}
	start := &deliverypb.RetryCommand{Operation: deliverypb.RetryCommand_START, NowMs: now.UnixMilli(), Attempts: []*deliverypb.DeliveryAttemptTarget{target}}
	allowed, err := ledger.AuthorizeRetrySend(start, time.Second, func(attempt uint64) (bool, error) {
		s.Equal(uint64(1), attempt)
		return true, nil
	}, func() (time.Time, error) { return time.Time{}, nil })
	s.False(allowed)
	s.NoError(err)

	ctx := context.Background()
	s.Require().ErrorContains(ledger.flushRetryRefunds(ctx), "transient cancellation failure")
	s.Require().NoError(ledger.expireDueRetryAttempts(ctx, deadline, nil))
	page, err := (raft.DeadLetterQuery{Topic: topic, Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
	s.Require().NoError(ledger.flushRetryRefunds(ctx))
	s.Len(ledger.retryOutcomes, 1, "refund does not require a slot in the outcome queue")
	target.DeadlineMs, target.Token = deadline+10, bytes.Repeat([]byte{2}, raft.AttemptTokenSize)
	start.NowMs = deadline + 1
	attempt, err := ledger.ApplyRetry(start)
	s.Require().NoError(err)
	s.Equal(uint64(1), attempt, "the unsent delivery must retain its first attempt")
}

func (s *RetryExpirySuite) TestOwnerRenewalUsesConfiguredLease() {
	const lease = 250 * time.Millisecond
	const maxClockSkew = 1500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	renewed := make(chan time.Duration, 2)
	ledger := NewDeliveryLedger(nil, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		select {
		case renewed <- time.Duration(cmd.LeaseUntilMs-cmd.NowMs) * time.Millisecond:
		case <-ctx.Done():
		}
		return statemachine.Result{Value: uint64(cmd.LeaseUntilMs)}, nil
	})
	ledger.SetRetryOwner(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ledger.RunRetryOwner(ctx, lease, maxClockSkew, zap.NewNop())
	}()
	defer func() { cancel(); <-done }()
	for range 2 {
		select {
		case duration := <-renewed:
			s.Equal(lease+maxClockSkew, duration)
		case <-ctx.Done():
			s.FailNow("retry owner renewal did not run")
		}
	}
	s.Require().Eventually(ledger.retryOwnerActive, time.Second, time.Millisecond)
	s.LessOrEqual(time.Until(*ledger.retry.until.Load()), lease, "the recovery margin cannot extend local admission")
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
	brokerCtx, stopBroker := context.WithCancel(context.Background())
	defer stopBroker()
	propose := func(ctx context.Context, raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_ACK {
			started <- struct{}{}
			select {
			case <-unblock:
			case <-ctx.Done():
				return statemachine.Result{}, ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			return statemachine.Result{}, err
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	}
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		return propose(brokerCtx, raw)
	})
	ledger.ProposeContext = propose
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
		value, err := ledger.ApplyRetry(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_SENT, NowMs: 101, Attempts: []*deliverypb.DeliveryAttemptTarget{target},
		})
		s.Require().NoError(err)
		s.Require().Equal(uint64(1), value)
		target.DeadlineMs = 0
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
	stopBroker()
	ledger.StopRetryOutcomeAdmission()
	s.False(ledger.TryRetryOutcome(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_ACK, NowMs: 109,
		Attempts: []*deliverypb.DeliveryAttemptTarget{due[0]},
	}, func() {}), "shutdown rejects new outcomes")
	close(unblock)
	select {
	case <-done:
	case <-time.After(time.Second):
		s.FailNow("outcome workers did not finish draining")
	}
	s.Len(committed, 3, "shutdown must commit accepted outcomes, including the buffered ACK")
	s.Require().NoError(NewDeliveryLedger(db, nil, nil, nil).expireDueRetryAttempts(ctx, 110, nil))
	page, err = (raft.DeadLetterQuery{Topic: topic, Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries, "recovery must not archive acknowledged messages")
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
