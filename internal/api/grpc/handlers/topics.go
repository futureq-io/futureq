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
	if err := topicReadBarrier(ctx); err != nil {
		return nil, err
	}
	exists, err := raft.TopicExists(app.A.DB, name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read topic: %v", err)
	}
	if !exists {
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
	if err := topicReadBarrier(ctx); err != nil {
		return nil, err
	}
	names, next, err := raft.ListTopics(app.A.DB, request.GetPageToken(), int(size))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list topics: %v", err)
	}
	topics := make([]*pb.Topic, 0, len(names))
	for _, name := range names {
		topics = append(topics, &pb.Topic{Name: name})
	}
	return &pb.ListTopicsResponse{Topics: topics, NextPageToken: next}, nil
}

func topicReadBarrier(ctx context.Context) error {
	a := app.A
	if a.NodeHost == nil {
		return nil
	}
	cfg := a.Config()
	ctx, cancel := context.WithTimeout(ctx, cfg.Publish.ProposalTimeout)
	defer cancel()
	if _, err := a.NodeHost.SyncRead(ctx, cfg.Cluster.ShardID, nil); err != nil {
		return status.Errorf(codes.Unavailable, "topic registry unavailable: %v", err)
	}
	return nil
}
