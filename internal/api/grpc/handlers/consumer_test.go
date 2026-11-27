package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/dispatcher"
	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type failedChunkReceiptRead struct {
	storage.DB
	key  []byte
	fail atomic.Bool
}

func (db *failedChunkReceiptRead) Get(key []byte) ([]byte, io.Closer, error) {
	if bytes.Equal(key, db.key) && db.fail.Swap(false) {
		return nil, nil, errors.New("injected receipt read failure")
	}
	return db.DB.Get(key)
}

type LimitedConsumerSuite struct{ suite.Suite }

func TestLimitedConsumerSuite(t *testing.T) {
	suite.Run(t, new(LimitedConsumerSuite))
}

func (s *LimitedConsumerSuite) TestFailedFirstChunkCheckRefundsBeforeSend() {
	a, hub, producer, _ := openPublishTest(s.T(), "pebble")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	s.Require().NoError(err)
	payload := bytes.Repeat([]byte("chunk"), 500)
	ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
		Topic: "orders", Payload: payload, MaxRetries: new(uint32(0)),
	}}})
	s.Require().NoError(err)
	s.Require().True(ack.GetSuccess())
	keys := publishedKeys(s.T(), a.DB)
	s.Require().Len(keys, 1)
	key := keys[0]
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	queue := make(chan *pb.QueueMessage, 1)
	hub.Register("consumer", "orders", group, queue)
	defer hub.Unregister("consumer")
	s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders", Payload: payload, MaxRetries: new(uint32(0))}, key))

	local := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	db := &failedChunkReceiptRead{DB: a.DB, key: raft.DeliveryStateKey(key)}
	handler := NewConsumerHandler(zap.NewNop(), hub, nil)
	handler.MaxSendBytes = 1024
	handler.RetryLedger = dispatcher.NewDeliveryLedger(db, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		value, err := local.ApplyRetry(&cmd)
		if err == nil && cmd.Operation == deliverypb.RetryCommand_SENT && value != 0 {
			db.fail.Store(true)
		}
		return statemachine.Result{Value: value}, err
	})
	handler.RetryLedger.ConfigureRetryOutcomes(1)
	stream := &blockedRetrySend{ctx: ctx, sending: make(chan *pb.QueueMessage, 1), unblock: make(chan struct{})}
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.sender(ctx, stream, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: group}, queue, errCh)
	}()
	defer func() { cancel(); <-done }()
	select {
	case err := <-errCh:
		s.Require().ErrorContains(err, "injected receipt read failure")
	case <-stream.sending:
		s.FailNow("the first chunk reached the transport")
	case <-ctx.Done():
		s.FailNow("sender did not report the receipt error")
	}
	due, err := raft.DueDeliveryAttempts(a.DB, time.Now().Add(time.Minute).UnixMilli())
	s.Require().NoError(err)
	s.Empty(due)
	state, err := raft.ReadDeliveryState(a.DB, key)
	s.Require().NoError(err)
	s.True(state.Ready(subscription.Recipient), "registration remains eligible for its first attempt")
}

func (s *LimitedConsumerSuite) TestRebalanceBetweenReservationAndSendDoesNotCharge() {
	s.Run("during-reservation", func() { s.exerciseCanceledSend(deliverypb.RetryCommand_START, false) })
	s.Run("during-authorization", func() { s.exerciseCanceledSend(deliverypb.RetryCommand_SENT, false) })
}

func (s *LimitedConsumerSuite) TestAuthorizationThatReturnsPastDeadlineIsRefunded() {
	s.exerciseCanceledSend(deliverypb.RetryCommand_SENT, true)
}

func (s *LimitedConsumerSuite) TestAuthorizationResumingAfterTTLDoesNotReturnDelivery() {
	for _, stage := range []deliverypb.RetryCommand_Operation{deliverypb.RetryCommand_START, deliverypb.RetryCommand_SENT} {
		s.Run(stage.String(), func() {
			a, hub, producer, _ := openPublishTest(s.T(), "pebble")
			ctx := context.Background()
			subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
			s.Require().NoError(err)
			ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
				Topic: "orders", Payload: []byte("original"), TtlMs: 500, MaxRetries: new(uint32(0)),
			}}})
			s.Require().NoError(err)
			s.Require().True(ack.GetSuccess())
			keys := publishedKeys(s.T(), a.DB)
			s.Require().Len(keys, 1)
			key := keys[0]
			value, closer, err := a.DB.Get(key)
			s.Require().NoError(err)
			var stored storagepb.StoredMessage
			err = proto.Unmarshal(value, &stored)
			s.Require().NoError(closer.Close())
			s.Require().NoError(err)
			expires := time.UnixMilli(stored.EnqueuedAtUnixMs + stored.TtlMs)
			group := strings.TrimPrefix(subscription.Recipient, "g:")
			queue := make(chan *pb.QueueMessage, 1)
			hub.Register("consumer", "orders", group, queue)
			defer hub.Unregister("consumer")
			s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{
				Topic: "orders", Payload: stored.Payload, EnqueuedAtUnixMs: stored.EnqueuedAtUnixMs, MaxRetries: stored.MaxRetries,
			}, key))

			logger := zap.NewNop()
			sm := raft.NewEventStateMachineFactory(a.DB, a.Repositories.Events, nil, logger)(1, 1)
			var index uint64
			delayed := false
			ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, func(raw []byte) (statemachine.Result, error) {
				var cmd deliverypb.RetryCommand
				if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
					return statemachine.Result{}, err
				}
				index++
				results, err := sm.Update([]statemachine.Entry{{Index: index, Cmd: raw}})
				if err != nil {
					return statemachine.Result{}, err
				}
				result := results[0].Result
				if cmd.Operation == stage && result.Value != 0 {
					delayed = true
					time.Sleep(time.Until(expires))
				}
				return result, nil
			})
			ledger.ConfigureRetryOutcomes(1)
			defer ledger.RemoveRetryTransports([][]byte{key})
			handler := NewConsumerHandler(logger, hub, nil)
			handler.RetryLedger = ledger
			frames, err := handler.beginLimitedSend(ctx, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: group}, <-queue)
			s.Require().NoError(err)
			s.Require().True(delayed, "the proposal must commit before TTL expiry")
			s.Nil(frames, "an expired payload must never reach the gRPC sender")
			due, err := raft.DueDeliveryAttempts(a.DB, time.Now().Add(time.Minute).UnixMilli())
			s.Require().NoError(err)
			s.Empty(due, "skipping transport must refund the attempt")
		})
	}
}

