package handlers

import (
	"context"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/dispatcher"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type DeadLetterHandler struct {
	pb.UnimplementedFutureQDeadLetterServer
	Ledger *dispatcher.DeliveryLedger
}

func (h *DeadLetterHandler) ListDeadLetters(ctx context.Context, request *pb.ListDeadLettersRequest) (*pb.ListDeadLettersResponse, error) {
	topic, token, size := request.GetTopic(), request.GetPageToken(), request.GetPageSize()
	if err := raft.ValidateTopicName(topic); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(token) != 0 {
		if err := raft.ValidateDeadLetterID(topic, token); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	switch {
	case size == 0:
		size = 100
	case size > 256:
		return nil, status.Error(codes.InvalidArgument, "page size cannot exceed 256")
	}
	result, err := readDeadLetters(ctx, raft.DeadLetterQuery{Topic: topic, After: token, Limit: int(size)})
	return result.Page, err
}

func (h *DeadLetterHandler) GetDeadLetter(ctx context.Context, request *pb.DeadLetterRequest) (*pb.DeadLetter, error) {
	topic, id := request.GetTopic(), request.GetId()
	if err := raft.ValidateDeadLetterID(topic, id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := readDeadLetters(ctx, raft.DeadLetterQuery{Topic: topic, ID: id})
	if err != nil {
		return nil, err
	}
	if result.Entry == nil {
		return nil, status.Error(codes.NotFound, "dead letter does not exist")
	}
	return result.Entry, nil
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
			return result, status.Errorf(codes.Internal, "read dead letter queue: %v", err)
		}
		return result, nil
	}
	cfg := a.Config()
	ctx, cancel := context.WithTimeout(ctx, cfg.Publish.ProposalTimeout)
	defer cancel()
	value, err := a.NodeHost.SyncRead(ctx, cfg.Cluster.ShardID, query)
	if err != nil {
		return raft.DeadLetterResult{}, status.Errorf(codes.Unavailable, "dead letter queue unavailable: %v", err)
	}
	result, ok := value.(raft.DeadLetterResult)
	if !ok {
		return raft.DeadLetterResult{}, status.Error(codes.Internal, "unexpected dead letter query result")
	}
	return result, nil
}
