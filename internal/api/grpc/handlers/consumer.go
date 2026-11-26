package handlers

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/dispatcher"
	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
)

// ConsumerHandler implements pb.FutureQConsumerServer.
type ConsumerHandler struct {
	pb.UnimplementedFutureQConsumerServer
	logger       *zap.Logger
	hub          *dispatcher.Hub
	deleter      *dispatcher.Deleter
	RetryLedger  *dispatcher.DeliveryLedger
	MaxSendBytes int
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
//   - Empty group_id requires a stable subscriber_id for durable universal delivery.
//   - Non-empty group_id: competing consumer — races with other consumers in
//     the same group; only one receives each message.
//
// Delivery semantics: manual ACK/NACK by default. Auto-ack streams complete
// each recipient after a successful Send, without waiting for client processing.
//   - On ACK (success=true): the key is queued for Raft-replicated deletion.
//   - On NACK (success=false): the key is immediately removed from in-flight,
//     making the message eligible for re-dispatch on the next dispatcher tick.
func (h *ConsumerHandler) Subscribe(stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage]) error {
	// ─── Read and validate the SubscribeInit handshake ─────────────────────────
	init, err := h.readInit(stream)
	if err != nil {
		return err
	}
	if init == nil {
		return nil
	}
	if init.AutoAck && !app.A.Config().Delivery.Subscriptions.AllowAutoAck {
		return status.Error(codes.PermissionDenied, "auto-ack subscriptions are disabled")
	}
	consumerID := uuid.New().String()
	service := app.A.Subscriptions
	callCtx, callCancel := context.WithTimeout(stream.Context(), 10*time.Second)
	subscription, err := service.Connect(callCtx, init.Topic, init.GroupId, init.SubscriberId, consumerID)
	callCancel()
	if err != nil {
		return subscriptionError(err)
	}
	defer service.DisconnectAsync(consumerID)
	// Each durable interest is an independent logical group internally. Its UUID
	// generation isolates old ACKs and assignments after unsubscribe/recreate.
	init.GroupId = strings.TrimPrefix(subscription.Recipient, "g:")

	ch := make(chan *pb.QueueMessage, app.A.Config().Delivery.ConsumerQueueSize)
	clusteredGroup := app.A.NodeHost != nil
	if clusteredGroup {
		member := metadata.ConsumerMember{ID: consumerID, NodeID: app.A.Config().Cluster.NodeID}
		err = h.hub.WithConsumerRegistry(func() error {
			h.hub.Register(consumerID, init.Topic, init.GroupId, ch)
			ctx, cancel := context.WithTimeout(stream.Context(), 10*time.Second)
			defer cancel()
			if err := app.A.MetadataSvc.ChangeConsumer(ctx, app.A.Config().Cluster.ShardID, init.Topic, init.GroupId, member, true); err != nil {
				h.hub.Unregister(consumerID)
				return err
			}
			return nil
		})
		if err != nil {
			return status.Errorf(codes.Unavailable, "failed to register consumer: %v", err)
		}
	} else {
		h.hub.Register(consumerID, init.Topic, init.GroupId, ch)
	}

	metrics.ActiveConsumers.WithLabelValues(init.Topic, init.GroupId).Inc()
	defer func() {
		if clusteredGroup {
			if err := h.hub.WithConsumerRegistry(func() error {
				h.hub.Unregister(consumerID)
				member := metadata.ConsumerMember{ID: consumerID, NodeID: app.A.Config().Cluster.NodeID}
				app.A.MetadataSvc.RemoveConsumerAsync(app.A.Config().Cluster.ShardID, init.Topic, init.GroupId, member)
				return nil
			}); err != nil {
				h.logger.Warn("failed to unregister consumer; reconciliation will retry", zap.Error(err))
			}
		} else {
			h.hub.Unregister(consumerID)
		}
		metrics.ActiveConsumers.WithLabelValues(init.Topic, init.GroupId).Dec()
		h.logger.Info("consumer disconnected",
			zap.String("id", consumerID),
			zap.String("topic", init.Topic),
			zap.String("group_id", init.GroupId),
		)
	}()
	if clusteredGroup {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for !app.A.MetadataSM.ConsumerActive(init.Topic, init.GroupId, consumerID) {
			select {
			case <-stream.Context().Done():
				return stream.Context().Err()
			case <-ticker.C:
			}
		}
	}

	headers := grpcmetadata.Pairs("subscription-generation", subscription.Recipient)
	if init.AutoAck {
		headers.Set("subscription-auto-ack", "true")
	}
	if err := stream.SendHeader(headers); err != nil {
		return err
	}

	h.logger.Info("consumer connected",
		zap.String("id", consumerID),
		zap.String("topic", init.Topic),
		zap.String("group_id", init.GroupId),
		zap.Bool("universal", init.SubscriberId != ""),
	)

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	errCh := make(chan error, 3)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !service.Alive(subscription.Recipient, consumerID) {
					select {
					case errCh <- status.Error(codes.NotFound, "subscription removed or session expired"):
					case <-ctx.Done():
					}
					return
				}
			}
		}
	}()

	// ─── Sender goroutine: push messages to the consumer ─────────────────────
	go h.sender(ctx, stream, consumerID, init, ch, errCh)

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
		if status.Code(err) != codes.Unknown {
			return err
		}
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

	if err := validateSubscription(&pb.SubscriptionRequest{Topic: init.Topic, GroupId: init.GroupId, SubscriberId: init.SubscriberId}); err != nil {
		return nil, err
	}

	return init, nil
}