func (s *LimitedConsumerSuite) TestTTLExpiresBetweenAuthorizationAndFirstFrame() {
	for _, tc := range []struct {
		name         string
		maxSendBytes int
		autoAck      bool
	}{
		{name: "unchunked"},
		{name: "chunked", maxSendBytes: 256, autoAck: true},
	} {
		s.Run(tc.name, func() {
			a, hub, producer, _ := openPublishTest(s.T(), "pebble")
			ctx := context.Background()
			subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
			s.Require().NoError(err)
			ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
				Topic: "orders", Payload: bytes.Repeat([]byte("message"), 100), TtlMs: 500, MaxRetries: new(uint32(0)),
			}}})
			s.Require().NoError(err)
			s.Require().True(ack.GetSuccess())
			keys := publishedKeys(s.T(), a.DB)
			s.Require().Len(keys, 1)
			key := keys[0]
			data, closer, err := a.DB.Get(key)
			s.Require().NoError(err)
			var stored storagepb.StoredMessage
			decodeErr := proto.Unmarshal(data, &stored)
			s.Require().NoError(closer.Close())
			s.Require().NoError(decodeErr)
			expires := time.UnixMilli(stored.EnqueuedAtUnixMs + stored.TtlMs)

			group := strings.TrimPrefix(subscription.Recipient, "g:")
			queue := make(chan *pb.QueueMessage, 1)
			hub.Register("consumer", "orders", group, queue)
			defer hub.Unregister("consumer")
			s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{
				Topic: "orders", Payload: stored.Payload, EnqueuedAtUnixMs: stored.EnqueuedAtUnixMs, MaxRetries: stored.MaxRetries,
			}, key))
			logger := zap.NewNop()
			ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
			ledger.ConfigureRetryOutcomes(1)
			defer ledger.RemoveRetryTransports([][]byte{key})
			handler := NewConsumerHandler(logger, hub, nil)
			handler.RetryLedger = ledger
			handler.MaxSendBytes = tc.maxSendBytes
			message := <-queue
			init := &pb.SubscribeInit{Topic: "orders", GroupId: group, AutoAck: tc.autoAck}
			frames, err := handler.beginLimitedSend(ctx, "consumer", init, message)
			s.Require().NoError(err)
			s.Require().NotNil(frames)
			s.Require().Equal(tc.maxSendBytes > 0, len(frames.encoded) > 0)

			// Stall after authorization, at the same boundary as send metrics.
			time.Sleep(time.Until(expires))
			sendCalls := 0
			stream := &autoAckSendStream{send: func(*pb.QueueMessage) error {
				sendCalls++
				return nil
			}}
			err = handler.sendDelivery(ctx, stream, "consumer", subscription.Recipient, message, frames)
			s.ErrorIs(err, errRetryTransportSkipped)
			s.Zero(sendCalls)
			due, err := raft.DueDeliveryAttempts(a.DB, time.Now().Add(time.Minute).UnixMilli())
			s.Require().NoError(err)
			s.Empty(due, "the first frame was skipped and its attempt refunded")
			state, err := raft.ReadDeliveryState(a.DB, key)
			s.Require().NoError(err)
			s.True(state.Ready(subscription.Recipient))
			page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(a.DB)
			s.Require().NoError(err)
			s.Empty(page.Page.Entries)
		})
	}
}

