package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/futureq-io/futureq/internal/config"
	pb "github.com/futureq-io/protocol/proto/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type ProducerSuite struct {
	suite.Suite
}

func TestProducerSuite(t *testing.T) {
	suite.Run(t, new(ProducerSuite))
}

func (s *ProducerSuite) TestPublishRejectsBatchWhoseDeliveryFrameExceedsSendLimit() {
	const sendLimit = 100 << 10
	a, _, producer, _ := openPublishTest(s.T(), "pebble")
	a.Config().API.GRPC.MaxSendMessageSize = "100KiB"
	ctx := context.Background()
	s.Require().NoError(createTestInterest(ctx, "orders", "workers"))
	now := time.Now().UnixMilli()
	message := &pb.PublishMessage{Topic: "orders", MaxRetries: new(uint32(0))}
	// Place the message one byte outside the actual delivery frame budget.
	message.Payload = bytes.Repeat([]byte{'x'}, sendLimit)
	for !deliveryFrameFits(sendLimit, message.Topic, message.Payload, now, 0, message.MaxRetries) {
		message.Payload = message.Payload[:len(message.Payload)-1]
	}
	exact := proto.CloneOf(message)
	message.Payload = append(message.Payload, 'x')
	batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}, message}}
	s.Less(proto.Size(batch), sendLimit, "the publish request fits the receive limit")
	ack, err := producer.processBatch(ctx, batch)
	s.Equal(codes.ResourceExhausted, status.Code(err))
	s.False(ack.GetSuccess())
	s.Empty(publishedKeys(s.T(), a.DB), "the entire batch must be rejected")

	ack, err = producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{exact}})
	s.Require().NoError(err)
	s.True(ack.GetSuccess())
	s.Len(publishedKeys(s.T(), a.DB), 1)
}

// TestAckLevelDurabilityOrdering asserts the load-bearing invariant behind the
// MinAckLevel floor check: proto enum values increase as durability weakens
// (QUORUM < LEADER < NO_ACK), so `ackLevel > minLevel` rejects too-weak acks.
//
// If anyone renumbers the AckLevel enum (e.g. regenerating from an edited
// producer.proto), this test fails and reminds them the floor check depends
// on the ordering.
func (s *ProducerSuite) TestAckLevelDurabilityOrdering() {
	require := s.Require()

	require.Less(pb.AckLevel_ACK_LEVEL_QUORUM, pb.AckLevel_ACK_LEVEL_LEADER,
		"QUORUM must be ordered before LEADER")
	require.Less(pb.AckLevel_ACK_LEVEL_LEADER, pb.AckLevel_ACK_LEVEL_NO_ACK,
		"LEADER must be ordered before NO_ACK")
}

// TestMinProtoAckLevel verifies that the string-based config floor maps to the
// correct proto enum value, and that unknown values fall back to the
// strictest level (QUORUM) rather than silently allowing weak acks.
func (s *ProducerSuite) TestMinProtoAckLevel() {
	require := s.Require()

	cases := []struct {
		name     string
		min      config.AckLevel
		expected pb.AckLevel
	}{
		{"Quorum maps to QUORUM", config.Quorum, pb.AckLevel_ACK_LEVEL_QUORUM},
		{"Leader maps to LEADER", config.Leader, pb.AckLevel_ACK_LEVEL_LEADER},
		{"NoAck maps to NO_ACK", config.NoAck, pb.AckLevel_ACK_LEVEL_NO_ACK},
		{"unknown falls back to QUORUM", config.AckLevel("bogus"), pb.AckLevel_ACK_LEVEL_QUORUM},
		{"empty falls back to QUORUM", config.AckLevel(""), pb.AckLevel_ACK_LEVEL_QUORUM},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			require.Equal(tc.expected, minProtoAckLevel(tc.min))
		})
	}
}

// TestMinAckLevelFloorMatrix exercises the floor logic end-to-end across the
// (config floor) × (batch ack level) matrix, mirroring the predicate used in
// processBatch. It does not touch Dragonboat — only the pure comparison.
func (s *ProducerSuite) TestMinAckLevelFloorMatrix() {
	require := s.Require()

	levels := []pb.AckLevel{
		pb.AckLevel_ACK_LEVEL_QUORUM,
		pb.AckLevel_ACK_LEVEL_LEADER,
		pb.AckLevel_ACK_LEVEL_NO_ACK,
	}

	// For each floor, which batch levels should be REJECTED.
	rejections := map[config.AckLevel]map[pb.AckLevel]bool{
		config.Quorum: {
			pb.AckLevel_ACK_LEVEL_QUORUM: false,
			pb.AckLevel_ACK_LEVEL_LEADER: true,
			pb.AckLevel_ACK_LEVEL_NO_ACK: true,
		},
		config.Leader: {
			pb.AckLevel_ACK_LEVEL_QUORUM: false,
			pb.AckLevel_ACK_LEVEL_LEADER: false,
			pb.AckLevel_ACK_LEVEL_NO_ACK: true,
		},
		config.NoAck: {
			pb.AckLevel_ACK_LEVEL_QUORUM: false,
			pb.AckLevel_ACK_LEVEL_LEADER: false,
			pb.AckLevel_ACK_LEVEL_NO_ACK: false,
		},
	}

	for floor, levelOutcomes := range rejections {
		min := minProtoAckLevel(floor)
		for _, level := range levels {
			wantRejected := levelOutcomes[level]
			gotRejected := level > min
			require.Equal(wantRejected, gotRejected,
				"floor=%v level=%v: rejected mismatch", floor, level)
		}
	}
}
