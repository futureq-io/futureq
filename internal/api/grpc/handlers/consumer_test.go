package handlers

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/dispatcher"
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

func TestAckReleasesRebalanceBeforeDeleteCommits(t *testing.T) {
	hub := dispatcher.NewHub(dispatcher.NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	hub.Register("consumer", "orders", "workers", make(chan *pb.QueueMessage, 1))
	key := []byte("delivery-tag")
	require.Equal(t, []string{"workers"}, hub.DispatchToTopic("orders", &pb.QueueMessage{}, key))
	require.Equal(t, 1, hub.GroupInFlightCount())
	backend := &retryDeleteBackend{fail: true}
	deleter := dispatcher.NewDeleter(backend, time.Hour, zap.NewNop())
	handler := NewConsumerHandler(zap.NewNop(), hub, deleter)
	ack := func(tag []byte) *pb.ConsumerFrame {
		return &pb.ConsumerFrame{Body: &pb.ConsumerFrame_Ack{Ack: &pb.AckRequest{DeliveryTag: tag, Success: true}}}
	}
	stream := &ackTestStream{frames: []*pb.ConsumerFrame{ack([]byte("other")), ack(key), ack(key)}}
	errCh := make(chan error, 1)
	handler.receiver(stream, "consumer", &pb.SubscribeInit{Topic: "orders", GroupId: "workers"}, errCh)
	require.NoError(t, <-errCh)
	require.Zero(t, hub.GroupInFlightCount(), "ACK must release the rebalance without waiting for Raft")
	require.Empty(t, backend.keys, "deletion is still queued")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deleter.Run(ctx) // shutdown flush fails and requeues the deletion
	require.Equal(t, [][][]byte{{key}}, backend.keys, "invalid and repeated ACKs must not enqueue deletions")
	deleter.Run(ctx) // another flush retries the same key
	require.Equal(t, [][][]byte{{key}, {key}}, backend.keys)
}
