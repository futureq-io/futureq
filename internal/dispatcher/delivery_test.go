package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/internal/subscriptions"
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
		_, err := ledger.AuthorizeRetrySend(context.Background(), first, time.Minute, func(uint64) (bool, error) {
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
	allowed, err := ledger.AuthorizeRetrySend(context.Background(), second, time.Minute, func(uint64) (bool, error) {
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
	allowed, err = ledger.AuthorizeRetrySend(context.Background(), second, time.Minute, func(attempt uint64) (bool, error) {
		s.Equal(uint64(1), attempt)
		return true, nil
	}, ready)
	s.Require().NoError(err)
	s.True(allowed)
}

func (s *RetryExpirySuite) TestRetrySendActiveStopsAtMessageTTL() {
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	deadline := time.Now().Add(time.Minute)
	start := s.storeRetryFixture(db, 1, deadline)
	target := start.Attempts[0]
	defer ledger.RemoveRetryTransports([][]byte{target.Key})
	enqueuedAtMs := time.Now().UnixMilli()
	const ttlMs = 500
	payload, err := proto.Marshal(&storagepb.StoredMessage{
		Topic: "orders", EnqueuedAtUnixMs: enqueuedAtMs, TtlMs: ttlMs, MaxRetries: new(uint32(0)),
	})
	s.Require().NoError(err)
	batch := db.NewBatch()
	defer batch.Close() //nolint:errcheck
	s.Require().NoError(batch.Set(target.Key, payload))
	s.Require().NoError(batch.Commit(storage.Sync))

	allowed, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Minute,
		func(uint64) (bool, error) { return true, nil },
		func() (time.Time, error) { return deadline, nil })
	s.Require().NoError(err)
	s.Require().True(allowed)
	tag := append(bytes.Clone(target.Key), target.Token...)
	s.Require().True(ledger.BeginRetryTransport(tag, func() bool { return true }))
	active, err := ledger.RetrySendActive(target.Key, target.Recipient, target.Token)
	s.Require().NoError(err)
	s.True(active)

	time.Sleep(time.Until(time.UnixMilli(enqueuedAtMs + ttlMs)))
	active, err = ledger.RetrySendActive(target.Key, target.Recipient, target.Token)
	s.Require().NoError(err)
	s.False(active, "the sender must skip remaining chunks after the TTL")
	state, err := raft.ReadDeliveryState(db, target.Key)
	s.Require().NoError(err)
	s.True(state.HasActiveAttempt(target.Recipient, target.Token), "the ACK window is separate from the message TTL")
}

func (s *RetryExpirySuite) TestFirstFrameRejectedAfterAttemptTimeoutWithoutTTL() {
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	local := NewDeliveryLedger(db, nil, nil, nil)
	claimed, commit := make(chan struct{}), make(chan struct{})
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_EXPIRE {
			close(claimed)
			<-commit
		}
		return local.applyRetryResult(ctx, &cmd)
	})
	ledger.ConfigureRetryOutcomes(1)
	start := s.storeRetryFixture(db, 1, time.Now().Add(time.Minute))
	target := start.Attempts[0]
	defer ledger.RemoveRetryTransports([][]byte{target.Key})
	const ackWindow = 100 * time.Millisecond
	var deadline time.Time
	allowed, err := ledger.AuthorizeManualRetrySend(ctx, start, ackWindow,
		func(uint64) (bool, error) { return true, nil },
		func() (time.Time, error) {
			deadline = time.Now().Add(ackWindow)
			return deadline, nil
		})
	s.Require().NoError(err)
	s.Require().True(allowed)

	time.Sleep(time.Until(deadline))
	tag := append(bytes.Clone(target.Key), target.Token...)
	canSend := func() bool { return true }
	s.False(ledger.BeginRetryTransport(tag, canSend), "the local ACK deadline ended")
	done := make(chan struct{})
	var expiryErr error
	go func() {
		defer close(done)
		expiryErr = ledger.expireDueRetryAttempts(ctx, time.Now().UnixMilli(), nil)
	}()
	commitExpiry := sync.OnceFunc(func() { close(commit) })
	defer func() {
		commitExpiry()
		<-done
	}()
	select {
	case <-claimed:
	case <-time.After(time.Second):
		s.FailNow("expiry did not claim the attempt")
	}
	s.False(ledger.BeginRetryTransport(tag, canSend), "expiry owns the attempt even before its proposal commits")
	commitExpiry()
	<-done
	s.Require().NoError(expiryErr)
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 1}).Read(db)
	s.Require().NoError(err)
	s.Require().Len(page.Page.Entries, 1)
	s.Equal(pb.DeadLetterReason_DEAD_LETTER_REASON_TIMEOUT, page.Page.Entries[0].GetReason())
}

