package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type autoAckSendStream struct {
	grpc.ServerStream
	send func(*pb.QueueMessage) error
}

func (s *autoAckSendStream) Send(msg *pb.QueueMessage) error  { return s.send(msg) }
func (s *autoAckSendStream) Recv() (*pb.ConsumerFrame, error) { panic("unused") }

func TestAutoAckSenderCompletion(t *testing.T) {
	for _, tc := range []struct {
		name           string
		auto, failSend bool
	}{
		{"auto-success", true, false},
		{"auto-send-failure", true, true},
		{"manual-success", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := dispatcher.NewHub(dispatcher.NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
			defer hub.Unregister("consumer")
			queue := make(chan *pb.QueueMessage, 1)
			hub.Register("consumer", "orders", "workers", queue)
			key := []byte("delivery-tag")
			require.NotEmpty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{Topic: "orders"}, key))
			deleter := dispatcher.NewDeleter(&retryDeleteBackend{}, time.Hour, zap.NewNop())
			var persisted [][]raft.DeliveryAck
			deleter.AcknowledgeBatch = func(acks []raft.DeliveryAck) error {
				persisted = append(persisted, acks)
				if len(persisted) == 1 {
					return errors.New("quorum unavailable")
				}
				return nil
			}
			deleter.OnAcknowledged = hub.RemoveAcknowledgedBatch
			h := NewConsumerHandler(zap.NewNop(), hub, deleter)
			init := &pb.SubscribeInit{Topic: "orders", GroupId: "workers", AutoAck: tc.auto}
			flush := func() {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				deleter.Run(ctx)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &autoAckSendStream{send: func(*pb.QueueMessage) error {
				if tc.auto {
					// BeginSend has happened, but Send has not succeeded yet.
					// Manual frames must neither complete nor release this attempt.
					frames := &ackTestStream{frames: []*pb.ConsumerFrame{
						{Body: &pb.ConsumerFrame_Ack{Ack: &pb.AckRequest{DeliveryTag: key, Success: true}}},
						{Body: &pb.ConsumerFrame_Ack{Ack: &pb.AckRequest{DeliveryTag: key, Success: false}}},
					}}
					errs := make(chan error, 1)
					h.receiver(frames, "consumer", init, errs)
					require.NoError(t, <-errs)
				}
				flush()
				require.Empty(t, persisted, "no persistence before successful Send")
				require.Equal(t, 1, hub.GroupInFlightCount(), "manual NACK must not release auto-ack attempts")
				cancel()
				if tc.failSend {
					return errors.New("transport failed")
				}
				return nil
			}}
			errs := make(chan error, 1)
			h.sender(ctx, stream, "consumer", init, queue, errs)
			<-errs
			flush()
			require.Equal(t, 1, hub.GroupInFlightCount(), "failed persistence must retain in-flight state")
			flush()
			if tc.auto && !tc.failSend {
				expected := []raft.DeliveryAck{{Key: key, Recipient: "g:workers"}}
				require.Equal(t, [][]raft.DeliveryAck{expected, expected}, persisted)
				require.Zero(t, hub.GroupInFlightCount())
			} else {
				require.Empty(t, persisted)
			}
		})
	}
}

func TestAutoAckRejectedBeforeBinding(t *testing.T) {
	_, hub, _, _ := openPublishTest(t, "pebble")
	stream := &ackTestStream{frames: []*pb.ConsumerFrame{{Body: &pb.ConsumerFrame_Init{Init: &pb.SubscribeInit{
		Topic: "orders", GroupId: "workers", AutoAck: true,
	}}}}}
	// No durable subscription or stream context is needed: policy is checked
	// before creating a session or registering a worker in the hub.
	err := NewConsumerHandler(zap.NewNop(), hub, nil).Subscribe(stream)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Empty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{}, []byte("key")))
}