// sender pushes messages from the consumer's channel to the gRPC stream.
func (h *ConsumerHandler) sender(
	ctx context.Context,
	stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage],
	consumerID string,
	init *pb.SubscribeInit,
	ch chan *pb.QueueMessage,
	errCh chan error,
) {
	recipient := metadata.DeliveryRecipient(init.GroupId, consumerID)
	for {
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		case msg := <-ch:
			wire := msg
			if msg.MaxRetries != nil {
				var err error
				wire, err = h.beginLimitedSend(consumerID, init, msg)
				if err != nil {
					errCh <- err
					return
				}
			} else if !h.hub.BeginSend(consumerID, msg) {
				wire = nil
			}
			if wire == nil {
				h.hub.RejectQueuedAttempt(consumerID, msg)
				continue // expired, ACKed, or fenced buffered delivery
			}
			// Observe before Send: consumer backpressure can block this call.
			// Count attempted sends here, including attempts that fail in Send.
			metrics.DeliverySendLatenessMs.WithLabelValues(msg.Topic).Observe(float64(time.Now().UnixNano())/1e6 - float64(msg.EnqueuedAtUnixMs+msg.DelayMs))
			if err := h.sendDelivery(stream, wire, func() error {
				return h.checkDeliveryChunk(consumerID, recipient, msg, wire)
			}); err != nil {
				errCh <- err
				return
			}
			metrics.DeliveryGRPCSendsTotal.WithLabelValues(msg.Topic).Inc()
			if init.AutoAck {
				if msg.MaxRetries != nil {
					if _, err := h.finishLimitedSend(consumerID, init, wire.DeliveryTag, true, false); err != nil {
						errCh <- err
						return
					}
					continue
				}
				// Expiry or disconnect may remove local tracking while Send is
				// blocked. A successful send still completes this generation's
				// recipient; local tracking is only a duplicate-send guard.
				h.hub.MarkAcknowledgedSent(consumerID, msg.DeliveryTag)
				// Use the same recipient ledger and retry queue as manual ACKs.
				// Never complete an attempt before Send succeeds, and never
				// release another subscription's obligation to this payload.
				if !h.deleter.TryMarkAcknowledged(msg.DeliveryTag, metadata.DeliveryRecipient(init.GroupId, consumerID)) {
					errCh <- status.Error(codes.ResourceExhausted, "durable ACK queue full; retry subscription")
					return
				}
			}
		}
	}
}