func (s *RetryExpirySuite) TestExpiredFirstFrameStaysProtectedUntilRefundCommits() {
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	local := NewDeliveryLedger(db, nil, nil, nil)
	failRefund := true
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_CANCEL_RESERVATION && failRefund {
			failRefund = false
			return statemachine.Result{}, errors.New("transient refund failure")
		}
		return local.applyRetryResult(nil, &cmd)
	})
	ledger.ConfigureRetryOutcomes(1)
	start := s.storeRetryFixture(db, 1, time.Now().Add(time.Minute))
	target := start.Attempts[0]
	defer ledger.RemoveRetryTransports([][]byte{target.Key})
	enqueuedAtMs := time.Now().UnixMilli()
	const ttlMs = 500
	payload, err := proto.Marshal(&storagepb.StoredMessage{
		Topic: "orders", EnqueuedAtUnixMs: enqueuedAtMs, TtlMs: ttlMs, MaxRetries: new(uint32(0)),
	})
	s.Require().NoError(err)
	batch := db.NewBatch()
	defer batch.Close() //nolint:errcheck
	s.Require().NoError(batch.Set(target.Key, payload))
	s.Require().NoError(batch.Commit(storage.Sync))

	const ackWindow = 250 * time.Millisecond
	allowed, err := ledger.AuthorizeRetrySend(ctx, start, ackWindow,
		func(uint64) (bool, error) { return true, nil },
		func() (time.Time, error) { return time.Now().Add(ackWindow), nil })
	s.Require().NoError(err)
	s.Require().True(allowed)
	time.Sleep(time.Until(time.UnixMilli(enqueuedAtMs + ttlMs)))

	assertProtected := func() {
		s.Require().NoError(ledger.expireDueRetryAttempts(ctx, time.Now().UnixMilli(), nil))
		state, err := raft.ReadDeliveryState(db, target.Key)
		s.Require().NoError(err)
		s.True(state.HasActiveAttempt(target.Recipient, target.Token))
	}
	assertProtected()
	s.False(ledger.BeginRetryTransport(append(bytes.Clone(target.Key), target.Token...), func() bool { return true }))
	assertProtected()
	refund, _ := ledger.claimRetryRefund()
	s.Require().NotNil(refund)
	s.Require().NoError(ledger.applyRetryRefund(ctx, refund, 0))
	due, err := raft.DueDeliveryAttempts(db, time.Now().UnixMilli())
	s.Require().NoError(err)
	s.Empty(due)
	state, err := raft.ReadDeliveryState(db, target.Key)
	s.Require().NoError(err)
	s.True(state.Ready(target.Recipient))
}

func (s *RetryExpirySuite) TestDisconnectCancelsAuthorizationWaitAndRefunds() {
	for _, stage := range []deliverypb.RetryCommand_Operation{deliverypb.RetryCommand_START, deliverypb.RetryCommand_SENT} {
		s.Run(stage.String(), func() {
			db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
			s.Require().NoError(err)
			defer db.Close() //nolint:errcheck
			deadline := time.Now().Add(time.Minute)
			first, second := s.storeRetryFixture(db, 1, deadline), s.storeRetryFixture(db, 2, deadline)
			firstTarget, secondTarget := first.Attempts[0], second.Attempts[0]
			local := NewDeliveryLedger(db, nil, nil, nil)
			brokerCtx, stopBroker := context.WithCancel(context.Background())
			streamCtx, disconnect := context.WithCancel(brokerCtx)
			defer disconnect()
			entered := make(chan struct{})
			propose := func(ctx context.Context, raw []byte) (statemachine.Result, error) {
				if err := ctx.Err(); err != nil {
					return statemachine.Result{}, err
				}
				var cmd deliverypb.RetryCommand
				if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
					return statemachine.Result{}, err
				}
				value, err := local.ApplyRetry(&cmd)
				if err == nil && value != 0 && cmd.Operation == stage && bytes.Equal(cmd.Attempts[0].Key, firstTarget.Key) {
					// Raft may already have applied the command when its
					// caller cancels the wait for the result.
					close(entered)
					<-ctx.Done()
					return statemachine.Result{}, ctx.Err()
				}
				return statemachine.Result{Value: value}, err
			}
			ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
				return propose(brokerCtx, raw)
			})
			ledger.ProposeContext = propose
			ledger.ConfigureRetryOutcomes(1)
			defer ledger.RemoveRetryTransports([][]byte{firstTarget.Key, secondTarget.Key})
			prepare := func(uint64) (bool, error) { return true, nil }
			ready := func() (time.Time, error) { return deadline, nil }
			type authorization struct {
				allowed bool
				err     error
			}
			finished := make(chan struct{})
			result := make(chan authorization, 1)
			go func() {
				defer close(finished)
				allowed, err := ledger.AuthorizeRetrySend(streamCtx, first, time.Minute, prepare, ready)
				result <- authorization{allowed, err}
			}()
			defer func() { stopBroker(); <-finished }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				s.FailNow("the first proposal did not start")
			}
			allowed, err := ledger.AuthorizeRetrySend(brokerCtx, second, time.Minute, prepare, ready)
			s.Require().NoError(err)
			s.False(allowed, "the outstanding proposal occupies the only slot")
			disconnect()
			select {
			case outcome := <-result:
				s.NoError(outcome.err)
				s.False(outcome.allowed)
			case <-time.After(time.Second):
				s.FailNow("disconnect did not cancel the proposal wait")
			}
			state, err := raft.ReadDeliveryState(db, firstTarget.Key)
			s.Require().NoError(err)
			s.True(state.Ready(firstTarget.Recipient), "the refund must outlive the disconnected stream")
			allowed, err = ledger.AuthorizeRetrySend(brokerCtx, second, time.Minute, prepare, ready)
			s.Require().NoError(err)
			s.True(allowed, "the second delivery can use the freed authorization slot")
		})
	}
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
	allowed, err := ledger.AuthorizeRetrySend(context.Background(), first, 600*time.Millisecond, prepared, func() (time.Time, error) { return time.Time{}, nil })
	s.Require().NoError(err)
	s.False(allowed)
	allowed, err = ledger.AuthorizeRetrySend(context.Background(), second, 600*time.Millisecond, prepared, func() (time.Time, error) { return deadline, nil })
	s.Require().NoError(err)
	s.Require().True(allowed)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Go(func() { ledger.RunRetryExpiry(ctx, time.Millisecond, nil, logger) })
	workers.Go(func() { ledger.RunRetryRefunds(ctx, time.Millisecond, logger) })
	defer func() { close(unblock); cancel(); workers.Wait() }()
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

