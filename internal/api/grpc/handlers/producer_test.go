package handlers

import (
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/futureq-io/futureq/internal/config"
	pb "github.com/futureq-io/protocol/proto/go"
)

type ProducerSuite struct {
	suite.Suite
}

func TestProducerSuite(t *testing.T) {
	suite.Run(t, new(ProducerSuite))
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
