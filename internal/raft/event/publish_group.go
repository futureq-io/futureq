package raft

import (
	"encoding/binary"
	"fmt"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/lni/dragonboat/v4/statemachine"
)

const PublishGroupFrameOverhead = 4 // uint32 big-endian command length

// MarshalPublishGroupCmd wraps existing commands in order. The command tag is
// followed by length-prefixed frames. A group cannot mix ACK modes at the caller,
// and only matching publish command types are allowed in the replicated entry.
func MarshalPublishGroupCmd(commands [][]byte) ([]byte, error) {
	if len(commands) < 2 || len(commands) > config.MaxPublishBatchRequests {
		return nil, fmt.Errorf("invalid publish group count %d", len(commands))
	}
	size := 1
	var kind byte
	for i, command := range commands {
		if len(command) == 0 || !isPublishGroupCommand(command[0]) || (i > 0 && command[0] != kind) {
			return nil, fmt.Errorf("invalid publish group command %d", i)
		}
		kind = command[0]
		if len(command) > config.MaxPublishBatchBytes-size-PublishGroupFrameOverhead {
			return nil, fmt.Errorf("publish group exceeds byte limit")
		}
		size += PublishGroupFrameOverhead + len(command)
	}
	group := make([]byte, 1, size)
	group[0] = byte(PublishGroupCmd)
	for _, cmd := range commands {
		group = binary.BigEndian.AppendUint32(group, uint32(len(cmd)))
		group = append(group, cmd...)
	}
	return group, nil
}

func isPublishGroupCommand(kind byte) bool {
	return CommandType(kind) == PublishBatchCmd || CommandType(kind) == StoreDeliveryBatchCmd
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
	if len(cmd) > config.MaxPublishBatchBytes {
		return statemachine.Result{}, 0, fmt.Errorf("publish group exceeds byte limit")
	}
	var grouped statemachine.Result
	var kind byte
	var maxID uint64
	for data := cmd[1:]; len(data) > 0; {
		frame, rest, err := readPublishGroupFrame(data)
		if err != nil {
			return statemachine.Result{}, 0, err
		}
		if grouped.Value >= config.MaxPublishBatchRequests || !isPublishGroupCommand(frame[0]) || (grouped.Value > 0 && frame[0] != kind) {
			return statemachine.Result{}, 0, fmt.Errorf("invalid publish group command %d", grouped.Value)
		}
		kind = frame[0]
		result, _, id, err := s.applyEntry(batch, delivery, frame, lastID)
		if err != nil {
			return statemachine.Result{}, 0, err
		}
		if id > lastID {
			lastID, maxID = id, id
		}
		grouped.Value++
		grouped.Data = binary.BigEndian.AppendUint32(grouped.Data, uint32(8+len(result.Data)))
		grouped.Data = binary.BigEndian.AppendUint64(grouped.Data, result.Value)
		grouped.Data = append(grouped.Data, result.Data...)
		data = rest
	}
	if grouped.Value < 2 {
		return statemachine.Result{}, 0, fmt.Errorf("publish group must contain multiple commands")
	}
	return grouped, maxID, nil
}

// UnmarshalPublishGroupResults returns the individual apply results in command
// order. Validate the entire envelope before a caller acknowledges any request.
func UnmarshalPublishGroupResults(result statemachine.Result, expected int) ([]statemachine.Result, error) {
	if expected < 2 || expected > config.MaxPublishBatchRequests || result.Value != uint64(expected) {
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