func (s *LimitedConsumerSuite) TestAssignmentChangesBetweenAuthorizationAndFirstFrame() {
	for _, maxSendBytes := range []int{0, 256} {
		s.Run(fmt.Sprintf("frame-limit-%d", maxSendBytes), func() {
			a, hub, producer, _ := openPublishTest(s.T(), "pebble")
			ctx := context.Background()
			subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
			s.Require().NoError(err)
			payload := bytes.Repeat([]byte("message"), 100)
			ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
				Topic: "orders", Payload: payload, MaxRetries: new(uint32(0)),
			}}})
			s.Require().NoError(err)
			s.Require().True(ack.GetSuccess())
			keys := publishedKeys(s.T(), a.DB)
			s.Require().Len(keys, 1)
			key := keys[0]

			group := strings.TrimPrefix(subscription.Recipient, "g:")
			queue := make(chan *pb.QueueMessage, 1)
			hub.Register("consumer", "orders", group, queue)
			defer hub.Unregister("consumer")
			epoch := uint64(1)
			hub.SetDeliveryFence(func() (uint64, bool) { return epoch, true }, time.Minute)
			hub.GrantDeliveryPermit(epoch, time.Now())
			s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{
				Topic: "orders", Payload: payload, MaxRetries: new(uint32(0)),
			}, key))
			message := <-queue
			ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
			ledger.ConfigureRetryOutcomes(1)
			defer ledger.RemoveRetryTransports([][]byte{key})
			handler := NewConsumerHandler(zap.NewNop(), hub, nil)
			handler.RetryLedger, handler.MaxSendBytes = ledger, maxSendBytes
			frames, err := handler.beginLimitedSend(ctx, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: group}, message)
			s.Require().NoError(err)
			s.Require().NotNil(frames)
			s.Require().Equal(maxSendBytes > 0, len(frames.encoded) > 0)

			epoch++
			s.Require().False(hub.CanSend("consumer", message))
			sendCalls := 0
			stream := &autoAckSendStream{send: func(*pb.QueueMessage) error {
				sendCalls++
				return nil
			}}
			err = handler.sendDelivery(ctx, stream, "consumer", subscription.Recipient, message, frames)
			s.ErrorIs(err, errRetryTransportSkipped)
			s.Zero(sendCalls)
			due, err := raft.DueDeliveryAttempts(a.DB, time.Now().Add(time.Minute).UnixMilli())
			s.Require().NoError(err)
			s.Empty(due)
			state, err := raft.ReadDeliveryState(a.DB, key)
			s.Require().NoError(err)
			s.True(state.Ready(subscription.Recipient), "the fenced send must refund the attempt")
		})
	}
}

func (s *LimitedConsumerSuite) exerciseCanceledSend(blockAt deliverypb.RetryCommand_Operation, stall bool) {
	a, hub, producer, _ := openPublishTest(s.T(), "pebble")
	a.Config().Publish.ProposalTimeout = 25 * time.Millisecond
	ctx := context.Background()
	subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	s.Require().NoError(err)
	ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
		Topic: "orders", MaxRetries: new(uint32(0)),
	}}})
	s.Require().NoError(err)
	s.Require().True(ack.GetSuccess())
	keys := publishedKeys(s.T(), a.DB)
	s.Require().Len(keys, 1)
	key := keys[0]
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	init := &pb.SubscribeInit{Topic: "orders", GroupId: group}
	queue := make(chan *pb.QueueMessage, 1)
	hub.Register("consumer", "orders", group, queue)
	defer hub.Unregister("consumer")
	epoch := uint64(1)
	const timeout = 400 * time.Millisecond
	hub.SetDeliveryFence(func() (uint64, bool) { return epoch, true }, timeout)
	hub.GrantDeliveryPermit(epoch, time.Now())

	local := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	logger := zap.NewNop()
	handler := NewConsumerHandler(logger, hub, nil)
	interrupted := false
	handler.RetryLedger = dispatcher.NewDeliveryLedger(a.DB, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		value, err := local.ApplyRetry(&cmd)
		if cmd.Operation == blockAt && !interrupted {
			interrupted = true
			if stall {
				time.Sleep(timeout + 75*time.Millisecond)
			} else {
				epoch++
			}
		}
		return statemachine.Result{Value: value}, err
	})
	handler.RetryLedger.ConfigureRetryOutcomes(1)
	if stall {
		expiryCtx, stopExpiry := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			handler.RetryLedger.RunRetryExpiry(expiryCtx, time.Millisecond, nil, logger)
		}()
		defer func() { stopExpiry(); <-done }()
	}
	input := &pb.QueueMessage{Topic: "orders", MaxRetries: new(uint32(0))}
	s.Require().NotEmpty(hub.DispatchToTopic("orders", input, key))
	queued := <-queue
	wire, err := handler.beginLimitedSend(ctx, "consumer", init, queued)
	s.Require().NoError(err)
	s.Require().Nil(wire, "an expired or invalid permit must prevent any send")
	hub.RejectQueuedAttempt("consumer", queued)
	due, err := raft.DueDeliveryAttempts(a.DB, time.Now().Add(time.Minute).UnixMilli())
	s.Require().NoError(err)
	s.Empty(due, "the unsent reservation should be released immediately")
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(a.DB)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries, "expiry must not archive an attempt whose send was skipped")

	hub.GrantDeliveryPermit(epoch, time.Now())
	s.Require().NotEmpty(hub.DispatchToTopic("orders", input, key))
	wire, err = handler.beginLimitedSend(ctx, "consumer", init, <-queue)
	s.Require().NoError(err)
	s.Require().NotNil(wire)
	s.Equal(uint64(1), wire.Attempt)
}

