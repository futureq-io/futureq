package raft

import (
	"encoding/binary"
	"fmt"

	"github.com/futureq-io/futureq/pkg/utils"
)

// CommandType identifies the state machine operation.
type CommandType uint8

const (
	// StoreBatchCmd atomically writes a batch of messages to Pebble.
	// Ordered apply assigns final monotonic IDs before writing event keys.
	StoreBatchCmd CommandType = iota

	// DeleteBatchCmd atomically deletes a batch of message keys from Pebble.
	// Used for Raft-replicated ACK-driven deletions and TTL expirations.
	DeleteBatchCmd
	_ // 2: reserved for the removed JSON single-delivery preparation command.
	_ // 3: reserved for the removed JSON delivery ACK command.
	_ // 4: reserved for the removed JSON batch-delivery preparation command.

	// Delivery commands use Protobuf exclusively.
	ProtoPrepareDeliveryBatchCmd
	ProtoAckDeliveryBatchCmd
	ProtoPrepareDeliveryCmd

	_ // 8: reserved for the removed early-ACK publish command.
	SubscriptionCmd
	_          // 10: reserved for the removed quorum publish command.
	PublishCmd // one client batch, with frozen or apply-time destinations
)

// StoreBatchItem is the minimal per-message metadata carried in a StoreBatchCmd.
// The serialised StoredMessage value is passed verbatim — no re-serialisation
// occurs in the state machine. Ordered apply chooses the final monotonic event
// ID and EventRepository constructs the full 25-byte key.
type StoreBatchItem struct {
	// ID is a proposed event ID, not a committed reservation. Ordered apply
	// retains it only when it is above the applied high-water mark; otherwise
	// it assigns the next ID. All replicas derive identical keys in log order.
	ID        uint64
	Bucket    uint64
	ShardSlot int
	// TopicHash is xxhash64(topic).
	TopicHash uint64
	Indexes   [][]byte
	// Msg is the already-serialised storagepb.StoredMessage proto bytes.
	// This slice aliases the original command buffer — do not mutate.
	Msg []byte
	// Receipt holds publish-bound destinations and is stored atomically with Msg.
	Receipt []byte
}

const storeItemHeaderSize = 8 + utils.StorageBucketSize + 8 + 2

// MarshalStoreBatchCmd serialises a list of items into a compact binary command.
//
// Wire format:
//
//	[0]      CommandType   (1 byte = 0)
//	[1..8]   count         (uint64 big-endian)
//	for each item:
//	  [n..n+7]   id          (uint64 big-endian)
//	  [n+8..n+16] bucket    (17-bit shard slot and 55-bit time bucket)
//	  [n+17..n+24] topicHash (uint64 big-endian)
//	  [n+25..n+26] numIdx    (uint16 big-endian)
//	  for each index:
//	    [m..m+1]   idxLen    (uint16 big-endian)
//	    [m+2..]    idxData   (idxLen bytes)
//	  [x..x+3]   valLen      (uint32 big-endian)
//	  [x+4..]    value       (valLen bytes — serialised StoredMessage)
//
// The Msg and Indexes slices in items are appended directly with a single copy.
func MarshalStoreBatchCmd(items []StoreBatchItem) ([]byte, error) {
	out, err := marshalStoreItems(items, 1, 0, 0)
	if err != nil {
		return nil, err
	}
	out[0] = byte(StoreBatchCmd)
	return out, nil
}

// marshalStoreItems leaves space for the caller's command header and any
// receipt trailer. The count and message fields have the same layout for both
// storage and publish commands.
func marshalStoreItems(items []StoreBatchItem, header, trailer, budget int) ([]byte, error) {
	size := header + 8
	for i, it := range items {
		if it.ShardSlot < 0 || it.ShardSlot >= 2*utils.MaxVirtualShards {
			return nil, fmt.Errorf("raft: item %d has invalid shard slot %d", i, it.ShardSlot)
		}
		if len(it.Indexes) > int(^uint16(0)) {
			return nil, fmt.Errorf("raft: item %d has %d indexes, max %d", i, len(it.Indexes), ^uint16(0))
		}
		if uint64(len(it.Msg)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("raft: item %d message has %d bytes, max %d", i, len(it.Msg), ^uint32(0))
		}
		size += storeItemHeaderSize
		for j, idx := range it.Indexes {
			if len(idx) > int(^uint16(0)) {
				return nil, fmt.Errorf("raft: item %d index %d has %d bytes, max %d", i, j, len(idx), ^uint16(0))
			}
			size += 2 + len(idx) // idxLen + data
		}
		size += 4 + len(it.Msg) // valLen + value
	}

	if budget > 0 && size+trailer > budget {
		return nil, &PublishRejectedError{Reason: PublishStorageBudgetExceeded}
	}
	out := make([]byte, size, size+trailer)
	binary.BigEndian.PutUint64(out[header:], uint64(len(items)))

	pos := header + 8
	for _, it := range items {
		binary.BigEndian.PutUint64(out[pos:pos+8], it.ID)
		pos += 8
		bucket := utils.ShardStorageBucket(it.ShardSlot, it.Bucket)
		copy(out[pos:], bucket[:])
		pos += len(bucket)
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
	if len(data) == 0 || CommandType(data[0]) != StoreBatchCmd {
		return nil, fmt.Errorf("raft: invalid store command")
	}
	items, rest, err := unmarshalStoreItems(data[1:])
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("raft: StoreBatchCmd has %d trailing bytes", len(rest))
	}
	return items, nil
}