func (s *RetryExpirySuite) TestShutdownDrainsRefundFromInFlightAuthorization() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	local := NewDeliveryLedger(db, nil, nil, nil)
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	broker, stopBroker := context.WithCancel(shutdown)
	defer stopBroker()
	refundStarted := make(chan struct{})
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_CANCEL_RESERVATION {
			close(refundStarted)
			<-broker.Done()
			return statemachine.Result{}, errors.New("transient refund failure")
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	})
	ledger.ProposeContext = func(ctx context.Context, raw []byte) (statemachine.Result, error) {
		if err := ctx.Err(); err != nil {
			return statemachine.Result{}, err
		}
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	}
	ledger.ConfigureRetryOutcomes(1)
	start := s.storeRetryFixture(db, 1, time.Now().Add(time.Minute))
	var workers sync.WaitGroup
	workers.Go(func() { ledger.RunRetryRefunds(shutdown, time.Millisecond, logger) })
	defer func() { stopBroker(); cancel(); workers.Wait() }()
	authorization := make(chan error, 1)
	workers.Go(func() {
		_, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Second,
			func(uint64) (bool, error) { return true, nil },
			func() (time.Time, error) { return time.Time{}, nil })
		authorization <- err
	})
	select {
	case <-refundStarted:
	case <-shutdown.Done():
		s.FailNow("authorization did not attempt its refund")
	}
	ledger.StopRetryOutcomeAdmission()
	stopBroker()
	select {
	case err := <-authorization:
		s.Require().NoError(err)
	case <-shutdown.Done():
		s.FailNow("authorization did not hand off its failed refund")
	}
	drained := make(chan struct{})
	go func() { workers.Wait(); close(drained) }()
	select {
	case <-drained:
		s.Require().NoError(shutdown.Err())
	case <-shutdown.Done():
		s.FailNow("refunds did not drain before the shutdown deadline")
	}

	s.Require().NoError(local.expireDueRetryAttempts(context.Background(), time.Now().Add(time.Minute).UnixMilli(), nil))
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
	key := start.Attempts[0].Key
	state, err := raft.ReadDeliveryState(db, key)
	s.Require().NoError(err)
	s.True(state.Ready(start.Attempts[0].Recipient))
}

func (s *RetryExpirySuite) TestSlowRefundDoesNotBlockAnotherDuringShutdown() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	local := NewDeliveryLedger(db, nil, nil, nil)
	deadline := time.Now().Add(time.Minute)
	first, second := s.storeRetryFixture(db, 1, deadline), s.storeRetryFixture(db, 2, deadline)
	firstStarted := make(chan struct{})
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_CANCEL_RESERVATION {
			return statemachine.Result{}, errors.New("initial refund failed")
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	})
	ledger.ProposeContext = func(ctx context.Context, raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_CANCEL_RESERVATION && bytes.Equal(cmd.Attempts[0].Key, first.Attempts[0].Key) {
			close(firstStarted)
			<-ctx.Done()
			return statemachine.Result{}, ctx.Err()
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	}

	prepare := func(uint64) (bool, error) { return true, nil }
	allowed, err := ledger.AuthorizeRetrySend(context.Background(), first, time.Second, prepare, func() (time.Time, error) { return time.Time{}, nil })
	s.Require().NoError(err)
	s.False(allowed)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ledger.RunRetryRefunds(ctx, time.Millisecond, logger)
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		s.FailNow("the first refund never started")
	}
	allowed, err = ledger.AuthorizeRetrySend(context.Background(), second, time.Second, prepare, func() (time.Time, error) {
		ledger.StopRetryOutcomeAdmission()
		return time.Time{}, nil
	})
	s.Require().NoError(err)
	s.False(allowed)
	s.Require().Eventually(func() bool {
		state, err := raft.ReadDeliveryState(db, second.Attempts[0].Key)
		return err == nil && state.Ready(second.Attempts[0].Recipient)
	}, time.Second, time.Millisecond)
	cancel()
	<-done

	s.Require().NoError(local.expireDueRetryAttempts(context.Background(), deadline.UnixMilli(), nil))
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Require().Len(page.Page.Entries, 1)
	s.True(bytes.HasPrefix(page.Page.Entries[0].Id, first.Attempts[0].Key))
}

