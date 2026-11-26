package raft

import (
	"encoding/binary"
	"fmt"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/futureq-io/stormoar/v4/statemachine"
)

const (
	PublishGroupHeaderSize    = 6 // command tag, frozen-receipt flag, uint32 byte budget
	PublishGroupFrameOverhead = 4 // uint32 frame length
)

var publishReceiptKeySize = len(DeliveryStateKey(nil)) + utils.EventKeySize

// PublishItemStorageBytes counts the keys and values written for one message.
// A shared receipt still occupies space under each individual event key.
func PublishItemStorageBytes(item StoreBatchItem) int {
	size := utils.EventKeySize + len(item.Msg) + publishReceiptKeySize + len(item.Receipt)
	for _, index := range item.Indexes {
		size += len(index) + utils.EventKeySize
	}
	return size
}

// MarshalPublishGroupCmd encodes every publish, including a single client
// batch. The leader records the budget so all replicas apply the same limit.
func MarshalPublishGroupCmd(frames [][]byte, frozen bool, budget int) ([]byte, error) {
	if len(frames) < 1 || len(frames) > config.MaxPublishBatchRequests {
		return nil, fmt.Errorf("invalid publish group count %d", len(frames))
	}
	if budget < 1 || budget > config.MaxPublishBatchBytes {
		return nil, fmt.Errorf("invalid publish group budget")
	}
	size := PublishGroupHeaderSize
	for _, frame := range frames {
		if len(frame) == 0 || len(frame) > budget-size-PublishGroupFrameOverhead {
			return nil, fmt.Errorf("publish group exceeds byte limit")
		}
		size += PublishGroupFrameOverhead + len(frame)
	}
	group := make([]byte, PublishGroupHeaderSize, size)
	group[0] = byte(PublishGroupCmd)
	if frozen {
		group[1] = 1
	}
	binary.BigEndian.PutUint32(group[2:], uint32(budget))
	for _, frame := range frames {
		group = binary.BigEndian.AppendUint32(group, uint32(len(frame)))
		group = append(group, frame...)
	}
	return group, nil
}

func publishGroupBudget(cmd []byte) (int, error) {
	if len(cmd) < PublishGroupHeaderSize || cmd[1] > 1 {
		return 0, fmt.Errorf("invalid publish group header")
	}
	n := binary.BigEndian.Uint32(cmd[2:])
	if n > config.MaxPublishBatchBytes || uint64(n) < uint64(len(cmd)) {
		return 0, fmt.Errorf("invalid publish group budget")
	}
	return int(n), nil
}

func readPublishGroupFrame(data []byte) (frame, rest []byte, err error) {
	if len(data) < PublishGroupFrameOverhead {
		return nil, nil, fmt.Errorf("truncated publish group frame")
	}
	n := binary.BigEndian.Uint32(data)
	data = data[PublishGroupFrameOverhead:]
	if n == 0 || uint64(n) > uint64(len(data)) {
		return nil, nil, fmt.Errorf("invalid publish group frame length")
	}
	return data[:n], data[n:], nil
}

func (s *EventStateMachine) applyPublishGroup(batch storage.Batch, delivery *DeliveryBatch, cmd []byte, lastID uint64) (statemachine.Result, uint64, error) {
	remaining, err := publishGroupBudget(cmd)
	if err != nil {
		return statemachine.Result{}, 0, err
	}
	frozen := cmd[1] == 1
	var groupReceipts map[string][]byte
	if !frozen {
		groupReceipts = make(map[string][]byte)
	}
	var grouped statemachine.Result
	var maxID uint64
	for data := cmd[PublishGroupHeaderSize:]; len(data) > 0; {
		frame, rest, err := readPublishGroupFrame(data)
		if err != nil {
			return statemachine.Result{}, 0, err
		}
		if grouped.Value >= config.MaxPublishBatchRequests {
			return statemachine.Result{}, 0, fmt.Errorf("too many publish group frames")
		}
		items, err := unmarshalPublishFrame(frame, frozen)
		if err != nil {
			return statemachine.Result{}, 0, fmt.Errorf("invalid publish group frame %d: %w", grouped.Value, err)
		}
		if len(items) == 0 {
			return statemachine.Result{}, 0, fmt.Errorf("empty publish group frame %d", grouped.Value)
		}
		size, result, err := delivery.measurePublish(items, frozen, remaining, groupReceipts)
		if err != nil {
			return statemachine.Result{}, 0, err
		}
		if len(result.Data) == 0 {
			result, maxID, err = s.applyStoreItems(batch, delivery, items, lastID)
			if err != nil {
				return statemachine.Result{}, 0, err
			}
			lastID = maxID
			remaining -= size
		}
		grouped.Value++
		grouped.Data = binary.BigEndian.AppendUint32(grouped.Data, uint32(8+len(result.Data)))
		grouped.Data = binary.BigEndian.AppendUint64(grouped.Data, result.Value)
		grouped.Data = append(grouped.Data, result.Data...)
		data = rest
	}
	if grouped.Value == 0 {
		return statemachine.Result{}, 0, fmt.Errorf("empty publish group")
	}
	return grouped, maxID, nil
}

// UnmarshalPublishGroupResults returns the individual apply results in command
// order. Validate the entire envelope before a caller acknowledges any request.
func UnmarshalPublishGroupResults(result statemachine.Result, expected int) ([]statemachine.Result, error) {
	if expected < 1 || expected > config.MaxPublishBatchRequests || result.Value != uint64(expected) {
		return nil, fmt.Errorf("unexpected publish group result count")
	}
	results := make([]statemachine.Result, 0, expected)
	data := result.Data
	for range expected {
		frame, rest, err := readPublishGroupFrame(data)
		if err != nil || len(frame) < 8 {
			return nil, fmt.Errorf("invalid publish group result frame")
		}
		results = append(results, statemachine.Result{Value: binary.BigEndian.Uint64(frame), Data: frame[8:]})
		data = rest
	}
	if len(data) > 0 {
		return nil, fmt.Errorf("trailing publish group results")
	}
	return results, nil
}
