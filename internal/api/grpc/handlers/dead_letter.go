package handlers

import (
	"context"
	"errors"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type DeadLetterHandler struct {
	pb.UnimplementedFutureQDeadLetterServer
	Ledger       *dispatcher.DeliveryLedger
	MaxSendBytes int
}

func (h *DeadLetterHandler) ListDeadLetters(ctx context.Context, request *pb.ListDeadLettersRequest) (*pb.ListDeadLettersResponse, error) {
	topic, token, size := request.GetTopic(), request.GetPageToken(), request.GetPageSize()
	if err := raft.ValidateTopicName(topic); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(token) != 0 {
		if _, err := raft.ParseDeadLetterPageToken(topic, token); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	switch {
	case size == 0:
		size = 100
	case size > 256:
		return nil, status.Error(codes.InvalidArgument, "page size cannot exceed 256")
	}
	result, err := readDeadLetters(ctx, raft.DeadLetterQuery{Topic: topic, After: token, Limit: int(size), MaxBytes: h.MaxSendBytes})
	return result.Page, err
}

func (h *DeadLetterHandler) GetDeadLetter(request *pb.DeadLetterRequest, stream grpc.ServerStreamingServer[pb.DeadLetterChunk]) error {
	topic, id := request.GetTopic(), request.GetId()
	if err := raft.ValidateDeadLetterID(topic, id); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := readDeadLetters(stream.Context(), raft.DeadLetterQuery{Topic: topic, ID: id})
	if err != nil {
		return err
	}
	if result.Entry == nil {
		return status.Error(codes.NotFound, "dead letter does not exist")
	}
	data, err := proto.Marshal(result.Entry)
	if err != nil {
		return status.Errorf(codes.Internal, "encode dead letter: %v", err)
	}
	const preferredChunkSize = 32 << 10
	chunkSize := preferredChunkSize
	if h.MaxSendBytes > 0 {
		// A bytes field up to 32 KiB adds at most four protobuf bytes.
		chunkSize = min(chunkSize, h.MaxSendBytes-4)
	}
	if chunkSize <= 0 {
		return status.Error(codes.ResourceExhausted, "gRPC send limit cannot fit a dead letter chunk")
	}
	for len(data) > 0 {
		n := min(len(data), chunkSize)
		if err := stream.Send(&pb.DeadLetterChunk{Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (h *DeadLetterHandler) DeleteDeadLetter(_ context.Context, request *pb.DeadLetterRequest) (*pb.DeleteDeadLetterResponse, error) {
	topic, id := request.GetTopic(), request.GetId()
	if err := raft.ValidateDeadLetterID(topic, id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	_, err := h.Ledger.ApplyRetry(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_DELETE_DEAD_LETTER, Topic: topic, DeadLetterId: id,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "delete dead letter: %v", err)
	}
	return &pb.DeleteDeadLetterResponse{}, nil
}

func readDeadLetters(ctx context.Context, query raft.DeadLetterQuery) (raft.DeadLetterResult, error) {
	a := app.A
	if a.NodeHost == nil {
		result, err := query.Read(a.DB)
		if err != nil {
			if errors.Is(err, raft.ErrDeadLetterPageTooSmall) {
				return result, status.Error(codes.ResourceExhausted, err.Error())
			}
			return result, status.Errorf(codes.Internal, "read dead letter queue: %v", err)
		}
		return result, nil
	}
	cfg := a.Config()
	ctx, cancel := context.WithTimeout(ctx, cfg.Publish.ProposalTimeout)
	defer cancel()
	value, err := a.NodeHost.SyncRead(ctx, cfg.Cluster.ShardID, query)
	if err != nil {
		if errors.Is(err, raft.ErrDeadLetterPageTooSmall) {
			return raft.DeadLetterResult{}, status.Error(codes.ResourceExhausted, err.Error())
		}
		return raft.DeadLetterResult{}, status.Errorf(codes.Unavailable, "dead letter queue unavailable: %v", err)
	}
	result, ok := value.(raft.DeadLetterResult)
	if !ok {
		return raft.DeadLetterResult{}, status.Error(codes.Internal, "unexpected dead letter query result")
	}
	return result, nil
}