func (s *LimitedConsumerSuite) TestSlowRaftProposalsLeaveAFullACKWindow() {
	a, hub, producer, _ := openPublishTest(s.T(), "pebble")
	const ackWindow = 250 * time.Millisecond
	a.Config().Publish.ProposalTimeout = 500 * time.Millisecond
	hub.SetDeliveryTimeout(ackWindow)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	s.Require().NoError(err)
	ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
		Topic: "orders", MaxRetries: new(uint32(0)),
	}}})
	s.Require().NoError(err)
	s.Require().True(ack.GetSuccess())
	keys := publishedKeys(s.T(), a.DB)
	s.Require().Len(keys, 1)
	key := keys[0]
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	init := &pb.SubscribeInit{Topic: "orders", GroupId: group}
	queue := make(chan *pb.QueueMessage, 1)
	hub.Register("consumer", "orders", group, queue)
	defer hub.Unregister("consumer")

	local := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	logger := zap.NewNop()
	ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		value, err := local.ApplyRetry(&cmd)
		if cmd.Operation == deliverypb.RetryCommand_START || cmd.Operation == deliverypb.RetryCommand_SENT {
			time.Sleep(150 * time.Millisecond)
		}
		return statemachine.Result{Value: value}, err
	})
	ledger.ConfigureRetryOutcomes(2)
	handler := NewConsumerHandler(logger, hub, nil)
	handler.RetryLedger = ledger
	stream := &blockedRetrySend{ctx: ctx, sending: make(chan *pb.QueueMessage, 1), unblock: make(chan struct{})}
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.sender(ctx, stream, "consumer", init, queue, errCh)
	}()
	defer func() { cancel(); <-done }()
	var workers sync.WaitGroup
	workers.Go(func() { ledger.RunRetryExpiry(ctx, time.Millisecond, nil, logger) })
	workers.Go(func() { ledger.RunRetryOutcomes(ctx, 2, logger) })
	defer func() { cancel(); workers.Wait() }()

	s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders", MaxRetries: new(uint32(0))}, key))
	var wire *pb.QueueMessage
	select {
	case wire = <-stream.sending:
	case err := <-errCh:
		s.Require().NoError(err)
		s.FailNow("sender returned without a delivery")
	case <-ctx.Done():
		s.FailNow("authorization never reached transport")
	}
	s.Equal(uint64(1), wire.Attempt)
	// The recovery deadline starts 150ms before Send. Wait until a live
	// reaper has seen that deadline, while still inside the real ACK window.
	time.Sleep(160 * time.Millisecond)
	due, err := raft.DueDeliveryAttempts(a.DB, time.Now().UnixMilli())
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	accepted, err := handler.finishLimitedSend("consumer", init, wire.DeliveryTag, true)
	s.Require().NoError(err)
	s.Require().True(accepted)
	close(stream.unblock)
	s.Require().Eventually(func() bool {
		state, err := raft.ReadDeliveryState(a.DB, key)
		return err == nil && !state.Needs(subscription.Recipient)
	}, time.Second, time.Millisecond)
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(a.DB)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
}

func (s *LimitedConsumerSuite) TestLaterACKCommitsWhileFirstOutcomeIsStalled() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	logger := zap.NewNop()
	hub := dispatcher.NewHub(dispatcher.NewRoundRobinStrategy(), logger, make(chan struct{}, 1))
	queue := make(chan *pb.QueueMessage, 2)
	hub.Register("consumer", "orders", "workers", queue)
	defer hub.Unregister("consumer")
	keys := [][]byte{
		utils.EventKey(1, utils.TopicHash("orders"), 1),
		utils.EventKey(1, utils.TopicHash("orders"), 2),
	}
	stream := new(ackTestStream)
	for i, key := range keys {
		s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{MaxRetries: new(uint32(0))}, key))
		token := bytes.Repeat([]byte{byte(i + 1)}, raft.AttemptTokenSize)
		s.Require().True(hub.BeginRetrySend("consumer", <-queue, token, time.Now().Add(time.Minute)))
		stream.frames = append(stream.frames, &pb.ConsumerFrame{Body: &pb.ConsumerFrame_Ack{
			Ack: &pb.AckRequest{DeliveryTag: append(bytes.Clone(key), token...), Success: true},
		}})
	}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondCommitted := make(chan struct{})
	ledger := dispatcher.NewDeliveryLedger(nil, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		if bytes.Equal(cmd.GetAttempts()[0].GetKey(), keys[0]) {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return statemachine.Result{}, ctx.Err()
			}
		} else {
			close(secondCommitted)
		}
		return statemachine.Result{}, nil
	})
	ledger.ConfigureRetryOutcomes(2)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		ledger.RunRetryOutcomes(ctx, 2, logger)
	}()
	defer func() { cancel(); <-workerDone }()
	handler := NewConsumerHandler(logger, hub, nil)
	handler.RetryLedger = ledger
	errCh := make(chan error, 1)
	go handler.receiver(stream, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: "workers"}, errCh)
	s.Require().Eventually(func() bool { return len(errCh) > 0 }, time.Second, time.Millisecond)
	s.Require().NoError(<-errCh)
	select {
	case <-firstStarted:
	case <-ctx.Done():
		s.FailNow("the first outcome was never proposed")
	}
	select {
	case <-secondCommitted:
	case <-ctx.Done():
		s.FailNow("the second outcome did not commit while the first was blocked")
	}
	close(releaseFirst)
}

func (s *LimitedConsumerSuite) TestAutoACKReservesOutcomeSpaceBeforeTransport() {
	s.exerciseRetryOutcomeCapacity(true)
}