func (s *RetryExpirySuite) TestDeletionClearsTransportTimersAndDeadlines() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, NewDirectDeleteBackend(db, logger), nil)
	deadline := time.Now().Add(time.Hour)
	prepare := func(uint64) (bool, error) { return true, nil }
	var keys [][]byte
	defer func() { ledger.RemoveRetryTransports(keys) }()
	var timers []*time.Timer
	for i := range 65 {
		start := s.storeRetryFixture(db, uint64(i+1), deadline)
		target := start.Attempts[0]
		keys = append(keys, target.Key)
		ok, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Hour, prepare, func() (time.Time, error) { return deadline, nil })
		s.Require().NoError(err)
		s.Require().True(ok)
		timer := ledger.retry.transports[[utils.EventKeySize]byte(target.Key)][identifyRetry(target)].timer
		s.Require().NotNil(timer)
		timers = append(timers, timer)
	}
	// TTL and explicit deletions use the same deleter path.
	deleter := NewDeleter(ledger, time.Hour, logger)
	for _, key := range keys[:64] {
		deleter.MarkDeleted(key)
	}
	deleter.flush()
	s.Len(ledger.retry.transports, 1)
	for _, timer := range timers[:64] {
		s.False(timer.Stop(), "deletion must have stopped the timer")
	}
	due, err := raft.DueDeliveryAttempts(db, deadline.Add(time.Second).UnixMilli())
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	s.Equal(keys[64], due[0].Key)
}

func (s *RetryExpirySuite) TestDeletionDuringAuthorizationCannotInstallTransportTimer() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, NewDirectDeleteBackend(db, logger), nil)
	start := s.storeRetryFixture(db, 1, time.Now().Add(time.Hour))
	allowed, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Hour, func(uint64) (bool, error) { return true, nil }, func() (time.Time, error) {
		err := ledger.DeleteKeys([][]byte{start.Attempts[0].Key})
		return time.Now().Add(time.Hour), err
	})
	s.Require().NoError(err)
	s.False(allowed)
	s.Empty(ledger.retry.transports)
}

func (s *RetryExpirySuite) TestForeignExpiryReleasesManualSlotForRedelivery() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	repo, err := repository.NewEventRepository(db, logger, time.Second)
	s.Require().NoError(err)
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	ledger.SetRetryOwner(1)
	sm := raft.NewEventStateMachineFactoryWithReceiptCache(db, repo, nil, ledger.CompleteRetryAttempts, nil, logger, 0, 0)(1, 1)
	var index uint64
	ledger.propose = func(cmd []byte) (statemachine.Result, error) {
		index++
		results, err := sm.Update([]statemachine.Entry{{Index: index, Cmd: cmd}})
		if err != nil {
			return statemachine.Result{}, err
		}
		return results[0].Result, nil
	}
	const recipient = "g:workers"
	topicHash := utils.TopicHash("orders")
	key := utils.EventKey(1, topicHash, 1)
	defer ledger.RemoveRetryTransports([][]byte{key})
	receipt, err := raft.MarshalPublishReceipt(0, []string{recipient})
	s.Require().NoError(err)
	payload, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", MaxRetries: new(uint32(1))})
	s.Require().NoError(err)
	store, err := raft.MarshalStoreDeliveryBatchCmd([]raft.StoreBatchItem{
		{ID: 1, Bucket: 1, TopicHash: topicHash, Msg: payload, Receipt: receipt},
	})
	s.Require().NoError(err)
	_, err = ledger.propose(store)
	s.Require().NoError(err)
	renew := func(now time.Time) {
		until := now.Add(30 * time.Second)
		result, err := ledger.ApplyRetry(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_RENEW_OWNER, Owner: ledger.retry.owner,
			NowMs: now.UnixMilli(), LeaseUntilMs: until.UnixMilli(),
		})
		s.Require().NoError(err)
		s.Require().Equal(uint64(until.UnixMilli()), result)
		ledger.retry.until.Store(&until)
	}
	authorize := func(now time.Time, token byte, want uint64) *deliverypb.DeliveryAttemptTarget {
		deadline := now.Add(time.Minute)
		target := &deliverypb.DeliveryAttemptTarget{
			Key: key, Recipient: recipient, Token: bytes.Repeat([]byte{token}, raft.AttemptTokenSize),
			DeadlineMs: deadline.UnixMilli(),
		}
		allowed, err := ledger.AuthorizeManualRetrySend(context.Background(), &deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_START, NowMs: now.UnixMilli(), Attempts: []*deliverypb.DeliveryAttemptTarget{target},
		}, time.Minute, func(attempt uint64) (bool, error) {
			s.Equal(want, attempt)
			return true, nil
		}, func() (time.Time, error) { return deadline, nil })
		s.Require().NoError(err)
		s.Require().True(allowed)
		return target
	}
	now := time.Now()
	renew(now)
	first := authorize(now, 1, 1)
	timer := ledger.retry.transports[[utils.EventKeySize]byte(key)][identifyRetry(first)].timer
	s.Require().NotNil(timer)
	expireAt := now.Add(2 * time.Minute)
	due, err := raft.DueDeliveryAttempts(db, expireAt.UnixMilli())
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	expire, err := raft.MarshalRetryCommand(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_EXPIRE, Owner: bytes.Repeat([]byte{2}, raft.RetryOwnerSize),
		NowMs: expireAt.UnixMilli(), Attempts: due,
	})
	s.Require().NoError(err)
	_, err = ledger.propose(expire)
	s.Require().NoError(err)
	s.Empty(ledger.retry.manualSlots)
	s.False(timer.Stop(), "the replicated expiry should have stopped the original owner's timer")
	state, err := raft.ReadDeliveryState(db, key)
	s.Require().NoError(err)
	s.True(state.Ready(recipient))

	renew(expireAt)
	second := authorize(expireAt, 2, 2)
	_, err = ledger.propose(expire)
	s.Require().NoError(err)
	s.Contains(ledger.retry.manualSlots, identifyRetry(second), "a stale expiry must preserve the new token's slot")
}

