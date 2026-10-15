package handlers

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

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

func TestAckRetainsInFlightUntilPersistenceSucceeds(t *testing.T) {
	for _, recipientACKs := range []bool{false, true} {
		name := "delete-backend"
		if recipientACKs {
			name = "recipient-acks"
		}
		t.Run(name, func(t *testing.T) {
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
			require.False(t, hub.CanSend("consumer", message), "ACKed attempts cannot be sent again while persistence is pending")
			require.False(t, hub.BeginSend("consumer", message))
			require.Empty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{}, key), "queued ACK must suppress redispatch")
			require.Empty(t, backend.keys)
			require.Empty(t, persisted)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			deleter.Run(ctx) // failed flush retains both the work and local tracking
			require.Equal(t, 1, hub.GroupInFlightCount())
			deleter.Run(ctx) // successful retry releases the in-flight record
			require.Zero(t, hub.GroupInFlightCount())
			deleter.Run(ctx) // completed batches must not be proposed again
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
	require.Equal(t, [][]byte{key}, nacked)
	require.NotEmpty(t, hub.DispatchToTopic("orders", &pb.QueueMessage{}, key))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deleter.Run(ctx)
	require.Empty(t, backend.keys)
}