func (s *LimitedConsumerSuite) TestSuccessfulAutoACKSendWinsExpiryWhileMetricsWait() {
	t := s.T()
	a, hub, producer, _ := openPublishTest(t, "pebble")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	published, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
		Topic: "orders", MaxRetries: new(uint32(0)),
	}}})
	require.NoError(t, err)
	require.True(t, published.GetSuccess())
	keys := publishedKeys(t, a.DB)
	require.Len(t, keys, 1)
	key := keys[0]

	const worker = "consumer"
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	queue := make(chan *pb.QueueMessage, 1)
	hub.SetDeliveryTimeout(300 * time.Millisecond)
	hub.Register(worker, "orders", group, queue)
	defer hub.Unregister(worker)
	require.NotEmpty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{
		Topic: "orders", MaxRetries: new(uint32(0)),
	}, key))
	queued := <-queue
	queue <- queued

	logger := zap.NewNop()
	ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	defer ledger.RemoveRetryTransports(keys)
	var workers sync.WaitGroup
	workers.Go(func() { ledger.RunRetryOutcomes(ctx, 1, logger) })
	workers.Go(func() { ledger.RunRetryExpiry(ctx, 25*time.Millisecond, nil, logger) })
	defer func() { cancel(); workers.Wait() }()

	original := metrics.DeliveryGRPCSendsTotal
	defer func() { metrics.DeliveryGRPCSendsTotal = original }()
	metrics.DeliveryGRPCSendsTotal = prometheus.V2.NewCounterVec(prometheus.CounterVecOpts{
		CounterOpts: prometheus.CounterOpts{Name: "test_delivery_grpc_sends"},
		VariableLabels: prometheus.ConstrainedLabels{{Name: "topic", Constraint: func(topic string) string {
			// Wait for the ACK or expiry to settle the delivery before telemetry returns.
			require.Eventually(t, func() bool {
				state, err := raft.ReadDeliveryState(a.DB, key)
				return err == nil && state == nil
			}, 2*time.Second, 10*time.Millisecond)
			page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 1}).Read(a.DB)
			require.NoError(t, err)
			require.Empty(t, page.Page.Entries)
			cancel()
			return topic
		}}},
	})

	handler := NewConsumerHandler(logger, hub, nil)
	handler.RetryLedger = ledger
	stream := &autoAckSendStream{send: func(*pb.QueueMessage) error {
		require.True(t, hub.CanSend(worker, queued), "the send must succeed within the attempt window")
		return nil
	}}
	errs := make(chan error, 1)
	handler.sender(ctx, stream, worker, &pb.SubscribeInit{Topic: "orders", GroupId: group, AutoAck: true}, queue, errs)
	require.ErrorIs(t, <-errs, context.Canceled)
}

func (s *LimitedConsumerSuite) TestCanceledAutoACKDuringAuthorizationRefundsWithoutSend() {
	a, hub, producer, _ := openPublishTest(s.T(), "pebble")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	s.Require().NoError(err)
	ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
		Topic: "orders", MaxRetries: new(uint32(0)),
	}}})
	s.Require().NoError(err)
	s.Require().True(ack.GetSuccess())
	keys := publishedKeys(s.T(), a.DB)
	s.Require().Len(keys, 1)
	key := keys[0]
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	queue := make(chan *pb.QueueMessage, 1)
	hub.Register("consumer", "orders", group, queue)
	defer hub.Unregister("consumer")
	input := &pb.QueueMessage{Topic: "orders", MaxRetries: new(uint32(0))}
	s.Require().NotEmpty(hub.DispatchToTopic("orders", input, key))

	logger := zap.NewNop()
	local := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	sentCommitted := false
	ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, func(raw []byte) (statemachine.Result, error) {
		var cmd deliverypb.RetryCommand
		if err := proto.Unmarshal(raw[1:], &cmd); err != nil {
			return statemachine.Result{}, err
		}
		value, err := local.ApplyRetry(&cmd)
		if cmd.Operation == deliverypb.RetryCommand_SENT && err == nil && value != 0 && !sentCommitted {
			sentCommitted = true
			cancel()
		}
		return statemachine.Result{Value: value}, err
	})
	ledger.ConfigureRetryOutcomes(1)
	defer ledger.RemoveRetryTransports([][]byte{key})
	handler := NewConsumerHandler(logger, hub, nil)
	handler.RetryLedger = ledger
	init := &pb.SubscribeInit{Topic: "orders", GroupId: group, AutoAck: true}
	sendCalls := 0
	stream := &autoAckSendStream{send: func(*pb.QueueMessage) error {
		sendCalls++
		return ctx.Err()
	}}
	errCh := make(chan error, 1)
	handler.sender(ctx, stream, "consumer", init, queue, errCh)
	s.Require().ErrorIs(<-errCh, context.Canceled)
	s.Require().True(sentCommitted, "cancellation followed a successful SENT apply")
	s.Zero(sendCalls)
	due, err := raft.DueDeliveryAttempts(a.DB, time.Now().Add(time.Minute).UnixMilli())
	s.Require().NoError(err)
	s.Empty(due)
	state, err := raft.ReadDeliveryState(a.DB, key)
	s.Require().NoError(err)
	s.True(state.Ready(subscription.Recipient))

	s.Require().NotEmpty(hub.DispatchToTopic("orders", input, key))
	next, err := handler.beginLimitedSend(context.Background(), "consumer", init, <-queue)
	s.Require().NoError(err)
	s.Require().NotNil(next, "the canceled authorization must release its auto-ACK slot")
	s.Equal(uint64(1), next.Attempt)
}

func (s *LimitedConsumerSuite) TestManualACKReservesOutcomeSpaceBeforeTransport() {
	s.exerciseRetryOutcomeCapacity(false)
}