func (s *RetryExpirySuite) TestSnapshotRecoveryReleasesOnlyMissingManualSlots() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	repo, err := repository.NewEventRepository(db, logger, time.Second)
	s.Require().NoError(err)
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(2)
	sm := raft.NewEventStateMachineFactoryWithReceiptCache(db, repo, nil, nil, ledger.ReconcileRetryAttempts, logger, 0, 0)(1, 1)
	deadline := time.Now().Add(time.Hour)
	retained := s.storeRetryFixture(db, 1, deadline)
	stale := s.storeRetryFixture(db, 2, deadline)
	newDelivery := s.storeRetryFixture(db, 3, deadline)
	defer ledger.RemoveRetryTransports([][]byte{retained.Attempts[0].Key, stale.Attempts[0].Key, newDelivery.Attempts[0].Key})
	authorize := func(start *deliverypb.RetryCommand) bool {
		allowed, err := ledger.AuthorizeManualRetrySend(context.Background(), start, time.Hour,
			func(uint64) (bool, error) { return true, nil },
			func() (time.Time, error) { return deadline, nil })
		s.Require().NoError(err)
		return allowed
	}
	s.Require().True(authorize(retained))
	snapshot, err := sm.PrepareSnapshot()
	s.Require().NoError(err)
	var encoded bytes.Buffer
	s.Require().NoError(sm.SaveSnapshot(snapshot, &encoded, nil))

	s.Require().True(authorize(stale))
	staleTarget := stale.Attempts[0]
	staleID := identifyRetry(staleTarget)
	staleTimer := ledger.retry.transports[[utils.EventKeySize]byte(staleTarget.Key)][staleID].timer
	s.Require().NotNil(staleTimer)
	s.False(authorize(newDelivery), "both slots are reserved before recovery")
	s.Require().NoError(sm.RecoverFromSnapshot(&encoded, nil))
	s.Len(ledger.retry.manualSlots, 1)
	s.Contains(ledger.retry.manualSlots, identifyRetry(retained.Attempts[0]))
	s.NotContains(ledger.retry.manualSlots, staleID)
	s.False(staleTimer.Stop(), "recovery must have stopped the stale timer")
	s.True(authorize(newDelivery), "the restored broker can use the freed slot")
}

func (s *RetryExpirySuite) TestShutdownReleasesManualSlotWhenACKWindowEnds() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	start := s.storeRetryFixture(db, 1, time.Now().Add(time.Minute))
	target := start.Attempts[0]
	defer ledger.RemoveRetryTransports([][]byte{target.Key})
	const ackWindow = 100 * time.Millisecond
	allowed, err := ledger.AuthorizeManualRetrySend(context.Background(), start, ackWindow,
		func(uint64) (bool, error) { return true, nil },
		func() (time.Time, error) { return time.Now().Add(ackWindow), nil })
	s.Require().NoError(err)
	s.Require().True(allowed)
	ledger.StopRetryOutcomeAdmission()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ledger.RunRetryOutcomes(ctx, 1, logger)
	s.Require().NoError(ctx.Err(), "the drain should finish once the ACK window ends")
	s.Empty(ledger.retry.manualSlots)
}

func (s *RetryExpirySuite) TestRetiringRecipientStopsOnlyItsTransportTimer() {
	for _, mode := range []string{"standalone", "raft"} {
		s.Run(mode, func() {
			logger := zap.NewNop()
			db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
			s.Require().NoError(err)
			defer db.Close() //nolint:errcheck
			ctx := context.Background()
			service := subscriptions.New(db, 0, logger)
			ledger := NewDeliveryLedger(db, nil, nil, nil)
			applySubscription := ledger.ApplySubscription
			if mode == "raft" {
				repo, err := repository.NewEventRepository(db, logger, time.Second)
				s.Require().NoError(err)
				sm := raft.NewEventStateMachineFactoryWithReceiptCache(db, repo, nil, ledger.CompleteRetryAttempts, nil, logger, 0, 0)(1, 1)
				var index uint64
				ledger.propose = func(cmd []byte) (statemachine.Result, error) {
					index++
					results, err := sm.Update([]statemachine.Entry{{Index: index, Cmd: cmd}})
					if err != nil {
						return statemachine.Result{}, err
					}
					return results[0].Result, nil
				}
				applySubscription = ledger.propose
				service.Propose = func(_ context.Context, cmd []byte) (statemachine.Result, error) { return applySubscription(cmd) }
			} else {
				service.ApplyLocal = applySubscription
			}
			s.Require().NoError(service.CreateTopic(ctx, "orders", nil))
			retired, err := service.Create(ctx, "orders", "retired", "")
			s.Require().NoError(err)
			remaining, err := service.Create(ctx, "orders", "remaining", "")
			s.Require().NoError(err)
			now := time.Now()
			deadline := now.Add(time.Hour)
			start := s.storeRetryFixture(db, 1, deadline)
			key := start.Attempts[0].Key
			defer ledger.RemoveRetryTransports([][]byte{key})
			receipt, err := raft.MarshalPublishReceipt(0, []string{retired.Recipient, remaining.Recipient})
			s.Require().NoError(err)
			batch := db.NewBatch()
			defer batch.Close() //nolint:errcheck
			s.Require().NoError(batch.Set(raft.DeliveryStateKey(key), receipt))
			s.Require().NoError(batch.Commit(storage.Sync))
			var timers []*time.Timer
			transports := [utils.EventKeySize]byte(key)
			for i, recipient := range []string{retired.Recipient, remaining.Recipient} {
				target := start.Attempts[0]
				target.Recipient = recipient
				target.Token = bytes.Repeat([]byte{byte(i + 1)}, raft.AttemptTokenSize)
				ok, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Hour,
					func(uint64) (bool, error) { return true, nil },
					func() (time.Time, error) { return deadline, nil })
				s.Require().NoError(err)
				s.Require().True(ok)
				timers = append(timers, ledger.retry.transports[transports][identifyRetry(target)].timer)
			}
			s.Require().NoError(service.Unsubscribe(ctx, "orders", "retired", ""))
			cleanup, err := raft.MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{
				Operation: raft.SubscriptionCleanup, NowMs: now.UnixMilli(), Keys: [][]byte{key},
			})
			s.Require().NoError(err)
			result, err := applySubscription(cleanup)
			s.Require().NoError(err)
			s.Zero(result.Value, "the remaining recipient still needs the payload")
			s.Len(ledger.retry.transports[transports], 1)
			s.False(timers[0].Stop(), "the retired recipient's timer must already be stopped")
			s.Equal(timers[1], ledger.retry.transports[transports][identifyRetry(start.Attempts[0])].timer)
			s.True(timers[1].Stop(), "the remaining recipient's timer must still be active")
			due, err := raft.DueDeliveryAttempts(db, deadline.Add(time.Second).UnixMilli())
			s.Require().NoError(err)
			s.Require().Len(due, 1)
			s.Equal(remaining.Recipient, due[0].Recipient)
		})
	}
}

