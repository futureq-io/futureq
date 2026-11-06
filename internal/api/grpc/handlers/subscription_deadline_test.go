package handlers

import (
	"context"
	"net"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestRaftSubscriptionRPCsWithoutClientDeadline(t *testing.T) {
	a, _, _, _ := openRaftAdmissionTest(t)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterFutureQConsumerServer(server, NewConsumerHandler(zap.NewNop(), nil, nil))
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient("passthrough:///subscription-deadlines",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewFutureQConsumerClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, hasDeadline := ctx.Deadline()
	require.False(t, hasDeadline)
	for _, identity := range []struct {
		name, group, subscriber string
	}{{"group", "workers", ""}, {"universal", "", "audit"}} {
		t.Run(identity.name, func(t *testing.T) {
			t.Run("create", func(t *testing.T) {
				request := &pb.SubscriptionRequest{Topic: "create", GroupId: identity.group, SubscriberId: identity.subscriber}
				response, err := client.CreateSubscription(ctx, request)
				require.NoError(t, err)
				require.NotEmpty(t, response.Generation)
				record, err := raft.ReadSubscription(a.DB, request.Topic, request.GroupId, request.SubscriberId)
				require.NoError(t, err)
				require.NotNil(t, record)
				require.True(t, record.Active)
				require.Equal(t, response.Generation, record.Recipient)
			})
			t.Run("unsubscribe", func(t *testing.T) {
				request := &pb.SubscriptionRequest{Topic: "remove", GroupId: identity.group, SubscriberId: identity.subscriber}
				seedCtx, seedCancel := context.WithTimeout(ctx, 3*time.Second)
				defer seedCancel()
				_, err := client.CreateSubscription(seedCtx, request)
				require.NoError(t, err)
				_, err = client.Unsubscribe(ctx, request)
				require.NoError(t, err)
				record, err := raft.ReadSubscription(a.DB, request.Topic, request.GroupId, request.SubscriberId)
				require.NoError(t, err)
				require.NotNil(t, record)
				require.False(t, record.Active)
			})
		})
	}
}
