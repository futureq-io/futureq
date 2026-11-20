package utils

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"time"

	"github.com/cespare/xxhash/v2"
)

const (
	MaxVirtualShards     = 1 << 16
	MaxVirtualTimeBucket = (uint64(1) << virtualBucketShift) - 1
	virtualBucketShift   = 55
	StorageBucketSize    = 9
	eventKeyPrefixSize   = 8 + StorageBucketSize
	EventKeySize         = eventKeyPrefixSize + 8
)

// StorageBucket holds a 17-bit shard slot followed by a 55-bit time bucket.
// Slot zero is unsharded. Slot one is reserved.
type StorageBucket [StorageBucketSize]byte

// ValidVirtualShardCount accepts zero (unsharded writes) or a supported power of two.
func ValidVirtualShardCount(count int) bool {
	return count == 0 || (count >= 2 && count <= MaxVirtualShards && count&(count-1) == 0)
}

// ChooseStorageBucket assigns a shard once, before the producer proposes its
// Raft command. The encoded bucket carries the slot through replication and
// replay regardless of the replicas' local shard count.
func ChooseStorageBucket(bucket uint64, count int) (uint64, int) {
	if count == 0 || bucket > MaxVirtualTimeBucket {
		return min(bucket, MaxVirtualTimeBucket), 0
	}
	return bucket, count + rand.IntN(count)
}

// ShardStorageBucket packs the slot (shard count + shard ordinal) ahead of
// the due-time bucket. Count is the highest set bit of a sharded slot.
func ShardStorageBucket(slot int, bucket uint64) StorageBucket {
	var packed StorageBucket
	packed[0] = byte(slot >> (64 - virtualBucketShift))
	binary.BigEndian.PutUint64(packed[1:], uint64(slot)<<virtualBucketShift|min(bucket, MaxVirtualTimeBucket))
	return packed
}

// Parts returns the time bucket and shard slot.
func (b StorageBucket) Parts() (bucket uint64, slot int) {
	return binary.BigEndian.Uint64(b[1:]) & MaxVirtualTimeBucket, storageBucketSlot(b[:])
}

func storageBucketSlot(b []byte) int {
	return int(b[0])<<9 | int(b[1])<<1 | int(b[2]>>7)
}

// EventShardSlot returns zero for an unsharded event key or a non-event key.
func EventShardSlot(key []byte) int {
	if len(key) != EventKeySize {
		return 0
	}
	return storageBucketSlot(key[8:eventKeyPrefixSize])
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

// EventKey constructs the 25-byte storage key for an unsharded message.
func EventKey(bucket, topicHash, eventID uint64) []byte {
	return ShardStorageBucket(0, bucket).EventKey(topicHash, eventID)
}

// EventKey constructs the 25-byte storage key for a stored message.
//
// Layout (big-endian, lexicographically sortable):
//
//	[0..7]   topicHash uint64        — xxhash64(topic)
//	[8..16]  bucket    StorageBucket — shard slot and time bucket
//	[17..24] eventID   uint64        — monotonic counter from EventRepository
//
// Sorting groups messages by topic, then by shard slot and time bucket.
func (b StorageBucket) EventKey(topicHash, eventID uint64) []byte {
	key := make([]byte, EventKeySize)
	binary.BigEndian.PutUint64(key[0:8], topicHash)
	copy(key[8:eventKeyPrefixSize], b[:])
	binary.BigEndian.PutUint64(key[eventKeyPrefixSize:], eventID)
	return key
}

// Prefix returns the inclusive lower bound of this topic, slot and time bucket.
func (b StorageBucket) Prefix(topicHash uint64) []byte {
	key := make([]byte, eventKeyPrefixSize)
	binary.BigEndian.PutUint64(key[:8], topicHash)
	copy(key[8:], b[:])
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

// DueUpperBound returns the exclusive end of the unsharded due range.
func DueUpperBound(topicHash, maxBucket uint64) []byte {
	if maxBucket >= MaxVirtualTimeBucket {
		return ShardStorageBucket(1, 0).Prefix(topicHash)
	}
	return ShardStorageBucket(0, maxBucket+1).Prefix(topicHash)
}

// BucketUpperBound returns the exclusive upper-bound key for an iterator that
// should stop after processing all entries in buckets [0..maxBucket].
func BucketUpperBound(maxBucket uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, maxBucket+1)
	return key
}

// ParseEventKey extracts a 25-byte event key's topic hash, time bucket and event ID.
func ParseEventKey(key []byte) (topicHash, bucket, eventID uint64, err error) {
	if len(key) != EventKeySize {
		return 0, 0, 0, fmt.Errorf("key length must be %d, got %d", EventKeySize, len(key))
	}
	topicHash = binary.BigEndian.Uint64(key[0:8])
	bucket = binary.BigEndian.Uint64(key[9:eventKeyPrefixSize]) & MaxVirtualTimeBucket
	eventID = binary.BigEndian.Uint64(key[eventKeyPrefixSize:])
	return topicHash, bucket, eventID, nil
}
