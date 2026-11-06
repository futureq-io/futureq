package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/futureq-io/futureq/internal/storage"
)

var deliveryGroupPrefix = []byte("metadata/delivery-groups/")

// A length-prefixed topic makes arbitrary topic/group names collision-free.
func deliveryGroupTopicPrefix(topic string) []byte {
	p := make([]byte, len(deliveryGroupPrefix)+4+len(topic))
	copy(p, deliveryGroupPrefix)
	binary.BigEndian.PutUint32(p[len(deliveryGroupPrefix):], uint32(len(topic)))
	copy(p[len(deliveryGroupPrefix)+4:], topic)
	return p
}

func deliveryGroupKey(topic, group string) ([]byte, error) {
	if topic == "" || group == "" || !utf8.ValidString(topic) || !utf8.ValidString(group) || uint64(len(topic)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("invalid durable delivery group")
	}
	return append(deliveryGroupTopicPrefix(topic), group...), nil
}

// MarshalRegisterDeliveryGroupCmd registers an obligation independently of any
// connected workers. Disconnect, fencing and reconciliation never remove it.
func MarshalRegisterDeliveryGroupCmd(topic, group string) ([]byte, error) {
	key, err := deliveryGroupKey(topic, group)
	if err != nil {
		return nil, err
	}
	return append([]byte{byte(RegisterDeliveryGroupCmd)}, key...), nil
}

func registerDeliveryGroupKey(cmd []byte) ([]byte, error) {
	min := 1 + len(deliveryGroupPrefix) + 4
	if len(cmd) < min || CommandType(cmd[0]) != RegisterDeliveryGroupCmd || !bytes.HasPrefix(cmd[1:], deliveryGroupPrefix) {
		return nil, fmt.Errorf("invalid delivery group command")
	}
	key := cmd[1:]
	n := uint64(binary.BigEndian.Uint32(key[len(deliveryGroupPrefix):]))
	start := len(deliveryGroupPrefix) + 4
	if n == 0 || n >= uint64(len(key)-start) {
		return nil, fmt.Errorf("invalid delivery group lengths")
	}
	if _, err := deliveryGroupKey(string(key[start:start+int(n)]), string(key[start+int(n):])); err != nil {
		return nil, err
	}
	return key, nil
}

// RegisterDeliveryGroup is the standalone equivalent of the Raft command. It
// must finish durably before exposing a named subscription to publishers.
func RegisterDeliveryGroup(db storage.DB, topic, group string) error {
	key, err := deliveryGroupKey(topic, group)
	if err != nil {
		return err
	}
	_, closer, err := db.Get(key)
	if err == nil {
		return closer.Close()
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	batch := db.NewBatch()
	defer batch.Close() //nolint:errcheck
	if err := batch.Set(key, []byte{1}); err != nil {
		return err
	}
	return batch.Commit(storage.Sync)
}

// ReadDeliveryGroups returns all durable named destinations, including those
// with no live workers. The snapshot is the publish's group-selection point.
func ReadDeliveryGroups(db storage.DB, topic string) ([]string, error) {
	prefix := deliveryGroupTopicPrefix(topic)
	upper := append(append([]byte(nil), prefix...), 0xff)
	iter, err := db.NewIter(&storage.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close() //nolint:errcheck
	var names []string
	for valid := iter.First(); valid; valid = iter.Next() {
		key := iter.Key()
		if !bytes.HasPrefix(key, prefix) || len(key) == len(prefix) {
			return nil, fmt.Errorf("invalid durable group key")
		}
		names = append(names, "g:"+string(key[len(prefix):]))
	}
	return names, iter.Error()
}

// MarshalPublishReceipt freezes destinations before the payload is committed.
func MarshalPublishReceipt(epoch uint64, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("message has no registered destinations")
	}
	if err := validatePrepares([]DeliveryPrepare{{Key: make([]byte, 24), Epoch: epoch, Recipients: recipients}}); err != nil {
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
	store, err := MarshalStoreBatchCmd(items)
	if err != nil {
		return nil, err
	}
	if uint64(len(store)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("store batch exceeds uint32 size")
	}
	out := []byte{byte(StoreDeliveryBatchCmd)}
	out = binary.BigEndian.AppendUint32(out, uint32(len(store)))
	out = append(out, store...)
	for _, item := range items {
		if err := validatePublishReceipt(item.Receipt); err != nil {
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
	n := uint64(binary.BigEndian.Uint32(cmd[1:5]))
	if n > uint64(len(cmd)-5) {
		return nil, fmt.Errorf("truncated store delivery command")
	}
	items, err := UnmarshalStoreBatchCmd(cmd[5 : 5+int(n)])
	if err != nil {
		return nil, err
	}
	data := cmd[5+int(n):]
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
		if err := validatePublishReceipt(items[i].Receipt); err != nil {
			return nil, err
		}
		data = data[int(n):]
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("trailing store delivery bytes")
	}
	return items, nil
}
