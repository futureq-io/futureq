package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/dispatcher"
	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/futureq-io/stormoar/v4"
	"github.com/futureq-io/stormoar/v4/statemachine"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

var errBatchSave = errors.New("failed to save batch")

// minProtoAckLevel maps the configured Publish.MinAckLevel string to the
// corresponding proto enum value. Proto values increase as durability weakens
// (QUORUM=0 < LEADER=1 < NO_ACK=2), so a batch ack level weaker than the
// configured minimum can be rejected with `ackLevel > minProtoAckLevel`.
// Unknown values fall back to the strictest level (QUORUM).
func minProtoAckLevel(min config.AckLevel) pb.AckLevel {
	switch min {
	case config.Leader:
		return pb.AckLevel_ACK_LEVEL_LEADER
	case config.NoAck:
		return pb.AckLevel_ACK_LEVEL_NO_ACK
	default:
		return pb.AckLevel_ACK_LEVEL_QUORUM
	}
}

// ProducerHandler implements pb.FutureQProducerServer.
type ProducerHandler struct {
	pb.UnimplementedFutureQProducerServer
	logger                  *zap.Logger
	batcher                 *publishBatcher
	pending                 chan struct{}
	maxPublishBytes         int
	standaloneWriter        sync.Mutex
	StandalonePublishLedger *dispatcher.DeliveryLedger
}

// NewProducerHandler initializes publish admission and optional flush workers.
// Clustered publishes write through the event Raft state machine.
func NewProducerHandler(logger *zap.Logger) *ProducerHandler {
	cfg := app.A.Config().Publish
	maxBytes, _ := cfg.BatchMaxBytes.Bytes() // validated at startup
	h := &ProducerHandler{
		logger:          logger.Named("producer"),
		pending:         make(chan struct{}, cfg.MaxPendingRequests),
		maxPublishBytes: int(maxBytes),
	}
	if cfg.BatchMaxRequests > 1 {
		h.batcher = newPublishBatcher(cfg, app.A.NodeHost != nil, h.flushPublishGroup)
	}
	return h
}

// Close stops the optional publish workers and waits for in-flight flushes.
func (ph *ProducerHandler) Close() {
	if ph.batcher != nil {
		ph.batcher.cancel()
		ph.batcher.workers.Wait()
		ph.batcher.flushes.Wait()
	}
}

// PublishStream handles a bidirectional stream where clients send PublishBatch
// frames and receive PublishBatchAck responses.
//
// Each client batch is atomic. The server can group independent client batches
// into a single commit when internal batching is enabled. The ack_level field controls
// whether the broker waits for quorum commit (ACK_LEVEL_QUORUM, default) or
// returns immediately after leader write (ACK_LEVEL_LEADER).
func (ph *ProducerHandler) PublishStream(stream grpc.BidiStreamingServer[pb.PublishBatch, pb.PublishBatchAck]) error {
	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			ph.logger.Error("failed to receive from stream", zap.Error(err))
			return status.Errorf(codes.Internal, "stream read error: %v", err)
		}

		ackResp, streamErr := ph.processBatch(stream.Context(), batch)
		if streamErr != nil {
			return streamErr
		}

		if err := stream.Send(ackResp); err != nil {
			ph.logger.Error("failed to send PublishBatchAck", zap.Error(err))
			return status.Errorf(codes.Internal, "failed to send ack: %v", err)
		}
	}
}