func (s *LimitedConsumerSuite) TestDisconnectedManualStreamReleasesShutdownReservation() {
	a, hub, producer, _ := openPublishTest(s.T(), "pebble")
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelDrain()
	streamCtx, disconnect := context.WithCancel(drainCtx)
	defer disconnect()
	subscription, err := a.Subscriptions.Create(drainCtx, "orders", "workers", "")
	s.Require().NoError(err)
	ack, err := producer.processBatch(drainCtx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
		Topic: "orders", MaxRetries: new(uint32(0)),
	}}})
	s.Require().NoError(err)
	s.Require().True(ack.GetSuccess())
	keys := publishedKeys(s.T(), a.DB)
	s.Require().Len(keys, 1)
	key := keys[0]
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	queue := make(chan *pb.QueueMessage, 1)
	hub.SetDeliveryTimeout(time.Minute)
	hub.Register("consumer", "orders", group, queue)
	defer hub.Unregister("consumer")
	s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders", MaxRetries: new(uint32(0))}, key))
	logger := zap.NewNop()
	ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	handler := NewConsumerHandler(logger, hub, nil)
	handler.RetryLedger = ledger
	sent := make(chan struct{}, 1)
	stream := &autoAckSendStream{send: func(*pb.QueueMessage) error { sent <- struct{}{}; return nil }}
	errCh := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		handler.sender(streamCtx, stream, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: group}, queue, errCh)
	})
	defer func() { disconnect(); cancelDrain(); workers.Wait() }()
	select {
	case <-sent:
	case err := <-errCh:
		s.Require().NoError(err)
		s.FailNow("manual delivery did not reach transport")
	case <-drainCtx.Done():
		s.FailNow("manual delivery did not reach transport")
	}

	ledger.StopRetryOutcomeAdmission()
	outcomesDone := make(chan struct{})
	workers.Go(func() {
		defer close(outcomesDone)
		ledger.RunRetryOutcomes(drainCtx, 1, logger)
	})
	disconnect()
	select {
	case <-outcomesDone:
		s.Require().NoError(drainCtx.Err())
	case <-drainCtx.Done():
		s.FailNow("the disconnected stream kept the drain open")
	}
	due, err := raft.DueDeliveryAttempts(a.DB, time.Now().Add(2*time.Minute).UnixMilli())
	s.Require().NoError(err)
	s.Require().Len(due, 1, "disconnect must leave the durable attempt for expiry")
	s.Equal(key, due[0].Key)
}

func (s *LimitedConsumerSuite) exerciseRetryOutcomeCapacity(auto bool) {
	a, hub, producer, _ := openPublishTest(s.T(), "pebble")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	s.Require().NoError(err)
	message := &pb.PublishMessage{Topic: "orders", MaxRetries: new(uint32(0))}
	ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{message, message}})
	s.Require().NoError(err)
	s.Require().True(ack.GetSuccess())
	keys := publishedKeys(s.T(), a.DB)
	s.Require().Len(keys, 2)
	const consumerID = "consumer"
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	queue := make(chan *pb.QueueMessage, 2)
	hub.Register(consumerID, "orders", group, queue)
	defer hub.Unregister(consumerID)
	for _, key := range keys {
		s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders", MaxRetries: new(uint32(0))}, key))
	}

	logger := zap.NewNop()
	ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	handler := NewConsumerHandler(logger, hub, nil)
	handler.RetryLedger = ledger
	init := &pb.SubscribeInit{Topic: "orders", GroupId: group, AutoAck: auto}
	sent := make(chan *pb.QueueMessage, 2)
	stream := &autoAckSendStream{send: func(msg *pb.QueueMessage) error {
		// The second send should still be able to enqueue its pre-reserved
		// ACK if graceful shutdown starts before Send returns.
		if auto && bytes.HasPrefix(msg.DeliveryTag, keys[1]) {
			ledger.StopRetryOutcomeAdmission()
		}
		sent <- msg
		return nil
	}}
	errCh := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		handler.sender(ctx, stream, consumerID, init, queue, errCh)
	})
	defer func() { cancel(); workers.Wait() }()
	nextSend := func() *pb.QueueMessage {
		select {
		case msg := <-sent:
			return msg
		case err := <-errCh:
			s.Require().NoError(err)
			s.FailNow("sender stopped before delivery")
		case <-ctx.Done():
			s.FailNow("delivery did not reach transport")
		}
		return nil
	}
	first := nextSend()
	s.True(bytes.HasPrefix(first.DeliveryTag, keys[0]))
	s.Require().Eventually(func() bool {
		return len(queue) == 0 && hub.GroupInFlightCount() == 1
	}, time.Second, time.Millisecond)
	s.Empty(sent, "the second delivery must wait for a completion slot")
	state, err := raft.ReadDeliveryState(a.DB, keys[1])
	s.Require().NoError(err)
	s.True(state.Ready(subscription.Recipient), "backpressure must leave the attempt uncharged")
	if !auto {
		accepted, err := handler.finishLimitedSend(consumerID, init, first.DeliveryTag, true)
		s.Require().NoError(err)
		s.Require().True(accepted, "the timely manual ACK must be able to use its reserved slot")
	}

	outcomesDone := make(chan struct{})
	workers.Go(func() {
		defer close(outcomesDone)
		ledger.RunRetryOutcomes(ctx, 1, logger)
	})
	s.Require().Eventually(func() bool {
		state, err := raft.ReadDeliveryState(a.DB, keys[0])
		return err == nil && state == nil
	}, time.Second, time.Millisecond)
	s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders", MaxRetries: new(uint32(0))}, keys[1]))
	second := nextSend()
	s.True(bytes.HasPrefix(second.DeliveryTag, keys[1]))
	s.Equal(uint64(1), second.Attempt)
	if !auto {
		ledger.StopRetryOutcomeAdmission()
		accepted, err := handler.finishLimitedSend(consumerID, init, second.DeliveryTag, true)
		s.Require().NoError(err)
		s.Require().True(accepted, "shutdown must honor the active manual reservation")
	}
	select {
	case <-outcomesDone:
	case <-ctx.Done():
		s.FailNow("ACK did not drain during shutdown")
	}
	for _, key := range keys {
		_, _, err := a.DB.Get(key)
		s.ErrorIs(err, storage.ErrNotFound)
	}
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(a.DB)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
}

