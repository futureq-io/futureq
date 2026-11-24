package handlers

import (
	"context"

	"github.com/futureq-io/futureq/internal/app"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/subscriptions"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type TopicHandler struct {
	pb.UnimplementedFutureQTopicServer
}

func (h *TopicHandler) CreateTopic(ctx context.Context, request *pb.CreateTopicRequest) (*pb.Topic, error) {
	name := request.GetName()
	if err := raft.ValidateTopicName(name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	initial := request.GetInitialSubscriptions()
	if len(initial) > raft.MaxInitialSubscriptions {
		return nil, status.Errorf(codes.InvalidArgument, "at most %d initial subscriptions are allowed", raft.MaxInitialSubscriptions)
	}
	mutations := make([]*deliverypb.SubscriptionMutation, 0, len(initial))
	for _, subscription := range initial {
		group, subscriber := subscription.GetGroupId(), subscription.GetSubscriberId()
		if _, err := raft.SubscriptionKey(name, group, subscriber); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		mutations = append(mutations, subscriptions.Mutation(name, group, subscriber))
	}
	if err := app.A.Subscriptions.CreateTopic(ctx, name, mutations); err != nil {
		return nil, status.Errorf(codes.Unavailable, "create topic: %v", err)
	}
	return &pb.Topic{Name: name}, nil
}

func (h *TopicHandler) GetTopic(ctx context.Context, request *pb.GetTopicRequest) (*pb.Topic, error) {
	name := request.GetName()
	if err := raft.ValidateTopicName(name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := readTopicCatalog(ctx, raft.TopicCatalogQuery{Name: name})
	if err != nil {
		return nil, err
	}
	if !result.Exists {
		return nil, status.Errorf(codes.NotFound, "topic %q does not exist", name)
	}
	return &pb.Topic{Name: name}, nil
}

func (h *TopicHandler) ListTopics(ctx context.Context, request *pb.ListTopicsRequest) (*pb.ListTopicsResponse, error) {
	size := request.GetPageSize()
	switch {
	case size == 0:
		size = 100
	case size > 256:
		return nil, status.Error(codes.InvalidArgument, "page size cannot exceed 256")
	}
	if token := request.GetPageToken(); token != "" {
		if err := raft.ValidateTopicName(token); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid page token: %v", err)
		}
	}
	result, err := readTopicCatalog(ctx, raft.TopicCatalogQuery{After: request.GetPageToken(), Limit: int(size)})
	if err != nil {
		return nil, err
	}
	topics := make([]*pb.Topic, 0, len(result.Names))
	for _, name := range result.Names {
		topics = append(topics, &pb.Topic{Name: name})
	}
	return &pb.ListTopicsResponse{Topics: topics, NextPageToken: result.Next}, nil
}

func readTopicCatalog(ctx context.Context, query raft.TopicCatalogQuery) (raft.TopicCatalogResult, error) {
	a := app.A
	if a.NodeHost == nil {
		result, err := query.Read(a.DB)
		if err != nil {
			return result, status.Errorf(codes.Internal, "read topic catalog: %v", err)
		}
		return result, nil
	}
	cfg := a.Config()
	ctx, cancel := context.WithTimeout(ctx, cfg.Publish.ProposalTimeout)
	defer cancel()
	value, err := a.NodeHost.SyncRead(ctx, cfg.Cluster.ShardID, query)
	if err != nil {
		return raft.TopicCatalogResult{}, status.Errorf(codes.Unavailable, "topic registry unavailable: %v", err)
	}
	result, ok := value.(raft.TopicCatalogResult)
	if !ok {
		return raft.TopicCatalogResult{}, status.Error(codes.Internal, "unexpected topic catalog result")
	}
	return result, nil
}