func (s *RetryExpirySuite) TestRetiringRecipientReleasesPendingRefundCapacity() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	local := NewDeliveryLedger(db, nil, nil, nil)
	ledger := NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if cmd.Operation == deliverypb.RetryCommand_CANCEL_RESERVATION {
			return statemachine.Result{}, errors.New("refund proposal unavailable")
		}
		value, err := local.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	})
	ledger.ConfigureRetryOutcomes(1)
	service := subscriptions.New(db, 0, logger)
	service.ApplyLocal = ledger.ApplySubscription
	s.Require().NoError(service.CreateTopic(ctx, "orders", nil))
	retired, err := service.Create(ctx, "orders", "retired", "")
	s.Require().NoError(err)
	remaining, err := service.Create(ctx, "orders", "remaining", "")
	s.Require().NoError(err)
	deadline := time.Now().Add(time.Minute)
	start := s.storeRetryFixture(db, 1, deadline)
	target := start.Attempts[0]
	target.Recipient = retired.Recipient
	defer ledger.RemoveRetryTransports([][]byte{target.Key})
	receipt, err := raft.MarshalPublishReceipt(0, []string{retired.Recipient, remaining.Recipient})
	s.Require().NoError(err)
	batch := db.NewBatch()
	defer batch.Close() //nolint:errcheck
	s.Require().NoError(batch.Set(raft.DeliveryStateKey(target.Key), receipt))
	s.Require().NoError(batch.Commit(storage.Sync))

	prepare := func(uint64) (bool, error) { return true, nil }
	allowed, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Minute, prepare, func() (time.Time, error) { return time.Time{}, nil })
	s.Require().NoError(err)
	s.False(allowed)
	refund, _ := ledger.claimRetryRefund()
	s.Require().NotNil(refund, "the failed cancellation must occupy the only refund slot")

	cleanup, err := raft.MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{
		Operation: raft.SubscriptionCleanup, NowMs: time.Now().UnixMilli(), Keys: [][]byte{target.Key},
	})
	s.Require().NoError(err)
	s.Require().NoError(service.Unsubscribe(ctx, "orders", "retired", ""))
	_, err = ledger.ApplySubscription(cleanup)
	s.Require().NoError(err)
	s.Require().NoError(ledger.applyRetryRefund(ctx, refund, time.Second), "a late refund failure cannot restore removed work")
	s.Empty(ledger.retry.refunds)

	target.Recipient = remaining.Recipient
	target.Token = bytes.Repeat([]byte{2}, raft.AttemptTokenSize)
	allowed, err = ledger.AuthorizeRetrySend(context.Background(), start, time.Minute, func(attempt uint64) (bool, error) {
		s.Equal(uint64(1), attempt)
		return true, nil
	}, func() (time.Time, error) { return deadline, nil })
	s.Require().NoError(err)
	s.True(allowed, "the remaining recipient must be able to use the released capacity")
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
	allowed, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Second, func(attempt uint64) (bool, error) {
		s.Equal(uint64(1), attempt)
		return true, nil
	}, func() (time.Time, error) { return time.Time{}, nil })
	s.False(allowed)
	s.NoError(err)

	ctx := context.Background()
	refund, _ := ledger.claimRetryRefund()
	s.Require().NotNil(refund)
	s.Require().ErrorContains(ledger.applyRetryRefund(ctx, refund, 0), "transient cancellation failure")
	s.Require().NoError(ledger.expireDueRetryAttempts(ctx, deadline, nil))
	page, err := (raft.DeadLetterQuery{Topic: topic, Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
	refund, _ = ledger.claimRetryRefund()
	s.Require().NotNil(refund)
	s.Require().NoError(ledger.applyRetryRefund(ctx, refund, 0))
	s.Len(ledger.retryOutcomes, 1, "refund does not require a slot in the outcome queue")
	target.DeadlineMs, target.Token = deadline+10, bytes.Repeat([]byte{2}, raft.AttemptTokenSize)
	start.NowMs = deadline + 1
	attempt, err := ledger.ApplyRetry(start)
	s.Require().NoError(err)
	s.Equal(uint64(1), attempt, "the unsent delivery must retain its first attempt")
}

func (s *RetryExpirySuite) TestAutoACKReservationReleasesCapacityOnSendFailure() {
	ledger := NewDeliveryLedger(nil, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	outcome := func(id uint64) *deliverypb.RetryCommand {
		return &deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_ACK, NowMs: time.Now().UnixMilli(),
			Attempts: []*deliverypb.DeliveryAttemptTarget{{
				Key: utils.EventKey(1, utils.TopicHash("orders"), id), Recipient: "g:workers",
				Token: bytes.Repeat([]byte{byte(id)}, raft.AttemptTokenSize),
			}},
		}
	}
	reservation := ledger.TryReserveRetryAutoACK(outcome(1), func() {})
	s.Require().NotNil(reservation)
	s.True(ledger.TryRetryOutcome(outcome(2), func() {}), "manual ACKs use their own capacity")
	s.Nil(ledger.TryReserveRetryAutoACK(outcome(2), func() {}))
	s.False(reservation.Finish(false))
	s.Empty(ledger.retryAutoACKOutcomes, "failed sends cannot enqueue an ACK")
	s.NotNil(ledger.TryReserveRetryAutoACK(outcome(3), func() {}), "automatic capacity is available with a full manual queue")
}

func (s *RetryExpirySuite) TestManualACKCommitsWhileAutoACKHoldsItsOnlySlot() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	deadline := time.Now().Add(time.Minute)
	manual := s.storeRetryFixture(db, 1, deadline)
	automatic := s.storeRetryFixture(db, 2, deadline)
	authorize := func(start *deliverypb.RetryCommand) {
		allowed, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Minute,
			func(uint64) (bool, error) { return true, nil },
			func() (time.Time, error) { return deadline, nil })
		s.Require().NoError(err)
		s.Require().True(allowed)
	}
	ack := func(start *deliverypb.RetryCommand) *deliverypb.RetryCommand {
		target := start.Attempts[0]
		return &deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_ACK, NowMs: time.Now().UnixMilli(),
			Attempts: []*deliverypb.DeliveryAttemptTarget{{
				Key: target.Key, Recipient: target.Recipient, Token: target.Token,
			}},
		}
	}
	defer ledger.RemoveRetryTransports([][]byte{manual.Attempts[0].Key, automatic.Attempts[0].Key})
	authorize(manual)
	autoACK := ledger.TryReserveRetryAutoACK(ack(automatic), func() {})
	s.Require().NotNil(autoACK)
	authorize(automatic)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ledger.RunRetryOutcomes(ctx, 2, logger)
	}()
	defer func() { cancel(); <-done }()
	committed := make(chan struct{})
	s.Require().True(ledger.TryRetryOutcome(ack(manual), func() { close(committed) }))
	select {
	case <-committed:
	case <-ctx.Done():
		s.FailNow("the manual ACK did not commit while the auto-ACK send was blocked")
	}

	ledger.StopRetryOutcomeAdmission()
	s.Require().True(autoACK.Finish(true))
	select {
	case <-done:
		s.Require().NoError(ctx.Err())
	case <-ctx.Done():
		s.FailNow("the automatic outcome did not drain during shutdown")
	}
	recovery := NewDeliveryLedger(db, nil, nil, nil)
	s.Require().NoError(recovery.expireDueRetryAttempts(ctx, deadline.Add(time.Second).UnixMilli(), nil))
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
	for _, start := range []*deliverypb.RetryCommand{manual, automatic} {
		state, err := raft.ReadDeliveryState(db, start.Attempts[0].Key)
		s.Require().NoError(err)
		s.Nil(state)
	}
}

