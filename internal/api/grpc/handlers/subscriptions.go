package handlers

import (
	"context"
	"errors"

	"github.com/futureq-io/futureq/internal/app"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/subscriptions"
	pb "github.com/futureq-io/protocol/proto/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validateSubscription(request *pb.SubscriptionRequest) error {
	if request == nil {
		return status.Error(codes.InvalidArgument, "subscription is required")
	}
	if _, err := raft.SubscriptionKey(request.Topic, request.GroupId, request.SubscriberId); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}
func subscriptionError(err error) error {
	if errors.Is(err, subscriptions.ErrNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Errorf(codes.Unavailable, "subscription operation failed: %v", err)
}
func (h *ConsumerHandler) CreateSubscription(ctx context.Context, request *pb.SubscriptionRequest) (*pb.SubscriptionResponse, error) {
	if err := validateSubscription(request); err != nil {
		return nil, err
	}
	record, err := app.A.Subscriptions.Create(ctx, request.Topic, request.GroupId, request.SubscriberId)
	if err != nil {
		return nil, subscriptionError(err)
	}
	return &pb.SubscriptionResponse{Topic: record.Topic, GroupId: record.GroupId, SubscriberId: record.SubscriberId, Generation: record.Recipient}, nil
}
func (h *ConsumerHandler) Unsubscribe(ctx context.Context, request *pb.SubscriptionRequest) (*pb.UnsubscribeResponse, error) {
	if err := validateSubscription(request); err != nil {
		return nil, err
	}
	if err := app.A.Subscriptions.Unsubscribe(ctx, request.Topic, request.GroupId, request.SubscriberId); err != nil {
		return nil, subscriptionError(err)
	}
	return &pb.UnsubscribeResponse{}, nil
}