func (s *LimitedConsumerSuite) TestBlockedSendCannotRefundItsAttempt() {
	s.exerciseExpiredSend(false)
}

func (s *LimitedConsumerSuite) TestChunkedSendStopsAfterItsRecipientExpires() {
	s.exerciseExpiredSend(true)
}

func (s *LimitedConsumerSuite) exerciseExpiredSend(chunked bool) {
	a, hub, producer, _ := openPublishTest(s.T(), "pebble")
	logger := zap.NewNop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	subscription, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	s.Require().NoError(err)
	var otherRecipient string
	var payload []byte
	if chunked {
		other, err := a.Subscriptions.Create(ctx, "orders", "offline", "")
		s.Require().NoError(err)
		otherRecipient = other.Recipient
		payload = bytes.Repeat([]byte("chunk"), 500)
	}
	ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{
		Topic: "orders", Payload: payload, MaxRetries: new(uint32(0)),
	}}})
	s.Require().NoError(err)
	s.Require().True(ack.GetSuccess())
	keys := publishedKeys(s.T(), a.DB)
	s.Require().Len(keys, 1)
	key := keys[0]
	group := strings.TrimPrefix(subscription.Recipient, "g:")
	queue := make(chan *pb.QueueMessage, 1)
	hub.Register("consumer", "orders", group, queue)
	defer hub.Unregister("consumer")
	s.Require().NotEmpty(hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders", Payload: payload, MaxRetries: new(uint32(0))}, key))
	ledger := dispatcher.NewDeliveryLedger(a.DB, nil, nil, nil)
	ledger.ConfigureRetryOutcomes(1)
	handler := NewConsumerHandler(logger, hub, nil)
	handler.RetryLedger = ledger
	if chunked {
		handler.MaxSendBytes = 1024
	}
	stream := &blockedRetrySend{ctx: ctx, sending: make(chan *pb.QueueMessage, 1), unblock: make(chan struct{})}
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.sender(ctx, stream, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: group}, queue, errCh)
	}()
	defer func() { cancel(); <-done }()
	select {
	case msg := <-stream.sending:
		if chunked {
			s.Require().True(msg.GetMoreChunks())
		} else {
			s.Equal(uint64(1), msg.Attempt)
		}
	case <-ctx.Done():
		s.FailNow("send did not start")
	}
	// Advance the expiry clock while the first call is still in gRPC Send.
	expireAt := time.Now().Add(time.Minute).UnixMilli()
	due, err := raft.DueDeliveryAttempts(a.DB, expireAt)
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	value, err := ledger.ApplyRetry(&deliverypb.RetryCommand{Operation: deliverypb.RetryCommand_EXPIRE, NowMs: expireAt, Attempts: due})
	s.Require().NoError(err)
	s.Require().Equal(uint64(1), value, "expiry must charge the send already exposed to the transport")
	page, err := (raft.DeadLetterQuery{Topic: "orders", Limit: 10}).Read(a.DB)
	s.Require().NoError(err)
	s.Require().Len(page.Page.Entries, 1)
	s.Equal(uint64(1), page.Page.Entries[0].Attempts)
	if chunked {
		receipt, err := raft.ReadDeliveryState(a.DB, key)
		s.Require().NoError(err)
		s.Require().True(receipt.Needs(otherRecipient), "expiry completes only the sending recipient")
	}
	close(stream.unblock)
	if chunked {
		select {
		case err := <-errCh:
			s.Equal(codes.Aborted, status.Code(err))
		case <-stream.sending:
			s.FailNow("sender exposed another chunk after the attempt expired")
		case <-ctx.Done():
			s.FailNow("sender did not stop after expiry")
		}
	}
}

type blockedRetrySend struct {
	ackTestStream
	ctx     context.Context
	sending chan *pb.QueueMessage
	unblock chan struct{}
}

