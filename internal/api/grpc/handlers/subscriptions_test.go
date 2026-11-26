package handlers

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type DeadLetterDeliverySuite struct{ suite.Suite }

func TestDeadLetterDeliverySuite(t *testing.T) {
	suite.Run(t, new(DeadLetterDeliverySuite))
}

func (s *DeadLetterDeliverySuite) TestDeadLetterAfterNACKOrDisconnect() {
	for _, engine := range []string{"pebble", "bolt"} {
		s.Run(engine, func() { s.exerciseDeadLetters(engine) })
	}
}

func (s *DeadLetterDeliverySuite) exerciseDeadLetters(engine string) {
	a, hub, producer, _ := openPublishTest(s.T(), engine)
	a.Config().Delivery.Subscriptions.AllowAutoAck = true
	logger := zap.NewNop()
	ledger := dispatcher.NewDeliveryLedger(a.DB, hub.LocalDeliveryRecipients, dispatcher.NewDirectDeleteBackend(a.DB, logger), nil)
	ledger.StandaloneReceipts = raft.NewStandaloneReceiptCache(1<<20, 128)
	producer.StandalonePublishLedger = ledger
	a.Subscriptions.ApplyLocal = ledger.ApplySubscription
	deleter := dispatcher.NewDeleter(ledger, time.Millisecond, logger)
	deleter.AcknowledgeBatch = ledger.Acknowledge
	deleter.OnAcknowledged = hub.RemoveAcknowledgedBatch
	d := dispatcher.NewDispatcher(a.DB, hub, deleter, time.Millisecond, 750*time.Millisecond, make(chan struct{}, 1), logger)
	d.PublishedReceipts = true
	hub.OnNack = d.RemoveInFlight
	ledger.OnDelete = hub.RemoveDeletedBatch

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	done := make(chan struct{}, 4)
	for _, run := range []func(context.Context){d.Run, deleter.Run, a.Subscriptions.Run, func(ctx context.Context) {
		ledger.RunRetryExpiry(ctx, time.Millisecond, nil, logger)
	}} {
		go func() { defer func() { done <- struct{}{} }(); run(ctx) }()
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	s.T().Cleanup(func() {
		cancel()
		server.Stop()
		_ = listener.Close()
		for range 4 {
			<-done
		}
	})
	consumer := NewConsumerHandler(logger, hub, deleter)
	consumer.RetryLedger = ledger
	pb.RegisterFutureQConsumerServer(server, consumer)
	pb.RegisterFutureQProducerServer(server, producer)
	pb.RegisterFutureQDeadLetterServer(server, &DeadLetterHandler{Ledger: ledger})
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///dead-letters", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	s.Require().NoError(err)
	s.T().Cleanup(func() { _ = conn.Close() })
	client := pb.NewFutureQConsumerClient(conn)
	dlq := pb.NewFutureQDeadLetterClient(conn)
	workers, err := client.CreateSubscription(ctx, &pb.SubscriptionRequest{Topic: "orders", GroupId: "workers"})
	s.Require().NoError(err)
	_, err = client.CreateSubscription(ctx, &pb.SubscriptionRequest{Topic: "orders", SubscriberId: "audit"})
	s.Require().NoError(err)
	connect := func(init *pb.SubscribeInit) grpc.BidiStreamingClient[pb.ConsumerFrame, pb.QueueMessage] {
		stream, err := client.Subscribe(ctx)
		s.Require().NoError(err)
		s.Require().NoError(stream.Send(&pb.ConsumerFrame{Body: &pb.ConsumerFrame_Init{Init: init}}))
		_, err = stream.Header()
		s.Require().NoError(err)
		return stream
	}
	worker := connect(&pb.SubscribeInit{Topic: "orders", GroupId: "workers"})
	audit := connect(&pb.SubscribeInit{Topic: "orders", SubscriberId: "audit"})
	publish, err := pb.NewFutureQProducerClient(conn).PublishStream(ctx)
	s.Require().NoError(err)
	send := func(payload string, retries uint32) {
		s.Require().NoError(publish.Send(&pb.PublishBatch{Messages: []*pb.PublishMessage{{
			Topic: "orders", Payload: []byte(payload), MaxRetries: &retries,
		}}}))
		ack, err := publish.Recv()
		s.Require().NoError(err)
		s.Require().True(ack.Success, ack.ErrorMessage)
	}
	ack := func(stream grpc.BidiStreamingClient[pb.ConsumerFrame, pb.QueueMessage], message *pb.QueueMessage, success bool) {
		s.Require().NoError(stream.Send(&pb.ConsumerFrame{Body: &pb.ConsumerFrame_Ack{Ack: &pb.AckRequest{
			DeliveryTag: message.DeliveryTag, Success: success,
		}}}))
	}
	send("rejected", 1)
	first, err := worker.Recv()
	s.Require().NoError(err)
	s.Equal(uint64(1), first.Attempt)
	audited, err := audit.Recv()
	s.Require().NoError(err)
	s.Equal(uint64(1), audited.Attempt)
	ack(audit, audited, true)
	ack(worker, first, false)
	second, err := worker.Recv()
	s.Require().NoError(err)
	s.Equal(uint64(2), second.Attempt)
	s.NotEqual(first.DeliveryTag, second.DeliveryTag)
	ack(worker, first, true)
	ack(worker, second, false)
	s.Require().Eventually(func() bool {
		page, err := dlq.ListDeadLetters(ctx, &pb.ListDeadLettersRequest{Topic: "orders"})
		return err == nil && len(page.Entries) == 1
	}, 2*time.Second, 5*time.Millisecond)

	s.Require().NoError(audit.CloseSend())
	_, err = audit.Recv()
	s.Require().ErrorIs(err, io.EOF)
	send("disconnected", 0)
	unacknowledged, err := worker.Recv()
	s.Require().NoError(err)
	s.Equal(uint64(1), unacknowledged.Attempt)
	s.Require().NoError(worker.CloseSend())
	s.Require().Eventually(func() bool {
		page, err := dlq.ListDeadLetters(ctx, &pb.ListDeadLettersRequest{Topic: "orders"})
		return err == nil && len(page.Entries) == 2
	}, 3*time.Second, 10*time.Millisecond)

	request := &pb.ListDeadLettersRequest{Topic: "orders", PageSize: 1}
	page, err := dlq.ListDeadLetters(ctx, request)
	s.Require().NoError(err)
	s.Require().Len(page.Entries, 1)
	s.Require().NotEmpty(page.NextPageToken)
	request.PageToken = page.NextPageToken
	next, err := dlq.ListDeadLetters(ctx, request)
	s.Require().NoError(err)
	s.Require().Len(next.Entries, 1)
	s.Empty(next.NextPageToken)
	for _, summary := range []*pb.DeadLetterSummary{page.Entries[0], next.Entries[0]} {
		s.Equal(workers.Generation, summary.Generation)
		s.Equal("workers", summary.GroupId)
		get := &pb.DeadLetterRequest{Topic: "orders", Id: summary.Id}
		entry, err := dlq.GetDeadLetter(ctx, get)
		s.Require().NoError(err)
		s.Require().NotNil(entry.Message.MaxRetries)
		switch summary.Reason {
		case pb.DeadLetterReason_DEAD_LETTER_REASON_NACK:
			s.Equal(uint64(2), summary.Attempts)
			s.Equal("rejected", string(entry.Message.Payload))
			s.Equal(uint32(1), *entry.Message.MaxRetries)
		case pb.DeadLetterReason_DEAD_LETTER_REASON_TIMEOUT:
			s.Equal(uint64(1), summary.Attempts)
			s.Equal("disconnected", string(entry.Message.Payload))
			s.Zero(*entry.Message.MaxRetries)
		default:
			s.Fail("unexpected dead-letter reason")
		}
		_, err = dlq.DeleteDeadLetter(ctx, get)
		s.Require().NoError(err)
		_, err = dlq.DeleteDeadLetter(ctx, get)
		s.Require().NoError(err)
		_, err = dlq.GetDeadLetter(ctx, get)
		s.Equal(codes.NotFound, status.Code(err))
	}

	auto := connect(&pb.SubscribeInit{Topic: "orders", SubscriberId: "audit", AutoAck: true})
	autoMessage, err := auto.Recv()
	s.Require().NoError(err)
	s.Equal("disconnected", string(autoMessage.Payload))
	s.Equal(uint64(1), autoMessage.Attempt)
	s.Require().Eventually(func() bool {
		_, closer, err := a.DB.Get(autoMessage.DeliveryTag[:len(autoMessage.DeliveryTag)-raft.AttemptTokenSize])
		if closer != nil {
			_ = closer.Close()
		}
		return err == storage.ErrNotFound
	}, 2*time.Second, 5*time.Millisecond)
	empty, err := dlq.ListDeadLetters(ctx, &pb.ListDeadLettersRequest{Topic: "orders"})
	s.Require().NoError(err)
	s.Empty(empty.Entries)
}

func TestDurableSubscriptionRPCAndOfflineUniversalDelivery(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("manual", func(t *testing.T) { testDurableSubscriptionDelivery(t, engine, false) })
			t.Run("auto", func(t *testing.T) { testDurableSubscriptionDelivery(t, engine, true) })
		})
	}
}

