package raft

import (
	"encoding/binary"
	"fmt"
)

// CommandType identifies the state machine operation.
type CommandType uint8

const (
	// StoreBatchCmd atomically writes a batch of messages to Pebble.
	// The state machine uses the shared EventRepository to assign keys,
	// ensuring consistent monotonic IDs across both Raft and standalone modes.
	StoreBatchCmd CommandType = iota

	// DeleteBatchCmd atomically deletes a batch of message keys from Pebble.
	// Used for Raft-replicated ACK-driven deletions and TTL expirations.
	DeleteBatchCmd
)

// StoreBatchItem is the minimal per-message metadata carried in a StoreBatchCmd.
// The serialised StoredMessage value is passed verbatim — no re-serialisation
// occurs in the state machine. The state machine calls EventRepository to assign
// the monotonic event ID and construct the full 24-byte key.
type StoreBatchItem struct {
	// Bucket is the pre-computed time bucket (fire_at_ms / timeBucketSize).
	Bucket uint64
	// TopicHash is xxhash64(topic).
	TopicHash uint64
	Indexes   [][]byte
	// Msg is the already-serialised storagepb.StoredMessage proto bytes.
	// This slice aliases the original command buffer — do not mutate.
	Msg []byte
}

// MarshalStoreBatchCmd serialises a list of items into a compact binary command.
//
// Wire format:
//
//	[0]      CommandType   (1 byte = 0)
//	[1..8]   count         (uint64 big-endian)
//	for each item:
//	  [n..n+7]   bucket      (uint64 big-endian)
//	  [n+8..n+15] topicHash  (uint64 big-endian)
//	  [n+16..n+17] numIdx    (uint16 big-endian)
//	  for each index:
//	    [m..m+1]   idxLen    (uint16 big-endian)
//	    [m+2..]    idxData   (idxLen bytes)
//	  [x..x+3]   valLen      (uint32 big-endian)
//	  [x+4..]    value       (valLen bytes — serialised StoredMessage)
//
// The Msg and Indexes slices in items are appended directly with a single copy.
func MarshalStoreBatchCmd(items []StoreBatchItem) ([]byte, error) {
	// Pre-calculate total size to do a single allocation.
	size := 1 + 8 // cmdType + count
	for _, it := range items {
		size += 8 + 8 + 2 // bucket + topicHash + numIdx
		for _, idx := range it.Indexes {
			size += 2 + len(idx) // idxLen + data
		}
		size += 4 + len(it.Msg) // valLen + value
	}

	out := make([]byte, size)
	out[0] = byte(StoreBatchCmd)
	binary.BigEndian.PutUint64(out[1:9], uint64(len(items)))

	pos := 9
	for _, it := range items {
		binary.BigEndian.PutUint64(out[pos:pos+8], it.Bucket)
		pos += 8
		binary.BigEndian.PutUint64(out[pos:pos+8], it.TopicHash)
		pos += 8
		
		binary.BigEndian.PutUint16(out[pos:pos+2], uint16(len(it.Indexes)))
		pos += 2
		for _, idx := range it.Indexes {
			binary.BigEndian.PutUint16(out[pos:pos+2], uint16(len(idx)))
			pos += 2
			copy(out[pos:], idx)
			pos += len(idx)
		}

		binary.BigEndian.PutUint32(out[pos:pos+4], uint32(len(it.Msg)))
		pos += 4
		copy(out[pos:], it.Msg)
		pos += len(it.Msg)
	}

	return out, nil
}

// UnmarshalStoreBatchCmd deserialises a StoreBatchCmd payload.
// The Msg and Indexes slices in the returned items alias the input data slice — they are
// zero-copy views into the Dragonboat log buffer. Callers must not mutate them.
func UnmarshalStoreBatchCmd(data []byte) ([]StoreBatchItem, error) {
	if len(data) < 1+8 {
		return nil, fmt.Errorf("raft: StoreBatchCmd too short: %d bytes", len(data))
	}
	if CommandType(data[0]) != StoreBatchCmd {
		return nil, fmt.Errorf("raft: expected StoreBatchCmd (0), got %d", data[0])
	}

	count := binary.BigEndian.Uint64(data[1:9])
	items := make([]StoreBatchItem, 0, count)

	pos := 9
	for i := uint64(0); i < count; i++ {
		if pos+8+8+2 > len(data) {
			return nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d header", i)
		}
		bucket := binary.BigEndian.Uint64(data[pos : pos+8])
		pos += 8
		topicHash := binary.BigEndian.Uint64(data[pos : pos+8])
		pos += 8
		
		numIdx := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		
		indexes := make([][]byte, 0, numIdx)
		for j := 0; j < numIdx; j++ {
			if pos+2 > len(data) {
				return nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d index %d len", i, j)
			}
			idxLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2
			
			if pos+idxLen > len(data) {
				return nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d index %d data", i, j)
			}
			indexes = append(indexes, data[pos : pos+idxLen])
			pos += idxLen
		}

		if pos+4 > len(data) {
			return nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d valLen", i)
		}
		valLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4

		if pos+valLen > len(data) {
			return nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d value (need %d, have %d)", i, valLen, len(data)-pos)
		}
		// Zero-copy: alias the command buffer directly.
		value := data[pos : pos+valLen]
		pos += valLen

		items = append(items, StoreBatchItem{
			Bucket:    bucket,
			TopicHash: topicHash,
			Indexes:   indexes,
			Msg:       value,
		})
	}

	return items, nil
}

// MarshalDeleteBatchCmd serialises a list of 24-byte Pebble keys to delete.
//
// Wire format:
//
//	[0]      CommandType  (1 byte = 1)
//	[1..8]   count        (uint64 big-endian)
//	for each key:
//	  [n..n+23] key       (24 bytes — fixed size)
func MarshalDeleteBatchCmd(keys [][]byte) ([]byte, error) {
	out := make([]byte, 1+8+len(keys)*24)
	out[0] = byte(DeleteBatchCmd)
	binary.BigEndian.PutUint64(out[1:9], uint64(len(keys)))

	pos := 9
	for _, k := range keys {
		if len(k) != 24 {
			return nil, fmt.Errorf("raft: DeleteBatchCmd: key must be 24 bytes, got %d", len(k))
		}
		copy(out[pos:pos+24], k)
		pos += 24
	}

	return out, nil
}

// UnmarshalDeleteBatchCmd deserialises a DeleteBatchCmd payload.
// The returned key slices alias the input data — do not mutate data.
func UnmarshalDeleteBatchCmd(data []byte) ([][]byte, error) {
	if len(data) < 1+8 {
		return nil, fmt.Errorf("raft: DeleteBatchCmd too short: %d bytes", len(data))
	}
	if CommandType(data[0]) != DeleteBatchCmd {
		return nil, fmt.Errorf("raft: expected DeleteBatchCmd (1), got %d", data[0])
	}
	count := int(binary.BigEndian.Uint64(data[1:9]))
	if len(data) < 9+count*24 {
		return nil, fmt.Errorf("raft: DeleteBatchCmd too short for %d keys", count)
	}

	keys := make([][]byte, count)
	for i := 0; i < count; i++ {
		start := 9 + i*24
		keys[i] = data[start : start+24]
	}
	return keys, nil
}