func (s *blockedRetrySend) Send(message *pb.QueueMessage) error {
	select {
	case s.sending <- message:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	select {
	case <-s.unblock:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

type ackTestStream struct {
	grpc.ServerStream
	frames []*pb.ConsumerFrame
}

func (s *ackTestStream) Send(*pb.QueueMessage) error { return nil }

func (s *ackTestStream) Recv() (*pb.ConsumerFrame, error) {
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

type retryDeleteBackend struct {
	keys [][][]byte
	fail bool
}

func (b *retryDeleteBackend) DeleteKeys(keys [][]byte) error {
	b.keys = append(b.keys, keys)
	if b.fail {
		b.fail = false
		return errors.New("quorum unavailable")
	}
	return nil
}

func inFlightGaugeValue(t *testing.T) float64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, metrics.MessagesInFlight.WithLabelValues("orders", "workers").Write(&metric))
	return metric.GetGauge().GetValue()
}

func TestAckRetainsInFlightUntilPersistenceSucceeds(t *testing.T) {
	for _, recipientACKs := range []bool{false, true} {
		name := "delete-backend"
		if recipientACKs {
			name = "recipient-acks"
		}
		t.Run(name, func(t *testing.T) {
			baseline := inFlightGaugeValue(t)
			hub := dispatcher.NewHub(dispatcher.NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
			queue := make(chan *pb.QueueMessage, 1)
			hub.Register("consumer", "orders", "workers", queue)
			key := []byte("delivery-tag")
			require.Equal(t, []string{"workers"}, hub.DispatchToTopic("orders", &pb.QueueMessage{}, key))
			message := <-queue
			require.True(t, hub.BeginSend("consumer", message))
			backend := &retryDeleteBackend{fail: true}
			deleter := dispatcher.NewDeleter(backend, time.Hour, zap.NewNop())
			deleter.OnDelete = func(key []byte) { hub.RemoveDeletedBatch([][]byte{key}) }
			deleter.OnAcknowledged = hub.RemoveAcknowledgedBatch
			var persisted [][]raft.DeliveryAck
			if recipientACKs {
				deleter.AcknowledgeBatch = func(acks []raft.DeliveryAck) error {
					require.Equal(t, 1, hub.GroupInFlightCount(), "persist before releasing the rebalance drain")
					persisted = append(persisted, acks)
					if len(persisted) == 1 {
						return errors.New("quorum unavailable")
					}
					return nil
				}
			}
			handler := NewConsumerHandler(zap.NewNop(), hub, deleter)
			ack := func(tag []byte, success bool) *pb.ConsumerFrame {
				return &pb.ConsumerFrame{Body: &pb.ConsumerFrame_Ack{Ack: &pb.AckRequest{DeliveryTag: tag, Success: success}}}
			}
			// Neither an invalid ACK, a duplicate ACK, nor a later NACK may
			// complete or release an already pending successful ACK.
			stream := &ackTestStream{frames: []*pb.ConsumerFrame{ack([]byte("other"), true), ack(key, true), ack(key, true), ack(key, false)}}
			errCh := make(chan error, 1)
			handler.receiver(stream, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: "workers"}, errCh)
			require.NoError(t, <-errCh)
			require.Equal(t, 1, hub.GroupInFlightCount())
			require.Equal(t, baseline+1, inFlightGaugeValue(t), "ACK receipt must not release the gauge before persistence")
			require.False(t, hub.CanSend("consumer", message), "ACKed attempts cannot be sent again while persistence is pending")
			require.False(t, hub.BeginSend("consumer", message))
			require.Empty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{}, key), "queued ACK must suppress redispatch")
			require.Empty(t, backend.keys)
			require.Empty(t, persisted)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			deleter.Run(ctx) // failed flush retains both the work and local tracking
			require.Equal(t, 1, hub.GroupInFlightCount())
			require.Equal(t, baseline+1, inFlightGaugeValue(t))
			deleter.Run(ctx) // successful retry releases the in-flight record
			require.Zero(t, hub.GroupInFlightCount())
			require.Equal(t, baseline, inFlightGaugeValue(t))
			deleter.Run(ctx) // completed batches must not be proposed again
			require.Equal(t, baseline, inFlightGaugeValue(t))
			if recipientACKs {
				expected := []raft.DeliveryAck{{Key: key, Recipient: "g:workers"}}
				require.Equal(t, [][]raft.DeliveryAck{expected, expected}, persisted)
				require.Empty(t, backend.keys)
			} else {
				require.Equal(t, [][][]byte{{key}, {key}}, backend.keys)
			}
		})
	}
}

func TestNackReleasesInFlightWithoutPersistence(t *testing.T) {
	baseline := inFlightGaugeValue(t)
	hub := dispatcher.NewHub(dispatcher.NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	queue := make(chan *pb.QueueMessage, 1)
	hub.Register("consumer", "orders", "workers", queue)
	key := []byte("delivery-tag")
	require.NotEmpty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{}, key))
	require.True(t, hub.BeginSend("consumer", <-queue))
	backend := &retryDeleteBackend{}
	deleter := dispatcher.NewDeleter(backend, time.Hour, zap.NewNop())
	deleter.AcknowledgeBatch = func([]raft.DeliveryAck) error {
		t.Fatal("NACK must not persist an acknowledgment")
		return nil
	}
	var nacked [][]byte
	hub.OnNack = func(key []byte) { nacked = append(nacked, key) }
	stream := &ackTestStream{frames: []*pb.ConsumerFrame{{Body: &pb.ConsumerFrame_Ack{Ack: &pb.AckRequest{DeliveryTag: key}}}}}
	errCh := make(chan error, 1)
	NewConsumerHandler(zap.NewNop(), hub, deleter).receiver(stream, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: "workers"}, errCh)
	require.NoError(t, <-errCh)
	require.Zero(t, hub.GroupInFlightCount())
	require.Equal(t, baseline, inFlightGaugeValue(t), "NACK must subtract exactly once")
	require.Equal(t, [][]byte{key}, nacked)
	require.NotEmpty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{}, key))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deleter.Run(ctx)
	require.Empty(t, backend.keys)
}