func unmarshalStoreItems(data []byte) ([]StoreBatchItem, []byte, error) {
	if len(data) < 8 {
		return nil, nil, fmt.Errorf("raft: store data too short: %d bytes", len(data))
	}
	count := binary.BigEndian.Uint64(data)
	// Each item needs at least its fixed fields and the message length.
	if count > uint64((len(data)-8)/(storeItemHeaderSize+4)) {
		return nil, nil, fmt.Errorf("raft: store data too short for %d items", count)
	}
	items := make([]StoreBatchItem, 0, count)

	pos := 8
	for i := uint64(0); i < count; i++ {
		if pos+storeItemHeaderSize > len(data) {
			return nil, nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d header", i)
		}
		id := binary.BigEndian.Uint64(data[pos : pos+8])
		pos += 8
		bucket, slot := utils.StorageBucket(data[pos : pos+utils.StorageBucketSize]).Parts()
		pos += utils.StorageBucketSize
		topicHash := binary.BigEndian.Uint64(data[pos : pos+8])
		pos += 8

		numIdx := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2

		indexes := make([][]byte, 0, numIdx)
		for j := 0; j < numIdx; j++ {
			if pos+2 > len(data) {
				return nil, nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d index %d len", i, j)
			}
			idxLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2

			if pos+idxLen > len(data) {
				return nil, nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d index %d data", i, j)
			}
			indexes = append(indexes, data[pos:pos+idxLen])
			pos += idxLen
		}

		if pos+4 > len(data) {
			return nil, nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d valLen", i)
		}
		valLen := binary.BigEndian.Uint32(data[pos : pos+4])
		pos += 4

		if uint64(valLen) > uint64(len(data)-pos) {
			return nil, nil, fmt.Errorf("raft: StoreBatchCmd truncated at item %d value (need %d, have %d)", i, valLen, len(data)-pos)
		}
		// Zero-copy: alias the command buffer directly.
		value := data[pos : pos+int(valLen)]
		pos += int(valLen)

		items = append(items, StoreBatchItem{
			ID:        id,
			Bucket:    bucket,
			ShardSlot: slot,
			TopicHash: topicHash,
			Indexes:   indexes,
			Msg:       value,
		})
	}
	return items, data[pos:], nil
}

// MarshalDeleteBatchCmd serialises a list of 25-byte event keys to delete.
//
// Wire format:
//
//	[0]      CommandType  (1 byte = 1)
//	[1..8]   count        (uint64 big-endian)
//	for each key:
//	  [n..n+24] key       (25 bytes — fixed size)
func MarshalDeleteBatchCmd(keys [][]byte) ([]byte, error) {
	out := make([]byte, 1+8+len(keys)*utils.EventKeySize)
	out[0] = byte(DeleteBatchCmd)
	binary.BigEndian.PutUint64(out[1:9], uint64(len(keys)))

	pos := 9
	for _, k := range keys {
		if len(k) != utils.EventKeySize {
			return nil, fmt.Errorf("raft: DeleteBatchCmd: key must be %d bytes, got %d", utils.EventKeySize, len(k))
		}
		copy(out[pos:pos+utils.EventKeySize], k)
		pos += utils.EventKeySize
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
	count := binary.BigEndian.Uint64(data[1:9])
	remaining := len(data) - 9
	if count > uint64(remaining/utils.EventKeySize) {
		return nil, fmt.Errorf("raft: DeleteBatchCmd too short for %d keys", count)
	}
	if uint64(remaining) != count*utils.EventKeySize {
		return nil, fmt.Errorf("raft: DeleteBatchCmd has %d trailing bytes", uint64(remaining)-count*utils.EventKeySize)
	}

	keys := make([][]byte, int(count))
	for i := range keys {
		start := 9 + i*utils.EventKeySize
		keys[i] = data[start : start+utils.EventKeySize]
	}
	return keys, nil
}
