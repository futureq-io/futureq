package handlers

import (
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPebbleRaftRejectsLeaderAckBeforeProposal(t *testing.T) {
	a, producer, sm, db := openRaftAdmissionTest(t)
	a.Config().Cluster.Raft.LogDB.Backend = config.LogDBPebble
	scans := db.scans.Load()
	ack, err := producer.processBatch(t.Context(), &pb.PublishBatch{
		AckLevel: pb.AckLevel_ACK_LEVEL_LEADER,
		Messages: []*pb.PublishMessage{{Topic: "orders"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "leader acknowledgements require the Tan Raft LogDB")
	require.False(t, ack.GetSuccess())
	require.Equal(t, scans, db.scans.Load())
	require.Empty(t, sm.publishedCommands())
}
