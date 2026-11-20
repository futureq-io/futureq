package utils

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

type UtilsSuite struct {
	suite.Suite
}

func TestUtilsSuite(t *testing.T) {
	suite.Run(t, new(UtilsSuite))
}

// ─── TopicHash ─────────────────────────────────────────────────────────────

func (s *UtilsSuite) TestTopicHash_Deterministic() {
	require := s.Require()

	h1 := TopicHash("orders.created")
	h2 := TopicHash("orders.created")
	require.Equal(h1, h2, "same topic should produce the same hash")
}

func (s *UtilsSuite) TestTopicHash_Distinct() {
	require := s.Require()

	h1 := TopicHash("orders.created")
	h2 := TopicHash("orders.cancelled")
	require.NotEqual(h1, h2, "different topics should produce different hashes")
}

func (s *UtilsSuite) TestTopicHash_EmptyString() {
	require := s.Require()

	h := TopicHash("")
	require.NotZero(h, "empty string should still produce a valid hash")
}

// ─── CalculateBucket ───────────────────────────────────────────────────────

func (s *UtilsSuite) TestCalculateBucket_ExactMultiple() {
	require := s.Require()

	// 17000 / 1000 = 17
	require.Equal(uint64(17), CalculateBucket(17000, 1*time.Second))
}

func (s *UtilsSuite) TestCalculateBucket_FloorDivision() {
	require := s.Require()

	// 17999 / 1000 = 17 (floor)
	require.Equal(uint64(17), CalculateBucket(17999, 1*time.Second))
	// 18000 / 1000 = 18
	require.Equal(uint64(18), CalculateBucket(18000, 1*time.Second))
}

func (s *UtilsSuite) TestCalculateBucket_ZeroTimestamp() {
	require := s.Require()

	require.Equal(uint64(0), CalculateBucket(0, 1*time.Second))
}

func (s *UtilsSuite) TestCalculateBucket_NegativeTimestamp() {
	require := s.Require()

	require.Equal(uint64(0), CalculateBucket(-100, 1*time.Second))
	require.Equal(uint64(0), CalculateBucket(-1, 500*time.Millisecond))
}

func (s *UtilsSuite) TestCalculateBucket_ZeroBucketSize() {
	require := s.Require()

	// When bucketSize is 0, return raw ms as bucket
	require.Equal(uint64(17300), CalculateBucket(17300, 0))
}

func (s *UtilsSuite) TestCalculateBucket_NegativeBucketSize() {
	require := s.Require()

	// Negative bucketSize should be treated same as zero (raw ms)
	require.Equal(uint64(5000), CalculateBucket(5000, -1*time.Second))
}

func (s *UtilsSuite) TestCalculateBucket_MillisecondPrecision() {
	require := s.Require()

	require.Equal(uint64(15), CalculateBucket(15, 1*time.Millisecond))
	require.Equal(uint64(150), CalculateBucket(150, 1*time.Millisecond))
}

// ─── EventKey ──────────────────────────────────────────────────────────────

func (s *UtilsSuite) TestEventKey_Length() {
	require := s.Require()

	key := EventKey(1, 2, 3)
	require.Len(key, 25)
}

func (s *UtilsSuite) TestEventKey_ByteLayout() {
	require := s.Require()

	bucket := uint64(0x0002030405060708)
	topicHash := uint64(0x1112131415161718)
	eventID := uint64(0x2122232425262728)

	key := EventKey(bucket, topicHash, eventID)

	require.Equal(topicHash, binary.BigEndian.Uint64(key[0:8]), "bytes 0-7 should be topicHash")
	require.Zero(key[8], "the high shard byte should be zero for unsharded keys")
	require.Equal(bucket, binary.BigEndian.Uint64(key[9:17]), "bytes 9-16 should be the time bucket")
	require.Equal(eventID, binary.BigEndian.Uint64(key[17:25]), "bytes 17-24 should be eventID")
}

func (s *UtilsSuite) TestEventKey_MaxShardAndTimeBucket() {
	require := s.Require()
	const largestSlot = 2*MaxVirtualShards - 1
	const largestID = ^uint64(0)
	key := ShardStorageBucket(largestSlot, MaxVirtualTimeBucket).EventKey(largestID, largestID)
	require.Equal(bytes.Repeat([]byte{0xff}, 25), key)
	require.Equal(largestSlot, EventShardSlot(key))
	require.Equal(MaxVirtualShards, SlotShardCount(EventShardSlot(key)))
	topic, bucket, id, err := ParseEventKey(key)
	require.NoError(err)
	require.Equal(largestID, topic)
	require.Equal(MaxVirtualTimeBucket, bucket)
	require.Equal(largestID, id)

	lowerSlot := ShardStorageBucket(511, MaxVirtualTimeBucket).EventKey(topic, largestID)
	higherSlot := ShardStorageBucket(512, 0).EventKey(topic, 0)
	require.Less(string(lowerSlot), string(higherSlot))
	require.Equal(511, EventShardSlot(lowerSlot))
	require.Equal(512, EventShardSlot(higherSlot))

	unsharded := EventKey(MaxVirtualTimeBucket+1, topic, largestID)
	end := DueUpperBound(topic, MaxVirtualTimeBucket)
	require.Equal(EventKey(MaxVirtualTimeBucket, topic, largestID), unsharded)
	require.Less(string(unsharded), string(end))
	require.Less(string(end), string(lowerSlot))
}