// processBatch handles one PublishBatch frame and returns the corresponding ack.
func (ph *ProducerHandler) processBatch(ctx context.Context, batch *pb.PublishBatch) (*pb.PublishBatchAck, error) {
	if len(batch.Messages) == 0 {
		return &pb.PublishBatchAck{}, nil
	}
	if err := ctx.Err(); err != nil {
		return &pb.PublishBatchAck{}, status.FromContextError(err).Err()
	}
	select {
	case ph.pending <- struct{}{}:
	default:
		return &pb.PublishBatchAck{}, status.Error(codes.ResourceExhausted, "broker publish admission is full")
	}
	transferred := false
	defer func() {
		if !transferred {
			<-ph.pending
		}
	}()
	for _, message := range batch.Messages {
		if err := raft.ValidateTopicName(message.GetTopic()); err != nil {
			return &pb.PublishBatchAck{Success: false}, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	ackLevel := batch.GetAckLevel()
	if ackLevel != pb.AckLevel_ACK_LEVEL_QUORUM && ackLevel != pb.AckLevel_ACK_LEVEL_LEADER && ackLevel != pb.AckLevel_ACK_LEVEL_NO_ACK {
		return &pb.PublishBatchAck{Success: false}, status.Error(codes.InvalidArgument, "unknown ack level")
	}
	topicLabel := batchTopicLabel(batch)
	start := time.Now()

	if minLevel := minProtoAckLevel(app.A.Config().Publish.MinAckLevel); ackLevel > minLevel {
		metrics.PublishRequestsTotal.WithLabelValues(topicLabel, ackLevel.String(), "rejected").Inc()
		return &pb.PublishBatchAck{Success: false}, status.Errorf(codes.InvalidArgument,
			"ack level %s is below the broker minimum %s", ackLevel, app.A.Config().Publish.MinAckLevel)
	}

	var receipts map[string][]byte
	// Quorum publishes decide admission in the ordered event state machine.
	// Standalone and early ACKs must bind destinations before storing/submitting.
	if app.A.NodeHost == nil || ackLevel != pb.AckLevel_ACK_LEVEL_QUORUM {
		var err error
		receipts, err = ph.publishReceipts(ctx, batch)
		if err != nil {
			metrics.PublishRequestsTotal.WithLabelValues(topicLabel, ackLevel.String(), "rejected").Inc()
			return &pb.PublishBatchAck{Success: false}, err
		}
	}

	req, processErr := ph.preparePublish(ctx, batch, start.UnixMilli(), ackLevel, receipts)
	if processErr == nil {
		if ph.batcher == nil {
			ph.flushPublishGroup(ctx, ackLevel, []*queuedPublish{req})
			processErr = <-req.done
		} else {
			req.release = func() { <-ph.pending }
			transferred, processErr = ph.batcher.queue(req, ackLevel)
		}
	}

	if processErr != nil {
		metrics.PublishRequestsTotal.WithLabelValues(topicLabel, ackLevel.String(), "error").Inc()
		return &pb.PublishBatchAck{Success: false}, processErr
	}

	msgCount := len(batch.Messages)
	metrics.PublishRequestsTotal.WithLabelValues(topicLabel, ackLevel.String(), "success").Inc()
	metrics.MessagesPublishedTotal.WithLabelValues(topicLabel, ackLevel.String()).Add(float64(msgCount))
	metrics.PublishBatchSize.WithLabelValues(topicLabel).Observe(float64(msgCount))
	metrics.PublishLatencyMs.WithLabelValues(topicLabel, ackLevel.String()).Observe(float64(time.Since(start).Milliseconds()))

	return &pb.PublishBatchAck{Success: true}, nil
}

// TODO: this is shit. we need to know how many messagers are per topic
func batchTopicLabel(batch *pb.PublishBatch) string {
	if len(batch.Messages) == 0 {
		return ""
	}

	topicSet := make(map[string]struct{})
	result := bytes.Buffer{}
	for i, m := range batch.Messages {
		if _, exists := topicSet[m.Topic]; !exists {
			result.WriteString(m.Topic)
			topicSet[m.Topic] = struct{}{}

			if i < len(batch.Messages)-1 {
				result.WriteString("|")
			}
		}
	}

	return result.String()
}

// marshalMessages is the single marshal loop shared by both write paths.
//
// For each message it computes the routing metadata (bucket, topicHash),
// builds the StoredMessage, marshals it exactly once, and calls fn with the
// result. If fn returns an error the loop stops immediately and the error is
// returned.
//
// Neither path re-serialises the bytes: the Raft path embeds them verbatim in
// the command buffer; the standalone path writes them directly to the Pebble
// batch.
func (ph *ProducerHandler) marshalMessages(
	batch *pb.PublishBatch,
	nowMs int64,
	fn func(data *storagepb.StoredMessage) error,
) error {
	for i, msg := range batch.Messages {
		if msg == nil || msg.Topic == "" {
			return status.Error(codes.InvalidArgument, "publish topic must not be empty")
		}
		if msg.DelayMs < 0 {
			return fmt.Errorf("negative delays are not allowed! delay: %d, topic: %s message_idx: %d", msg.DelayMs, msg.Topic, i)
		}

		if msg.TtlMs < 0 {
			return fmt.Errorf("negative ttls are not allowed! ttl: %d, topic: %s message_idx: %d", msg.TtlMs, msg.Topic, i)
		}

		stored := &storagepb.StoredMessage{
			Topic:            msg.Topic,
			Payload:          msg.Payload,
			EnqueuedAtUnixMs: nowMs,
			DelayMs:          msg.DelayMs,
			TtlMs:            msg.TtlMs,
			Indexes:          msg.Indexes,
		}

		if err := fn(stored); err != nil {
			return err
		}
	}

	return nil
}

func checkPublishLeader() error {
	cfg := app.A.Config()
	id, _, valid, err := app.A.NodeHost.GetLeaderID(cfg.Cluster.ShardID)
	if err != nil || !valid || id != cfg.Cluster.NodeID {
		return errors.New("node is not the cluster leader")
	}
	return nil
}

func (ph *ProducerHandler) buildStoreItems(batch *pb.PublishBatch, nowMs int64, receipts map[string][]byte, clustered bool) ([]raft.StoreBatchItem, error) {
	cfg := app.A.Config()
	items := make([]raft.StoreBatchItem, 0, len(batch.Messages))
	if err := ph.marshalMessages(batch, nowMs, func(data *storagepb.StoredMessage) error {
		dataBytes, err := proto.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}

		bucket := utils.CalculateBucket(data.EnqueuedAtUnixMs+data.DelayMs, cfg.Delivery.TimeBucket)
		var slot int
		var id uint64
		if clustered {
			bucket, slot = utils.ChooseStorageBucket(bucket, cfg.Storage.VirtualShards)
			id = app.A.Repositories.Events.NextID()
		}
		raftItem := raft.StoreBatchItem{
			ID:        id,
			Bucket:    bucket,
			ShardSlot: slot,
			TopicHash: utils.TopicHash(data.Topic),
			Msg:       dataBytes,
			Receipt:   receipts[data.Topic],
		}

		for _, idx := range data.GetIndexes() {
			idxBytes, err := proto.Marshal(idx)
			if err != nil {
				return fmt.Errorf("failed to marshal index: %w", err)
			}
			if (clustered || cfg.Storage.Engine == "bolt") && (len(idxBytes) == 0 || len(idxBytes) > bolt.MaxKeySize) {
				return status.Error(codes.InvalidArgument, "publish index exceeds Bolt key limits or is empty")
			}

			raftItem.Indexes = append(raftItem.Indexes, idxBytes)
		}

		items = append(items, raftItem)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to build store batch: %w", err)
	}
	return items, nil
}

func (ph *ProducerHandler) proposePublish(ctx context.Context, ackLevel pb.AckLevel, cmdBytes []byte) (statemachine.Result, error) {
	cfg := app.A.Config()
	shardID := cfg.Cluster.ShardID
	start := time.Now()
	proposalTimeout := cfg.Publish.ProposalTimeout
	var proposeErr error
	var result statemachine.Result

	switch ackLevel {
	case pb.AckLevel_ACK_LEVEL_NO_ACK:
		session := app.A.NodeHost.GetNoOPSession(shardID)
		_, proposeErr = app.A.NodeHost.Propose(session, cmdBytes, proposalTimeout)
	case pb.AckLevel_ACK_LEVEL_LEADER:
		if app.A.LeaderTracker == nil {
			proposeErr = errors.New("leader-persist tracker is not initialised (raft disabled?)")
			break
		}

		persistedCh, cancelWait := app.A.LeaderTracker.Register(app.A.LeaderTracker.Hash(cmdBytes))
		defer cancelWait()

		session := app.A.NodeHost.GetNoOPSession(shardID)
		rs, err := app.A.NodeHost.Propose(session, cmdBytes, proposalTimeout)
		if err != nil {
			proposeErr = err
			break
		}
		defer rs.Release()

		propCtx, cancel := context.WithTimeout(ctx, proposalTimeout)
		defer cancel()

		proposeErr = waitForLeaderAck(propCtx, persistedCh, rs.AppliedC())
		if proposeErr == nil {
			ph.logger.Debug("leader has persisted and acked")
		}
	default:
		propCtx, cancel := context.WithTimeout(ctx, proposalTimeout)
		session := app.A.NodeHost.GetNoOPSession(shardID)
		result, proposeErr = app.A.NodeHost.SyncPropose(propCtx, session, cmdBytes)
		cancel()
	}

	metrics.RaftProposeDurationMs.WithLabelValues(ackLevel.String()).Observe(
		float64(time.Since(start).Milliseconds()),
	)
	return result, proposeErr
}

func publishProposalError(proposeErr error) error {
	if proposeErr != nil {
		var rejection *raft.PublishRejectedError
		if errors.As(proposeErr, &rejection) {
			return publishRejectionStatus(rejection)
		}
		return fmt.Errorf("failed to do raft proposal: %w", proposeErr)
	}

	return nil
}

func publishRejectionStatus(rejection *raft.PublishRejectedError) error {
	code := codes.FailedPrecondition
	switch rejection.Reason {
	case raft.PublishTopicNotFound:
		code = codes.NotFound
	case raft.PublishTooManyDestinations, raft.PublishStorageBudgetExceeded:
		code = codes.ResourceExhausted
	}
	return status.Error(code, rejection.Error())
}

// waitForLeaderAck waits for local persistence or successful application, which
// also proves persistence. It does not require quorum application to finish.
func waitForLeaderAck(ctx context.Context, persisted <-chan struct{}, applied <-chan dragonboat.RequestResult) error {
	var waitErr error
	select {
	case <-persisted:
		return nil
	case result := <-applied:
		if result.Completed() {
			return nil
		}
		waitErr = errors.New("leader-ack proposal did not complete")
	case <-ctx.Done():
		waitErr = fmt.Errorf("leader-ack wait cancelled: %w", ctx.Err())
	}
	// select chooses randomly when persistence and a failure are both ready.
	// Once the local log is persisted the LEADER guarantee is satisfied, even
	// if replication/application later times out or the caller is cancelled.
	select {
	case <-persisted:
		return nil
	default:
		return waitErr
	}
}

func (ph *ProducerHandler) commitStandalonePublish(batch storage.Batch, published map[string]*raft.DeliveryState) error {
	if ph.StandalonePublishLedger != nil {
		return ph.StandalonePublishLedger.CommitStandalonePublish(batch, published)
	}
	return batch.Commit(storage.Sync)
}

// publishReceipts binds standalone and early-ACK destinations before acceptance.
// Only the early-ACK Raft path performs this admission quorum read.
func (ph *ProducerHandler) publishReceipts(ctx context.Context, batch *pb.PublishBatch) (map[string][]byte, error) {
	a := app.A
	query := make(raft.PublishRecipientsQuery, len(batch.Messages))
	for i, msg := range batch.Messages {
		query[i] = msg.Topic
	}
	var admission raft.PublishRecipientsResult
	if a.NodeHost == nil {
		admission = query.Read(a.DB)
	} else {
		callCtx, cancel := context.WithTimeout(ctx, a.Config().Publish.ProposalTimeout)
		defer cancel()
		value, err := a.NodeHost.SyncRead(callCtx, a.Config().Cluster.ShardID, query)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "subscription registry unavailable: %v", err)
		}
		var ok bool
		admission, ok = value.(raft.PublishRecipientsResult)
		if !ok {
			return nil, status.Error(codes.Internal, "unexpected publish admission result")
		}
	}
	if admission.Err != nil {
		var rejection *raft.PublishRejectedError
		if errors.As(admission.Err, &rejection) {
			return nil, publishRejectionStatus(rejection)
		}
		return nil, status.Error(codes.Internal, admission.Err.Error())
	}
	result := make(map[string][]byte, len(admission.Recipients))
	for topic, names := range admission.Recipients {
		data, err := raft.MarshalPublishReceipt(0, names)
		if err != nil {
			return nil, status.Errorf(codes.ResourceExhausted, "publish destinations: %v", err)
		}
		result[topic] = data
	}
	return result, nil
}
