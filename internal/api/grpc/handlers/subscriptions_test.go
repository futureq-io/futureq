package handlers

import (
	"context"
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
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

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
