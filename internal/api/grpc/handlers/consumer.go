package handlers

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/dispatcher"
	"github.com/futureq-io/futureq/internal/metrics"
	pb "github.com/futureq-io/protocol/proto/go"
)

// ConsumerHandler implements pb.FutureQConsumerServer.
type ConsumerHandler struct {
	pb.UnimplementedFutureQConsumerServer
	logger  *zap.Logger
	hub     *dispatcher.Hub
	deleter *dispatcher.Deleter
}

// NewConsumerHandler returns an initialised ConsumerHandler.
func NewConsumerHandler(logger *zap.Logger, hub *dispatcher.Hub, deleter *dispatcher.Deleter) *ConsumerHandler {
	return &ConsumerHandler{
		logger:  logger.Named("consumer"),
		hub:     hub,
		deleter: deleter,
	}
}

// Subscribe handles a bidirectional stream where the server pushes QueueMessage
// items to the client and the client replies with ConsumerFrame (AckRequest).
//
// Protocol:
//  1. The client must send a ConsumerFrame with a SubscribeInit as the first frame.
//     This declares the topic and optional consumer group for this connection.
//  2. All subsequent client frames must carry AckRequest.
//  3. The server pushes QueueMessage frames as messages become eligible.
//
// Group semantics:
//   - Empty group_id: universal consumer — receives every message on the topic.
//   - Non-empty group_id: competing consumer — races with other consumers in
//     the same group; only one receives each message.
//
// Delivery semantics: at-least-once.
//   - On ACK (success=true): the key is queued for Raft-replicated deletion.
//   - On NACK (success=false): the key is immediately removed from in-flight,
//     making the message eligible for re-dispatch on the next dispatcher tick.
func (h *ConsumerHandler) Subscribe(stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage]) error {
	// ─── Read and validate the SubscribeInit handshake ─────────────────────────
	init, err := h.readInit(stream)
	if err != nil {
		return err
	}

	// ─── Verify leadership in Raft mode ────────────────────────────────────────
	// TODO: WE SHALL ENABLE CONSUME ON REPLICAS
	if err := h.checkLeadership(init); err != nil {
		return err
	}

	// ─── Register consumer with the Hub ────────────────────────────────────────
	consumerID := uuid.New().String()
	ch := make(chan *pb.QueueMessage, 1024)
	h.hub.Register(consumerID, init.Topic, init.GroupId, ch)

	metrics.ActiveConsumers.WithLabelValues(init.Topic, init.GroupId).Inc()
	defer func() {
		h.hub.Unregister(consumerID)
		metrics.ActiveConsumers.WithLabelValues(init.Topic, init.GroupId).Dec()
		h.logger.Info("consumer disconnected",
			zap.String("id", consumerID),
			zap.String("topic", init.Topic),
			zap.String("group_id", init.GroupId),
		)
	}()

	h.logger.Info("consumer connected",
		zap.String("id", consumerID),
		zap.String("topic", init.Topic),
		zap.String("group_id", init.GroupId),
		zap.Bool("universal", init.GroupId == ""),
	)

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	errCh := make(chan error, 2)

	// ─── Sender goroutine: push messages to the consumer ─────────────────────
	go h.sender(ctx, stream, ch, errCh)

	// ─── Receiver goroutine: process ACK/NACK frames ─────────────────────────
	go h.receiver(stream, consumerID, init, errCh)

	err = <-errCh
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && err != io.EOF {
		h.logger.Error("consumer stream ended with error",
			zap.Error(err),
			zap.String("id", consumerID),
			zap.String("topic", init.Topic),
			zap.String("group_id", init.GroupId),
		)
		return status.Errorf(codes.Internal, "stream error: %v", err)
	}

	return nil
}

// readInit reads and validates the mandatory SubscribeInit first frame.
func (h *ConsumerHandler) readInit(stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage]) (*pb.SubscribeInit, error) {
	initFrame, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to read init frame: %v", err)
	}

	init := initFrame.GetInit()
	if init == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"first frame must be a SubscribeInit; got %T", initFrame.Body)
	}
	if init.Topic == "" {
		return nil, status.Errorf(codes.InvalidArgument, "SubscribeInit.topic must not be empty")
	}

	// group_id may be empty — that registers a universal (fan-out) consumer.

	return init, nil
}

// checkLeadership verifies this node is the Raft leader (if Raft is enabled).
func (h *ConsumerHandler) checkLeadership(init *pb.SubscribeInit) error {
	if app.A.NodeHost == nil {
		return nil
	}

	shardID := app.A.Config().Raft.ClusterID
	leaderID, _, valid, err := app.A.NodeHost.GetLeaderID(shardID)
	isLeader := err == nil && valid && leaderID == app.A.Config().Raft.NodeID

	if !isLeader {
		h.logger.Warn("rejecting consumer: not the leader",
			zap.String("topic", init.Topic),
			zap.String("group_id", init.GroupId),
		)
		return status.Errorf(codes.FailedPrecondition,
			"node is not the cluster leader")
	}

	return nil
}

// sender pushes messages from the consumer's channel to the gRPC stream.
func (h *ConsumerHandler) sender(
	ctx context.Context,
	stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage],
	ch chan *pb.QueueMessage,
	errCh chan error,
) {
	for {
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		case msg := <-ch:
			if err := stream.Send(msg); err != nil {
				errCh <- err
				return
			}
		}
	}
}

// receiver processes incoming ACK/NACK frames from the consumer.
func (h *ConsumerHandler) receiver(
	stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage],
	consumerID string,
	init *pb.SubscribeInit,
	errCh chan error,
) {
	for {
		frame, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				errCh <- nil
			} else {
				errCh <- err
			}
			return
		}

		ackReq := frame.GetAck()
		if ackReq == nil {
			h.logger.Warn("received unexpected SubscribeInit after handshake",
				zap.String("consumer_id", consumerID))
			continue
		}

		success := ackReq.Success

		metrics.ConsumerAckTotal.WithLabelValues(
			init.Topic, init.GroupId, boolToStr(success),
		).Inc()

		h.hub.RemoveInFlightForConsumer(consumerID, ackReq.DeliveryTag)
		if success {
			// ACK: queue the key for Raft-replicated deletion.
			h.deleter.MarkDeleted(ackReq.DeliveryTag)
		}
		// NACK: the key remains in storage; the dispatcher will re-deliver it.
		// In-flight gauge was incremented at dispatch time in the hub.
		metrics.MessagesInFlight.WithLabelValues(init.Topic, init.GroupId).Dec()
	}
}

func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
