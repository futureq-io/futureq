package raft

import (
	"encoding/binary"
	"fmt"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/pkg/utils"
)

const publishHeaderSize = 6 // tag, frozen-receipt flag, uint32 byte budget

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

// MarshalPublishCmd encodes one client batch with the leader's byte budget.
// An early ACK carries one frozen, length-prefixed receipt per message after
// the message data. For quorum ACK, apply selects the receipts in log order.
func MarshalPublishCmd(items []StoreBatchItem, frozen bool, budget int) ([]byte, error) {
	if len(items) == 0 || budget < 1 || budget > config.MaxPublishBatchBytes {
		return nil, fmt.Errorf("invalid publish batch or byte budget")
	}
	trailer := 0
	for _, item := range items {
		if frozen {
			if len(item.Receipt) > budget-trailer-4 {
				return nil, &PublishRejectedError{Reason: PublishStorageBudgetExceeded}
			}
			if err := validatePublishReceipt(item.Receipt); err != nil {
				return nil, err
			}
			trailer += 4 + len(item.Receipt)
		} else if len(item.Receipt) != 0 {
			return nil, fmt.Errorf("quorum publish cannot contain frozen receipts")
		}
	}
	cmd, err := marshalStoreItems(items, publishHeaderSize, trailer, budget)
	if err != nil {
		return nil, err
	}
	cmd[0] = byte(PublishCmd)
	binary.BigEndian.PutUint32(cmd[2:], uint32(budget))
	if frozen {
		cmd[1] = 1
		for _, item := range items {
			cmd = binary.BigEndian.AppendUint32(cmd, uint32(len(item.Receipt)))
			cmd = append(cmd, item.Receipt...)
		}
	}
	return cmd, nil
}

func unmarshalPublishCmd(cmd []byte) (items []StoreBatchItem, frozen bool, budget int, err error) {
	if len(cmd) < publishHeaderSize || CommandType(cmd[0]) != PublishCmd || cmd[1] > 1 {
		return nil, false, 0, fmt.Errorf("invalid publish header")
	}
	budget = int(binary.BigEndian.Uint32(cmd[2:]))
	if budget < len(cmd) || budget > config.MaxPublishBatchBytes {
		return nil, false, 0, fmt.Errorf("invalid publish byte budget")
	}
	items, data, err := unmarshalStoreItems(cmd[publishHeaderSize:])
	if err != nil {
		return nil, false, 0, err
	}
	if len(items) == 0 {
		return nil, false, 0, fmt.Errorf("empty publish batch")
	}

	frozen = cmd[1] == 1
	if frozen {
		for i := range items {
			if len(data) < 4 {
				return nil, false, 0, fmt.Errorf("missing publish receipt")
			}
			n := uint64(binary.BigEndian.Uint32(data))
			data = data[4:]
			if n > uint64(len(data)) {
				return nil, false, 0, fmt.Errorf("truncated publish receipt")
			}
			items[i].Receipt, data = data[:int(n)], data[int(n):]
			if err := validatePublishReceipt(items[i].Receipt); err != nil {
				return nil, false, 0, err
			}
		}
	}
	if len(data) != 0 {
		return nil, false, 0, fmt.Errorf("trailing publish bytes")
	}
	return items, frozen, budget, nil
}