func (h *ConsumerHandler) checkDeliveryChunk(consumerID, recipient string, queued, wire *pb.QueueMessage) error {
	if queued.MaxRetries != nil {
		active, err := h.RetryLedger.RetrySendActive(queued.DeliveryTag, recipient, wire.DeliveryTag[utils.EventKeySize:])
		if err != nil {
			return status.Errorf(codes.Unavailable, "read chunked delivery attempt: %v", err)
		}
		if !active {
			return status.Error(codes.Aborted, "delivery attempt ended during chunked send")
		}
	}
	if !h.hub.CanSend(consumerID, queued) {
		return status.Error(codes.Aborted, "delivery permit or deadline ended during chunked send")
	}
	return nil
}

func (h *ConsumerHandler) sendDelivery(stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage], message *pb.QueueMessage, checkChunk func() error) error {
	if h.MaxSendBytes <= 0 || proto.Size(message) <= h.MaxSendBytes {
		return stream.Send(message)
	}
	data, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	// Reserve the chunk field tag and the "more chunks" boolean. The bytes
	// field's length prefix grows with each varint boundary.
	overhead := protowire.SizeTag(8) + protowire.SizeTag(9) + 1
	chunkSize := min(32<<10, h.MaxSendBytes-overhead-1)
	for chunkSize > 0 && overhead+protowire.SizeBytes(chunkSize) > h.MaxSendBytes {
		chunkSize--
	}
	if chunkSize <= 0 {
		return status.Error(codes.ResourceExhausted, "gRPC send limit cannot fit a delivery chunk")
	}
	for len(data) > 0 {
		if err := checkChunk(); err != nil {
			return err
		}
		size := min(chunkSize, len(data))
		if err := stream.Send(&pb.QueueMessage{ChunkData: data[:size], MoreChunks: size < len(data)}); err != nil {
			return err
		}
		data = data[size:]
	}
	return nil
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
		// Ignore manual ACKs and NACKs even while Send is blocked. Only the
		// sender may complete a delivery on an accepted auto-ack stream.
		if init.AutoAck {
			continue
		}

		success := ackReq.Success

		metrics.ConsumerAckTotal.WithLabelValues(
			init.Topic, init.GroupId, boolToStr(success),
		).Inc()

		if len(ackReq.DeliveryTag) == utils.EventKeySize+raft.AttemptTokenSize {
			accepted, err := h.finishLimitedSend(consumerID, init, ackReq.DeliveryTag, success, true)
			if err != nil {
				errCh <- err
				return
			}
			if !accepted {
				h.logger.Debug("ignoring ACK for a stale delivery attempt", zap.String("consumer_id", consumerID))
			}
			continue
		}
		accepted := false
		if success {
			accepted = h.hub.MarkAcknowledgedSent(consumerID, ackReq.DeliveryTag)
		} else {
			accepted = h.hub.AcknowledgeSent(consumerID, ackReq.DeliveryTag)
		}
		if !accepted {
			h.logger.Warn("ignoring ACK for a delivery not sent to this consumer", zap.String("consumer_id", consumerID))
			continue
		}
		if success {
			// Keep this recipient in flight until the batched ACK commits.
			// Persistence failures retain the record while the deleter retries.
			if !h.deleter.TryMarkAcknowledged(ackReq.DeliveryTag, metadata.DeliveryRecipient(init.GroupId, consumerID)) {
				errCh <- status.Error(codes.ResourceExhausted, "durable ACK queue full; retry subscription")
				return
			}
		} else {
			if h.hub.OnNack != nil {
				h.hub.OnNack(ackReq.DeliveryTag)
			}
		}
		// NACK: the key remains in storage; the dispatcher will re-deliver it.
	}
}

