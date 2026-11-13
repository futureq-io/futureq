package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	pb "github.com/futureq-io/protocol/proto/go"
	"go.uber.org/zap"
)

func TestReviewAutoAckAfterSendRecordExpires(t *testing.T) {
	hub := dispatcher.NewHub(dispatcher.NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	defer hub.Unregister("review-consumer")
	queue := make(chan *pb.QueueMessage, 1)
	hub.Register("review-consumer", "orders", "workers", queue)
	key := []byte("review-key")
	hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, key)
	deleter := dispatcher.NewDeleter(&retryDeleteBackend{}, time.Hour, zap.NewNop())
	var persisted []raft.DeliveryAck
	deleter.AcknowledgeBatch = func(acks []raft.DeliveryAck) error {
		persisted = append(persisted, acks...)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &autoAckSendStream{send: func(*pb.QueueMessage) error {
		// The scheduler can expire the record while flow control blocks Send.
		hub.ExpireInFlight(time.Now().Add(time.Hour))
		cancel()
		return nil // The transport eventually accepts the message.
	}}
	h := NewConsumerHandler(zap.NewNop(), hub, deleter)
	errs := make(chan error, 1)
	h.sender(ctx, stream, "review-consumer", &pb.SubscribeInit{Topic: "orders", GroupId: "workers", AutoAck: true}, queue, errs)
	<-errs
	flushCtx, stopFlush := context.WithCancel(context.Background())
	stopFlush()
	deleter.Run(flushCtx)
	if len(persisted) != 1 {
		t.Fatalf("successful auto-ack send must persist one completion; got %d", len(persisted))
	}
}