func (s *UtilsSuite) TestVirtualShardCountAndPublishSlot() {
	require := s.Require()
	require.Equal(65536, MaxVirtualShards)
	require.True(ValidVirtualShardCount(MaxVirtualShards))
	require.False(ValidVirtualShardCount(MaxVirtualShards + 1))
	require.False(ValidVirtualShardCount(2 * MaxVirtualShards))
	bucket, slot := ChooseStorageBucket(MaxVirtualTimeBucket, MaxVirtualShards)
	require.Equal(MaxVirtualTimeBucket, bucket)
	require.GreaterOrEqual(slot, MaxVirtualShards)
	require.Less(slot, 2*MaxVirtualShards)
}

func (s *UtilsSuite) TestEventKey_LexicographicOrdering() {
	require := s.Require()

	// Smaller topicHash should sort before larger
	key1 := EventKey(1, 100, 1)
	key2 := EventKey(1, 200, 1)
	require.Less(string(key1), string(key2), "keys should be lexicographically sortable by topicHash first")

	// Same topic, smaller bucket should sort before larger
	key3 := EventKey(10, 100, 1)
	key4 := EventKey(20, 100, 1)
	require.Less(string(key3), string(key4), "within same topic, smaller bucket should come first")

	// Same topic + bucket, smaller eventID should sort first
	key5 := EventKey(10, 100, 1)
	key6 := EventKey(10, 100, 2)
	require.Less(string(key5), string(key6), "within same bucket, smaller eventID should come first")
}

// ─── TopicLowerBound / TopicUpperBound ─────────────────────────────────────

func (s *UtilsSuite) TestTopicLowerBound() {
	require := s.Require()

	lb := TopicLowerBound(42)
	require.Len(lb, 8)
	require.Equal(uint64(42), binary.BigEndian.Uint64(lb))
}

func (s *UtilsSuite) TestTopicUpperBound() {
	require := s.Require()

	ub := TopicUpperBound(42)
	require.Len(ub, 8)
	require.Equal(uint64(43), binary.BigEndian.Uint64(ub))
}

func (s *UtilsSuite) TestTopicBounds_Ordering() {
	require := s.Require()

	lb := TopicLowerBound(100)
	ub := TopicUpperBound(100)
	require.Less(string(lb), string(ub), "lower bound must be less than upper bound")
}

// ─── DueUpperBound ─────────────────────────────────────────────────────────

func (s *UtilsSuite) TestDueUpperBound_Structure() {
	require := s.Require()

	ub := DueUpperBound(42, 17)
	require.Len(ub, 17)
	require.Equal(uint64(42), binary.BigEndian.Uint64(ub[0:8]))
	require.Equal(uint64(18), binary.BigEndian.Uint64(ub[9:17]))
}

func (s *UtilsSuite) TestDueUpperBound_CoversDueKeys() {
	require := s.Require()

	topic := uint64(7)
	maxBucket := uint64(10)
	ub := DueUpperBound(topic, maxBucket)

	// Keys with bucket ≤ maxBucket must sort BEFORE the upper bound (i.e. inside the range).
	for _, b := range []uint64{0, 1, 5, 10} {
		k := EventKey(b, topic, 999)
		require.Less(string(k), string(ub), "due key bucket=%d must be inside range", b)
	}

	// Keys with bucket > maxBucket must sort AT OR AFTER the upper bound (i.e. outside).
	for _, b := range []uint64{11, 12, 1000} {
		k := EventKey(b, topic, 0)
		require.GreaterOrEqual(string(k), string(ub), "future key bucket=%d must be outside range", b)
	}
}

func (s *UtilsSuite) TestDueUpperBound_ExcludesOtherTopics() {
	require := s.Require()

	ub := DueUpperBound(7, 100)

	// A key for a different topic must be outside the range.
	other := EventKey(0, 8, 0) // different topicHash
	require.GreaterOrEqual(string(other), string(ub))
}

// ─── BucketUpperBound ──────────────────────────────────────────────────────

func (s *UtilsSuite) TestBucketUpperBound() {
	require := s.Require()

	ub := BucketUpperBound(17)
	require.Len(ub, 8)
	require.Equal(uint64(18), binary.BigEndian.Uint64(ub))
}

func (s *UtilsSuite) TestBucketUpperBound_Zero() {
	require := s.Require()

	ub := BucketUpperBound(0)
	require.Equal(uint64(1), binary.BigEndian.Uint64(ub))
}

// ─── ParseEventKey ─────────────────────────────────────────────────────────

func (s *UtilsSuite) TestParseEventKey_Valid() {
	require := s.Require()

	topicHash := uint64(0xCAFEBABE)
	bucket := uint64(12345)
	eventID := uint64(67890)

	key := EventKey(bucket, topicHash, eventID)
	th, b, eid, err := ParseEventKey(key)

	require.NoError(err)
	require.Equal(topicHash, th)
	require.Equal(bucket, b)
	require.Equal(eventID, eid)
}

func (s *UtilsSuite) TestParseEventKey_InvalidLength() {
	require := s.Require()

	tests := []struct {
		name string
		key  []byte
	}{
		{"empty", []byte{}},
		{"too short (8)", make([]byte, 8)},
		{"too short (16)", make([]byte, 16)},
		{"old event key size", make([]byte, 24)},
		{"too long (26)", make([]byte, 26)},
		{"too long (32)", make([]byte, 32)},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			_, _, _, err := ParseEventKey(tt.key)
			require.Error(err, "expected error for key length %d", len(tt.key))
		})
	}
}

func (s *UtilsSuite) TestParseEventKey_RoundTrip() {
	require := s.Require()

	original := EventKey(999, 0xDEADBEEF, 42)
	th, b, eid, err := ParseEventKey(original)
	require.NoError(err)

	reconstructed := EventKey(b, th, eid)
	require.Equal(original, reconstructed, "round-trip must reproduce the original key")
}
