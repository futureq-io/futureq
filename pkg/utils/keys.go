package utils

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/cespare/xxhash/v2"
)

var (
	errInvalidKeyLength = "key length must be 24. got %d"
)

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
//	[0..7]   topicHash     uint64 — time bucket (enqueued_at_ms + delay_ms) / timeBucketSize
//	[8..15]  bucket  	   uint64 — xxhash64(topic)
//	[16..23] eventID       uint64 — monotonic counter from EventRepository
//
// Sorting by this key gives a time-ordered, topic-grouped layout that lets
// the dispatcher scan all due messages in a single forward iterator pass.
func EventKey(bucket, topicHash, eventID uint64) []byte {
	key := make([]byte, 24)
	binary.BigEndian.PutUint64(key[0:8], topicHash)
	binary.BigEndian.PutUint64(key[8:16], bucket)
	binary.BigEndian.PutUint64(key[16:24], eventID)
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
// Returns ok=false if the key length is not exactly 24 bytes.
func ParseEventKey(key []byte) (bucket, topicHash, eventID uint64, err error) {
	if len(key) != 24 {
		return 0, 0, 0, fmt.Errorf(errInvalidKeyLength, len(key))
	}
	bucket = binary.BigEndian.Uint64(key[0:8])
	topicHash = binary.BigEndian.Uint64(key[8:16])
	eventID = binary.BigEndian.Uint64(key[16:24])
	return bucket, topicHash, eventID, nil
}