func (h *ConsumerHandler) beginLimitedSend(consumerID string, init *pb.SubscribeInit, message *pb.QueueMessage) (*pb.QueueMessage, error) {
	if !h.hub.CanSend(consumerID, message) {
		return nil, nil
	}
	if h.MaxSendBytes > 0 && h.MaxSendBytes < 5 {
		return nil, status.Error(codes.ResourceExhausted, "gRPC send limit cannot fit a delivery chunk")
	}
	if h.RetryLedger == nil {
		return nil, status.Error(codes.Unavailable, "delivery retry ledger unavailable")
	}
	now := time.Now()
	deadline := max(now.UnixMilli()+1, now.Add(h.hub.AttemptTimeout()).UnixMilli())
	token := uuid.New()
	recipient := metadata.DeliveryRecipient(init.GroupId, consumerID)
	attempt, err := h.RetryLedger.ApplyRetry(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_START, NowMs: now.UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: message.DeliveryTag, Recipient: recipient, Token: token[:], DeadlineMs: deadline,
		}},
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reserve delivery attempt: %v", err)
	}
	if attempt == 0 {
		return nil, nil
	}
	tag := make([]byte, 0, utils.EventKeySize+raft.AttemptTokenSize)
	tag = append(tag, message.DeliveryTag...)
	tag = append(tag, token[:]...)
	if !h.hub.BeginRetrySend(consumerID, message, token[:], time.UnixMilli(deadline)) {
		if _, err := h.RetryLedger.ApplyRetry(limitedOutcome(consumerID, init, tag, deliverypb.RetryCommand_CANCEL_RESERVATION)); err != nil {
			h.logger.Warn("failed to release unsent delivery reservation", zap.Error(err))
		}
		return nil, nil
	}
	// Charge the attempt before exposing it to the transport. Once Send can
	// observe a token, expiry must never refund or reuse its attempt number.
	authorized, err := h.RetryLedger.AuthorizeRetrySend(
		limitedOutcome(consumerID, init, tag, deliverypb.RetryCommand_SENT),
		func() bool { return h.hub.CanSend(consumerID, message) },
	)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "authorize delivery send: %v", err)
	}
	if !authorized {
		h.hub.ReleaseRetrySend(consumerID, message.DeliveryTag, token[:])
		return nil, nil
	}
	return &pb.QueueMessage{
		Topic: message.Topic, Payload: message.Payload, DeliveryTag: tag,
		EnqueuedAtUnixMs: message.EnqueuedAtUnixMs, DelayMs: message.DelayMs,
		MaxRetries: message.MaxRetries, Attempt: attempt,
	}, nil
}

func (h *ConsumerHandler) finishLimitedSend(consumerID string, init *pb.SubscribeInit, tag []byte, success, requireTracking bool) (bool, error) {
	key, token := tag[:utils.EventKeySize], tag[utils.EventKeySize:]
	if !h.hub.MarkRetryOutcome(consumerID, key, token) && requireTracking {
		return false, nil
	}
	op := deliverypb.RetryCommand_ACK
	if !success {
		op = deliverypb.RetryCommand_NACK
	}
	if !h.RetryLedger.TryRetryOutcome(limitedOutcome(consumerID, init, tag, op), func() {
		h.hub.ReleaseRetrySend(consumerID, key, token)
		if !success && h.hub.OnNack != nil {
			h.hub.OnNack(key)
		}
	}) {
		return false, status.Error(codes.Unavailable, "delivery outcome admission unavailable; retry subscription")
	}
	return true, nil
}

func limitedOutcome(consumerID string, init *pb.SubscribeInit, tag []byte, op deliverypb.RetryCommand_Operation) *deliverypb.RetryCommand {
	return &deliverypb.RetryCommand{
		Operation: op, NowMs: time.Now().UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: tag[:utils.EventKeySize], Recipient: metadata.DeliveryRecipient(init.GroupId, consumerID),
			Token: tag[utils.EventKeySize:],
		}},
	}
}

func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
