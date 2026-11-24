package handlers

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestTopicLifecycleOverGRPC(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			_, _, producer, reopen := openPublishTest(t, engine)
			listener := bufconn.Listen(1 << 20)
			server := grpc.NewServer()
			pb.RegisterFutureQTopicServer(server, &TopicHandler{})
			pb.RegisterFutureQConsumerServer(server, NewConsumerHandler(zap.NewNop(), nil, nil))
			pb.RegisterFutureQProducerServer(server, producer)
			go server.Serve(listener) //nolint:errcheck
			t.Cleanup(func() { server.Stop(); _ = listener.Close() })
			conn, err := grpc.NewClient("passthrough:///topics",
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
				grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			topics := pb.NewFutureQTopicClient(conn)
			consumer := pb.NewFutureQConsumerClient(conn)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			name := "email.send"
			get := &pb.GetTopicRequest{Name: name}
			subscription := &pb.SubscriptionRequest{Topic: name, GroupId: "mailers"}
			create := &pb.CreateTopicRequest{Name: name, InitialSubscriptions: []*pb.InitialSubscription{{GroupId: "mailers"}}}
			publish := func() error {
				stream, err := pb.NewFutureQProducerClient(conn).PublishStream(ctx)
				require.NoError(t, err)
				require.NoError(t, stream.Send(&pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: name}}}))
				require.NoError(t, stream.CloseSend())
				ack, err := stream.Recv()
				if err == nil {
					require.True(t, ack.GetSuccess())
				}
				return err
			}

			require.Equal(t, codes.NotFound, status.Code(publish()))
			_, err = consumer.CreateSubscription(ctx, subscription)
			require.Equal(t, codes.NotFound, status.Code(err))
			_, err = topics.CreateTopic(ctx, &pb.CreateTopicRequest{Name: name, InitialSubscriptions: []*pb.InitialSubscription{{GroupId: "mailers"}, {}}})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			_, err = topics.GetTopic(ctx, get)
			require.Equal(t, codes.NotFound, status.Code(err))

			topic, err := topics.CreateTopic(ctx, create)
			require.NoError(t, err)
			require.Equal(t, name, topic.GetName())
			require.NoError(t, publish(), "initial subscription accepts jobs before any worker connects")
			first, err := consumer.CreateSubscription(ctx, subscription)
			require.NoError(t, err)
			_, err = topics.CreateTopic(ctx, create)
			require.NoError(t, err)
			repeated, err := consumer.CreateSubscription(ctx, subscription)
			require.NoError(t, err)
			require.Equal(t, first.GetGeneration(), repeated.GetGeneration())

			_, err = consumer.Unsubscribe(ctx, subscription)
			require.NoError(t, err)
			require.Equal(t, codes.FailedPrecondition, status.Code(publish()))
			require.Len(t, publishedKeys(t, app.A.DB), 1)
			reopen()
			topic, err = topics.GetTopic(ctx, get)
			require.NoError(t, err)
			require.Equal(t, name, topic.GetName())

			_, err = topics.CreateTopic(ctx, &pb.CreateTopicRequest{Name: name + ".audit"})
			require.NoError(t, err)
			page, err := topics.ListTopics(ctx, &pb.ListTopicsRequest{PageSize: 1})
			require.NoError(t, err)
			require.Len(t, page.GetTopics(), 1)
			require.Equal(t, name, page.GetTopics()[0].GetName())
			require.NotEmpty(t, page.GetNextPageToken())
			page, err = topics.ListTopics(ctx, &pb.ListTopicsRequest{PageToken: page.GetNextPageToken()})
			require.NoError(t, err)
			require.Len(t, page.GetTopics(), 2)
			require.Equal(t, name+".audit", page.GetTopics()[0].GetName())
			require.Equal(t, "orders", page.GetTopics()[1].GetName())
			require.Empty(t, page.GetNextPageToken())
		})
	}
}