func (s *RetryExpirySuite) TestManualACKsCommitWhileBothAutomaticWorkersAreStalled() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	deadline := time.Now().Add(time.Minute)
	autoA := s.storeRetryFixture(db, 1, deadline)
	autoB := s.storeRetryFixture(db, 2, deadline)
	manualA := s.storeRetryFixture(db, 3, deadline)
	manualB := s.storeRetryFixture(db, 4, deadline)
	keys := [][]byte{autoA.Attempts[0].Key, autoB.Attempts[0].Key, manualA.Attempts[0].Key, manualB.Attempts[0].Key}
	defer ledger.RemoveRetryTransports(keys)
	autoStarted := make(chan struct{}, 2)
	unblock := make(chan struct{})
	ledger.ProposeContext = func(ctx context.Context, raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if key := cmd.Attempts[0].Key; cmd.Operation == deliverypb.RetryCommand_ACK && (bytes.Equal(key, keys[0]) || bytes.Equal(key, keys[1])) {
			autoStarted <- struct{}{}
			select {
			case <-unblock:
			case <-ctx.Done():
				return statemachine.Result{}, ctx.Err()
			}
		}
		value, err := ledger.ApplyRetry(&cmd)
		return statemachine.Result{Value: value}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ledger.RunRetryOutcomes(ctx, 2, logger)
	}()
	defer func() { cancel(); <-done }()
	wait := func(ch <-chan struct{}, message string) {
		select {
		case <-ch:
		case <-ctx.Done():
			s.FailNow(message)
		}
	}
	ack := func(start *deliverypb.RetryCommand) *deliverypb.RetryCommand {
		target := start.Attempts[0]
		return &deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_ACK, NowMs: time.Now().UnixMilli(),
			Attempts: []*deliverypb.DeliveryAttemptTarget{{
				Key: target.Key, Recipient: target.Recipient, Token: target.Token,
			}},
		}
	}
	prepare := func(uint64) (bool, error) { return true, nil }
	ready := func() (time.Time, error) { return deadline, nil }
	for _, start := range []*deliverypb.RetryCommand{autoA, autoB} {
		automatic := ledger.TryReserveRetryAutoACK(ack(start), func() {})
		s.Require().NotNil(automatic)
		allowed, err := ledger.AuthorizeRetrySend(context.Background(), start, time.Minute, prepare, ready)
		s.Require().NoError(err)
		s.Require().True(allowed)
		s.Require().True(automatic.Finish(true))
		wait(autoStarted, "an automatic worker did not start its proposal")
	}

	commitManual := func(start *deliverypb.RetryCommand) {
		committed := make(chan struct{})
		s.Require().True(ledger.TryRetryOutcome(ack(start), func() { close(committed) }))
		wait(committed, "manual persistence waited for the stalled automatic workers")
	}
	allowed, err := ledger.AuthorizeManualRetrySend(ctx, manualA, time.Minute, prepare, ready)
	s.Require().NoError(err)
	s.Require().True(allowed)
	allowed, err = ledger.AuthorizeManualRetrySend(ctx, manualB, time.Minute, prepare, ready)
	s.Require().NoError(err)
	s.False(allowed, "a delivery must wait when its outcome capacity is reserved")
	state, err := raft.ReadDeliveryState(db, keys[3])
	s.Require().NoError(err)
	s.True(state.Ready(manualB.Attempts[0].Recipient))
	commitManual(manualA)
	allowed, err = ledger.AuthorizeManualRetrySend(ctx, manualB, time.Minute, prepare, ready)
	s.Require().NoError(err)
	s.Require().True(allowed)
	commitManual(manualB)

	ledger.StopRetryOutcomeAdmission()
	close(unblock)
	wait(done, "outcomes did not drain")
	recovery := NewDeliveryLedger(db, nil, nil, nil)
	s.Require().NoError(recovery.expireDueRetryAttempts(ctx, deadline.Add(time.Second).UnixMilli(), nil))
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
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

