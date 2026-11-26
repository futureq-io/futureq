package raft

import (
	"github.com/futureq-io/futureq/pkg/utils"

	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MarshalPublishReceipt freezes destinations before the payload is committed.
func MarshalPublishReceipt(epoch uint64, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("message has no registered destinations")
	}
	if err := validatePrepares([]DeliveryPrepare{{Key: make([]byte, utils.EventKeySize), Epoch: epoch, Recipients: recipients}}); err != nil {
		return nil, err
	}
	s := newDeliveryState(epoch, recipients)
	s.PublishBound = true
	return marshalDeliveryState(s)
}

func validatePublishReceipt(data []byte) error {
	s, err := UnmarshalDeliveryState(data)
	if err != nil {
		return err
	}
	if !s.PublishBound || len(s.Recipients) == 0 || len(s.Recipients) > MaxDeliveryRecipients || s.remaining != len(s.Recipients) {
		return fmt.Errorf("invalid publish receipt")
	}
	for _, name := range s.Recipients {
		if len(name) <= 2 || (!strings.HasPrefix(name, "g:") && !strings.HasPrefix(name, "u:")) || !utf8.ValidString(name) {
			return fmt.Errorf("invalid publish recipient")
		}
	}
	return nil
}

// StoreDeliveryBatchCmd embeds the existing store encoding, then one receipt
// per item. Message keys and frozen receipts share one atomic storage commit.
func MarshalStoreDeliveryBatchCmd(items []StoreBatchItem) ([]byte, error) {
	frame, err := MarshalPublishFrame(items, true)
	if err != nil {
		return nil, err
	}
	return append([]byte{byte(StoreDeliveryBatchCmd)}, frame...), nil
}

// MarshalPublishFrame encodes a client batch. Quorum frames carry empty
// receipt fields so apply can bind destinations in log order.
func MarshalPublishFrame(items []StoreBatchItem, frozen bool) ([]byte, error) {
	store, err := MarshalStoreBatchCmd(items)
	if err != nil {
		return nil, err
	}
	if uint64(len(store)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("store batch exceeds uint32 size")
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(len(store)))
	out = append(out, store...)
	for _, item := range items {
		if err := validateFrameReceipt(item.Receipt, frozen); err != nil {
			return nil, err
		}
		if uint64(len(item.Receipt)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("publish receipt exceeds uint32 size")
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(item.Receipt)))
		out = append(out, item.Receipt...)
	}
	return out, nil
}

func UnmarshalStoreDeliveryBatchCmd(cmd []byte) ([]StoreBatchItem, error) {
	if len(cmd) < 5 || CommandType(cmd[0]) != StoreDeliveryBatchCmd {
		return nil, fmt.Errorf("invalid store delivery command")
	}
	return unmarshalPublishFrame(cmd[1:], true)
}

func unmarshalPublishFrame(frame []byte, frozen bool) ([]StoreBatchItem, error) {
	if len(frame) < 4 {
		return nil, fmt.Errorf("truncated publish frame")
	}
	n := uint64(binary.BigEndian.Uint32(frame))
	data := frame[4:]
	if n > uint64(len(data)) {
		return nil, fmt.Errorf("truncated store delivery command")
	}
	items, err := UnmarshalStoreBatchCmd(data[:int(n)])
	if err != nil {
		return nil, err
	}
	data = data[int(n):]
	for i := range items {
		if len(data) < 4 {
			return nil, fmt.Errorf("missing publish receipt")
		}
		n := uint64(binary.BigEndian.Uint32(data[:4]))
		data = data[4:]
		if n > uint64(len(data)) {
			return nil, fmt.Errorf("truncated publish receipt")
		}
		items[i].Receipt = data[:int(n)]
		if err := validateFrameReceipt(items[i].Receipt, frozen); err != nil {
			return nil, err
		}
		data = data[int(n):]
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("trailing store delivery bytes")
	}
	return items, nil
}

func validateFrameReceipt(data []byte, frozen bool) error {
	if frozen {
		return validatePublishReceipt(data)
	}
	if len(data) != 0 {
		return fmt.Errorf("quorum publish cannot contain frozen receipts")
	}
	return nil
}
