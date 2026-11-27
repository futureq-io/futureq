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
			var frames *deliveryFrames
			var err error
			if msg.MaxRetries != nil {
				frames, err = h.beginLimitedSend(ctx, consumerID, init, msg)
			} else if h.hub.BeginSend(consumerID, msg) {
				frames, err = h.prepareDeliveryFrames(msg)
			}
			if err != nil {
				errCh <- err
				return
			}
			if frames == nil {
				h.hub.RejectQueuedAttempt(consumerID, msg)
				continue // expired, ACKed, or fenced buffered delivery
			}
			// Observe before entering transport, where consumer backpressure
			// can block the send.
			metrics.DeliverySendLatenessMs.WithLabelValues(msg.Topic).Observe(float64(time.Now().UnixNano())/1e6 - float64(msg.EnqueuedAtUnixMs+msg.DelayMs))
			if err := h.sendDelivery(ctx, stream, consumerID, recipient, msg, frames); err != nil {
				if frames.autoACK != nil {
					frames.autoACK.Finish(false)
				} else if msg.MaxRetries != nil {
					h.RetryLedger.AbandonManualRetryOutcome(frames.DeliveryTag)
				}
				if errors.Is(err, errRetryTransportSkipped) {
					h.hub.ReleaseRetrySend(consumerID, msg.DeliveryTag, frames.DeliveryTag[utils.EventKeySize:])
					continue
				}
				errCh <- err
				return
			}
			// Queue the auto-ACK before metrics or hub bookkeeping can delay it.
			autoACKAccepted := frames.autoACK == nil || frames.autoACK.Finish(true)
			metrics.DeliveryGRPCSendsTotal.WithLabelValues(msg.Topic).Inc()
			if !autoACKAccepted {
				errCh <- status.Error(codes.Unavailable, "automatic delivery ACK no longer active")
				return
			}
			if frames.autoACK != nil {
				h.hub.MarkRetryOutcome(consumerID, msg.DeliveryTag, frames.DeliveryTag[utils.EventKeySize:])
				continue
			}
			if init.AutoAck {
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

type deliveryFrames struct {
	*pb.QueueMessage
	encoded      []byte
	chunkSize    int
	firstChecked bool
	autoACK      *dispatcher.RetryAutoACK
}

func (h *ConsumerHandler) prepareDeliveryFrames(message *pb.QueueMessage) (*deliveryFrames, error) {
	if h.MaxSendBytes <= 0 || proto.Size(message) <= h.MaxSendBytes {
		return &deliveryFrames{QueueMessage: message}, nil
	}
	data, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	// Reserve the chunk field tag and the "more chunks" boolean. The bytes
	// field's length prefix grows with each varint boundary.
	overhead := protowire.SizeTag(8) + protowire.SizeTag(9) + 1
	chunkSize := min(32<<10, h.MaxSendBytes-overhead-1)
	for chunkSize > 0 && overhead+protowire.SizeBytes(chunkSize) > h.MaxSendBytes {
		chunkSize--
	}
	if chunkSize <= 0 {
		return nil, status.Error(codes.ResourceExhausted, "gRPC send limit cannot fit a delivery chunk")
	}
	return &deliveryFrames{QueueMessage: message, encoded: data, chunkSize: chunkSize}, nil
}

var errRetryTransportSkipped = errors.New("delivery attempt ended before the first frame")

func (h *ConsumerHandler) sendDelivery(ctx context.Context, stream grpc.BidiStreamingServer[pb.ConsumerFrame, pb.QueueMessage], consumerID, recipient string, queued *pb.QueueMessage, frames *deliveryFrames) error {
	first := true
	canSend := func() bool { return h.hub.CanSend(consumerID, queued) && ctx.Err() == nil }
	send := func(frame *pb.QueueMessage) error {
		if first && frames.MaxRetries != nil && !h.RetryLedger.BeginRetryTransport(frames.DeliveryTag, canSend) {
			return errRetryTransportSkipped
		}
		first = false
		return stream.Send(frame)
	}
	if frames.encoded == nil {
		return send(frames.QueueMessage)
	}
	// The first check for a limited delivery ran under refund protection.
	// Every subsequent frame must still verify that the attempt is active.
	check := !frames.firstChecked
	data := frames.encoded
	for len(data) > 0 {
		if check {
			if err := h.checkDeliveryChunk(consumerID, recipient, queued, frames.QueueMessage); err != nil {
				return err
			}
		}
		check = true
		size := min(frames.chunkSize, len(data))
		if err := send(&pb.QueueMessage{ChunkData: data[:size], MoreChunks: size < len(data)}); err != nil {
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
			accepted, err := h.finishLimitedSend(consumerID, init, ackReq.DeliveryTag, success)
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

func (h *ConsumerHandler) beginLimitedSend(ctx context.Context, consumerID string, init *pb.SubscribeInit, message *pb.QueueMessage) (*deliveryFrames, error) {
	if !h.hub.CanSend(consumerID, message) {
		return nil, nil
	}
	if h.RetryLedger == nil {
		return nil, status.Error(codes.Unavailable, "delivery retry ledger unavailable")
	}
	now := time.Now()
	ackWindow := h.hub.AttemptTimeout()
	proposalTimeout := app.A.Config().Publish.ProposalTimeout
	// Allow START and SENT their own proposal budgets while keeping an
	// abandoned reservation recoverable.
	reservationDeadline := now.Add(proposalTimeout).Add(proposalTimeout).Add(ackWindow)
	token := uuid.New()
	tag := make([]byte, 0, utils.EventKeySize+raft.AttemptTokenSize)
	tag = append(tag, message.DeliveryTag...)
	tag = append(tag, token[:]...)
	var autoACK *dispatcher.RetryAutoACK
	if init.AutoAck {
		autoACK = h.RetryLedger.TryReserveRetryAutoACK(limitedOutcome(consumerID, init, tag, deliverypb.RetryCommand_ACK), func() {
			h.hub.ReleaseRetrySend(consumerID, tag[:utils.EventKeySize], token[:])
		})
		if autoACK == nil {
			return nil, nil
		}
	}
	if !h.hub.BeginRetrySend(consumerID, message, token[:], reservationDeadline) {
		if autoACK != nil {
			autoACK.Finish(false)
		}
		return nil, nil
	}
	recipient := metadata.DeliveryRecipient(init.GroupId, consumerID)
	start := &deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_START, NowMs: now.UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: message.DeliveryTag, Recipient: recipient, Token: token[:],
			DeadlineMs: max(now.UnixMilli()+1, reservationDeadline.UnixMilli()),
		}},
	}
	var frames *deliveryFrames
	prepare := func(attempt uint64) (bool, error) {
		if !h.hub.CanSend(consumerID, message) {
			return false, nil
		}
		prepared, err := h.prepareDeliveryFrames(&pb.QueueMessage{
			Topic: message.Topic, Payload: message.Payload, DeliveryTag: tag,
			EnqueuedAtUnixMs: message.EnqueuedAtUnixMs, DelayMs: message.DelayMs,
			MaxRetries: message.MaxRetries, Attempt: attempt,
		})
		frames = prepared
		return err == nil, err
	}
	checkTransport := func() (time.Time, error) {
		if frames.encoded != nil {
			if err := h.checkDeliveryChunk(consumerID, recipient, message, frames.QueueMessage); err != nil {
				return time.Time{}, err
			}
			frames.firstChecked = true
		}
		deadline := time.Now().Add(ackWindow)
		if !h.hub.ActivateRetrySend(consumerID, message, token[:], deadline) {
			return time.Time{}, nil
		}
		return deadline, nil
	}
	var authorized bool
	var err error
	if init.AutoAck {
		authorized, err = h.RetryLedger.AuthorizeRetrySend(ctx, start, ackWindow, prepare, checkTransport)
	} else {
		authorized, err = h.RetryLedger.AuthorizeManualRetrySend(ctx, start, ackWindow, prepare, checkTransport)
	}
	if !authorized {
		if autoACK != nil {
			autoACK.Finish(false)
		}
		h.hub.ReleaseRetrySend(consumerID, message.DeliveryTag, token[:])
		if err != nil {
			if _, ok := status.FromError(err); ok {
				return nil, err
			}
			return nil, status.Errorf(codes.Unavailable, "authorize delivery send: %v", err)
		}
		return nil, nil
	}
	frames.autoACK = autoACK
	return frames, nil
}

func (h *ConsumerHandler) finishLimitedSend(consumerID string, init *pb.SubscribeInit, tag []byte, success bool) (bool, error) {
	key, token := tag[:utils.EventKeySize], tag[utils.EventKeySize:]
	op := deliverypb.RetryCommand_ACK
	if !success {
		op = deliverypb.RetryCommand_NACK
	}
	valid, accepted := h.RetryLedger.TryManualRetryOutcome(limitedOutcome(consumerID, init, tag, op), func() bool {
		return h.hub.MarkRetryOutcome(consumerID, key, token)
	}, func() {
		h.hub.ReleaseRetrySend(consumerID, key, token)
		if !success && h.hub.OnNack != nil {
			h.hub.OnNack(key)
		}
	})
	if !valid {
		return false, nil
	}
	if !accepted {
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