func (s *RetryExpirySuite) TestExpiryCannotClaimAttemptBetweenACKValidationAndAdmission() {
	logger := zap.NewNop()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, logger)
	s.Require().NoError(err)
	defer db.Close() //nolint:errcheck
	ledger := NewDeliveryLedger(db, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	deadline := time.Now().Add(time.Second)
	start := s.storeRetryFixture(db, 1, deadline)
	value, err := ledger.ApplyRetry(start)
	s.Require().NoError(err)
	s.Require().Equal(uint64(1), value)
	value, err = ledger.ApplyRetry(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_SENT, NowMs: start.NowMs, Attempts: start.Attempts,
	})
	s.Require().NoError(err)
	s.Require().Equal(uint64(1), value)
	target := start.Attempts[0]
	command := &deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_ACK, NowMs: deadline.UnixMilli() - 1,
		Attempts: []*deliverypb.DeliveryAttemptTarget{{Key: target.Key, Recipient: target.Recipient, Token: target.Token}},
	}
	validated := make(chan struct{})
	resume := make(chan struct{})
	admitted := make(chan bool, 1)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		valid, accepted := ledger.TryManualRetryOutcome(command, func() bool {
			close(validated)
			<-resume
			return true
		}, func() {})
		admitted <- valid && accepted
	})
	defer tasks.Wait()
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	select {
	case <-validated:
	case <-time.After(time.Second):
		s.FailNow("ACK validation did not start")
	}
	expiryStarted := make(chan struct{})
	expired := make(chan error, 1)
	tasks.Go(func() {
		close(expiryStarted)
		expired <- ledger.expireDueRetryAttempts(context.Background(), deadline.UnixMilli(), nil)
	})
	<-expiryStarted
	select {
	case err := <-expired:
		s.Require().NoError(err)
		s.FailNow("expiry ran before the validated ACK could queue")
	case <-time.After(25 * time.Millisecond):
	}
	close(resume)
	resumed = true
	s.True(<-admitted)
	s.Require().NoError(<-expired)
	// A queued ACK must also retain protection until its worker commits.
	s.Require().NoError(ledger.expireDueRetryAttempts(context.Background(), deadline.UnixMilli(), nil))
	ledger.StopRetryOutcomeAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ledger.RunRetryOutcomes(ctx, 1, logger)
	s.Require().NoError(ctx.Err())
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
	state, err := raft.ReadDeliveryState(db, target.Key)
	s.Require().NoError(err)
	s.Nil(state, "the ACK must complete the delivery")
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
