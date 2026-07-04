package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/internal/raft"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/gogo/protobuf/proto"
)

var errBatchSave = errors.New("failed to save batch")

// ProducerHandler implements pb.FutureQProducerServer.
type ProducerHandler struct {
	pb.UnimplementedFutureQProducerServer
	logger         *zap.Logger
	timeBucketSize time.Duration
}

// NewProducerHandler returns an initialised ProducerHandler.
// In Raft mode the handler never writes directly to Pebble; all writes go
// through SyncPropose → state machine → Pebble.
func NewProducerHandler(logger *zap.Logger) *ProducerHandler {
	return &ProducerHandler{
		logger:         logger.Named("producer"),
		timeBucketSize: app.A.Config().Storage.TimeBucketSize,
	}
}

// PublishStream handles a bidirectional stream where clients send PublishBatch
// frames and receive PublishBatchAck responses.
//
// Each batch is written atomically as a single Raft log entry (in Raft mode)
// or a single Pebble batch (standalone mode). The ack_level field controls
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

	ackLevel := batch.GetAckLevel()

	nowMs := time.Now().UnixMilli()

	if app.A.NodeHost != nil {
		if err := ph.processRaftBatch(ctx, batch, nowMs, ackLevel); err != nil {
			return &pb.PublishBatchAck{Success: false}, err
		}
	} else {
		if err := ph.processStandaloneBatch(batch, nowMs); err != nil {
			return &pb.PublishBatchAck{Success: false}, err
		}
	}

	metrics.PublishBatchSize.WithLabelValues("").Observe(float64(len(batch.Messages)))

	return &pb.PublishBatchAck{Success: true}, nil
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
	for _, msg := range batch.Messages {
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

func (ph *ProducerHandler) processRaftBatch(
	ctx context.Context,
	batch *pb.PublishBatch,
	nowMs int64,
	ackLevel pb.AckLevel,
) error {
	shardID := app.A.Config().Raft.ClusterID

	// Only the leader may propose.
	leaderID, _, valid, errL := app.A.NodeHost.GetLeaderID(shardID)
	if errL != nil || !valid || leaderID != app.A.Config().Raft.NodeID {
		return errors.New("node is not the cluster leader")
	}

	items := make([]raft.StoreBatchItem, 0, len(batch.Messages))
	if err := ph.marshalMessages(batch, nowMs, func(data *storagepb.StoredMessage) error {
		dataBytes, err := proto.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}

		raftItem := raft.StoreBatchItem{
			Bucket:    utils.CalculateBucket(data.EnqueuedAtUnixMs+data.DelayMs, app.A.Config().Storage.TimeBucketSize),
			TopicHash: utils.TopicHash(data.Topic),
			Msg:       dataBytes,
		}

		for _, idx := range data.GetIndexes() {
			idxBytes, err := proto.Marshal(idx)
			if err != nil {
				return fmt.Errorf("failed to marshal index: %w", err)
			}

			raftItem.Indexes = append(raftItem.Indexes, idxBytes)
		}

		items = append(items, raftItem)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to build store batch: %w", err)
	}

	cmdBytes, err := raft.MarshalStoreBatchCmd(items)
	if err != nil {
		return fmt.Errorf("failed to marshal StoreBatchCmd: %w", err)
	}

	start := time.Now()
	var proposeErr error

	switch ackLevel {
	case pb.AckLevel_ACK_LEVEL_NO_ACK:
		session := app.A.NodeHost.GetNoOPSession(shardID)
		_, proposeErr = app.A.NodeHost.Propose(session, cmdBytes, 5*time.Second)
	default:
		propCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		session := app.A.NodeHost.GetNoOPSession(shardID)
		_, proposeErr = app.A.NodeHost.SyncPropose(propCtx, session, cmdBytes)
		cancel()
	}

	metrics.RaftProposeDurationMs.WithLabelValues(ackLevel.String()).Observe(
		float64(time.Since(start).Milliseconds()),
	)

	if proposeErr != nil {
		return fmt.Errorf("failed to do raft proposal: %w", proposeErr)
	}

	return nil
}

// processStandaloneBatch writes the batch directly to Pebble (non-Raft mode).
func (ph *ProducerHandler) processStandaloneBatch(batch *pb.PublishBatch, nowMs int64) error {
	b := app.A.DB.NewBatch()
	defer func() { _ = b.Close() }()

	if err := ph.marshalMessages(batch, nowMs, func(data *storagepb.StoredMessage) error {
		_, err := app.A.Repositories.Events.StoreWithBatch(b, data)
		return err
	}); err != nil {
		return err
	}

	if err := b.Commit(storage.Sync); err != nil {
		ph.logger.Error("failed to commit standalone batch", zap.Error(err))
		return errBatchSave
	}

	return nil
}