func testDurableSubscriptionDelivery(t *testing.T, engine string, autoAck bool) {
	a, hub, producer, _ := openPublishTest(t, engine)
	a.Config().Delivery.Subscriptions.AllowAutoAck = autoAck
	ledger := dispatcher.NewDeliveryLedger(a.DB, hub.LocalDeliveryRecipients, nil, nil)
	a.Subscriptions.ApplyLocal = ledger.ApplySubscription
	deleter := dispatcher.NewDeleter(ledger, time.Millisecond, zap.NewNop())
	deleter.AcknowledgeBatch = ledger.Acknowledge
	deleter.OnAcknowledged = hub.RemoveAcknowledgedBatch
	wake := make(chan struct{}, 1)
	d := dispatcher.NewDispatcher(a.DB, hub, deleter, time.Millisecond, time.Second, wake, zap.NewNop())
	d.TimeBucket = a.Config().Delivery.TimeBucket
	d.PrepareBatch = ledger.PublishedBatch
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{}, 3)
	go func() { defer func() { done <- struct{}{} }(); d.Run(ctx) }()
	go func() { defer func() { done <- struct{}{} }(); deleter.Run(ctx) }()
	go func() { defer func() { done <- struct{}{} }(); a.Subscriptions.Run(ctx) }()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterFutureQConsumerServer(server, NewConsumerHandler(zap.NewNop(), hub, deleter))
	go server.Serve(listener) //nolint:errcheck
	conn, err := grpc.NewClient("passthrough:///subscriptions", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		conn.Close()
		server.Stop()
		listener.Close()
		for i := 0; i < 3; i++ {
			<-done
		}
	})
	client := pb.NewFutureQConsumerClient(conn)
	group, err := client.CreateSubscription(ctx, &pb.SubscriptionRequest{Topic: "orders", GroupId: "workers"})
	require.NoError(t, err)
	universal, err := client.CreateSubscription(ctx, &pb.SubscriptionRequest{Topic: "orders", SubscriberId: "audit"})
	require.NoError(t, err)
	_, err = client.CreateSubscription(ctx, &pb.SubscriptionRequest{Topic: "orders"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", Payload: []byte("offline")}}})
	require.NoError(t, err)
	keys := publishedKeys(t, a.DB)
	require.Len(t, keys, 1)
	state, err := raft.ReadDeliveryState(a.DB, keys[0])
	require.NoError(t, err)
	require.ElementsMatch(t, []string{group.Generation, universal.Generation}, state.Recipients)
	connect := func(init *pb.SubscribeInit) grpc.BidiStreamingClient[pb.ConsumerFrame, pb.QueueMessage] {
		stream, err := client.Subscribe(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&pb.ConsumerFrame{Body: &pb.ConsumerFrame_Init{Init: init}}))
		headers, err := stream.Header()
		require.NoError(t, err)
		if init.AutoAck {
			require.Equal(t, []string{"true"}, headers.Get("subscription-auto-ack"))
		}
		return stream
	}
	audit := connect(&pb.SubscribeInit{Topic: "orders", SubscriberId: "audit", AutoAck: autoAck})
	first, err := audit.Recv()
	require.NoError(t, err)
	require.Equal(t, "offline", string(first.Payload))
	if !autoAck {
		require.NoError(t, audit.Send(&pb.ConsumerFrame{Body: &pb.ConsumerFrame_Ack{Ack: &pb.AckRequest{Success: true, DeliveryTag: first.DeliveryTag}}}))
	}
	require.Eventually(t, func() bool {
		state, err := raft.ReadDeliveryState(a.DB, keys[0])
		return err == nil && state != nil && !state.Needs(universal.Generation)
	}, time.Second, time.Millisecond)
	state, err = raft.ReadDeliveryState(a.DB, keys[0])
	require.NoError(t, err)
	require.True(t, state.Needs(group.Generation), "completing audit must retain the offline worker's obligation")
	audit.CloseSend() //nolint:errcheck
	workers := connect(&pb.SubscribeInit{Topic: "orders", GroupId: "workers"})
	second, err := workers.Recv()
	require.NoError(t, err)
	require.Equal(t, first.DeliveryTag, second.DeliveryTag)
	_, err = client.Unsubscribe(ctx, &pb.SubscriptionRequest{Topic: "orders", GroupId: "workers"})
	require.NoError(t, err)
	require.NoError(t, a.Subscriptions.Maintain(ctx, time.Now()))
	_, _, err = a.DB.Get(keys[0])
	require.ErrorIs(t, err, storage.ErrNotFound, "unsubscribe releases only remaining group obligation")
	workers.CloseSend() //nolint:errcheck
	newer, err := client.CreateSubscription(ctx, &pb.SubscriptionRequest{Topic: "orders", GroupId: "workers"})
	require.NoError(t, err)
	require.NotEqual(t, group.Generation, newer.Generation)
	require.True(t, strings.HasPrefix(newer.Generation, "g:s:"))
	missing, err := client.Subscribe(ctx)
	require.NoError(t, err)
	require.NoError(t, missing.Send(&pb.ConsumerFrame{Body: &pb.ConsumerFrame_Init{Init: &pb.SubscribeInit{Topic: "orders", SubscriberId: "missing"}}}))
	_, err = missing.Recv()
	require.Equal(t, codes.NotFound, status.Code(err))
	// A connection alone cannot create interest.
	names, err := raft.ReadSubscriptionRecipients(app.A.DB, "orders")
	require.NoError(t, err)
	require.Len(t, names, 2)
}
