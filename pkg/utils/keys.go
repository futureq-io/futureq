package utils

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"time"

	"github.com/cespare/xxhash/v2"
)

var (
	errInvalidKeyLength = "key length must be 24. got %d"
)

const (
	MaxVirtualShards     = 128
	MaxVirtualTimeBucket = (uint64(1) << virtualBucketShift) - 1
	virtualBucketFlag    = uint64(1) << 63
	virtualBucketShift   = 55
)

// ValidVirtualShardCount accepts zero (legacy writes) or a supported power of two.
func ValidVirtualShardCount(count int) bool {
	return count == 0 || (count >= 2 && count <= MaxVirtualShards && count&(count-1) == 0)
}

// ChooseStorageBucket assigns a shard once, before the producer proposes its
// Raft command. The existing uint64 bucket field carries the layout and shard
// through replication, replay, and changes to the leader's configuration.
// Buckets beyond the packed range retain the legacy layout.
func ChooseStorageBucket(bucket uint64, count int) uint64 {
	if count == 0 || bucket > MaxVirtualTimeBucket {
		return bucket
	}
	return ShardStorageBucket(count+rand.IntN(count), bucket)
}

// ShardStorageBucket packs the slot (shard count + shard ordinal) ahead of
// the due-time bucket. Count is the highest set bit of the slot. The marker
// lies outside the entire legacy range of nonnegative int64 millisecond buckets.
func ShardStorageBucket(slot int, bucket uint64) uint64 {
	return virtualBucketFlag | uint64(slot)<<virtualBucketShift | bucket
}

// EventShardSlot returns zero for a legacy event key or a non-event key.
func EventShardSlot(key []byte) int {
	if len(key) != 24 || key[8]&0x80 == 0 {
		return 0
	}
	return int(key[8]&0x7f)<<1 | int(key[9]>>7)
}

// SlotShardCount returns the layout's shard count for a nonzero shard slot.
func SlotShardCount(slot int) int {
	return 1 << (bits.Len(uint(slot)) - 1)
}

// TopicHash computes a stable 64-bit hash of a topic name using xxhash64.
// This hash is embedded in Pebble keys to enable fast topic-based range scans
// without deserializing message values.
func TopicHash(topic string) uint64 {
	return xxhash.Sum64String(topic)
}

// CalculateBucket maps a Unix-millisecond timestamp to its bucket index.
// The bucket is the time divided by bucketSize. If bucketSize is zero,
// the raw millisecond timestamp is used as the bucket (maximum precision).
// Negative timestamps (invalid for scheduled messages) return bucket 0.
func CalculateBucket(unixMs int64, bucketSize time.Duration) uint64 {
	if unixMs <= 0 {
		return 0
	}
	if bucketSize <= 0 {
		return uint64(unixMs)
	}
	return uint64(unixMs) / uint64(bucketSize.Milliseconds())
}

// EventKey constructs the 24-byte Pebble key for a stored message.
//
// Layout (big-endian, lexicographically sortable):
//
//	[0..7]   topicHash uint64 — xxhash64(topic)
//	[8..15]  bucket    uint64 — time bucket, optionally preceded by a virtual shard slot
//	[16..23] eventID   uint64 — monotonic counter from EventRepository
//
// Sorting groups messages by topic, then by shard slot and time bucket.
func EventKey(bucket, topicHash, eventID uint64) []byte {
	key := make([]byte, 24)
	binary.BigEndian.PutUint64(key[0:8], topicHash)
	binary.BigEndian.PutUint64(key[8:16], bucket)
	binary.BigEndian.PutUint64(key[16:24], eventID)
	return key
}

// TopicLowerBound returns the inclusive lower-bound key for scanning all
// messages belonging to a specific topic.
func TopicLowerBound(topicHash uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, topicHash)
	return key
}

// TopicUpperBound returns the exclusive upper-bound key for scanning all
// messages belonging to a specific topic.
func TopicUpperBound(topicHash uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, topicHash+1)
	return key
}

// DueUpperBound returns the exclusive upper-bound key for scanning only the
// due (already-scheduled) messages of a topic — those whose bucket is at most
// maxBucket. The returned 16-byte prefix is [topicHash][maxBucket+1]; since
// EventKey is [topicHash][bucket][eventID] with big-endian sortable fields,
// this bound covers exactly the keys with bucket ≤ maxBucket.
func DueUpperBound(topicHash, maxBucket uint64) []byte {
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[0:8], topicHash)
	binary.BigEndian.PutUint64(key[8:16], maxBucket+1)
	return key
}

// BucketUpperBound returns the exclusive upper-bound key for an iterator that
// should stop after processing all entries in buckets [0..maxBucket].
func BucketUpperBound(maxBucket uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, maxBucket+1)
	return key
}

// ParseEventKey extracts the three components of a 24-byte event key.
// Returns (topicHash, bucket, eventID) matching the byte layout [topicHash][bucket][eventID].
// Returns ok=false if the key length is not exactly 24 bytes.
func ParseEventKey(key []byte) (topicHash, bucket, eventID uint64, err error) {
	if len(key) != 24 {
		return 0, 0, 0, fmt.Errorf(errInvalidKeyLength, len(key))
	}
	topicHash = binary.BigEndian.Uint64(key[0:8])
	bucket = binary.BigEndian.Uint64(key[8:16])
	if EventShardSlot(key) != 0 {
		bucket &= MaxVirtualTimeBucket
	}
	eventID = binary.BigEndian.Uint64(key[16:24])
	return topicHash, bucket, eventID, nil
}
