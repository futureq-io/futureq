package raft

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/futureq-io/futureq/internal/storage"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"google.golang.org/protobuf/proto"
)

// PublishBatchCmd deliberately carries no receipt: only quorum publishers use
// it, and their receipt is decided by the subscription state at this log entry.
func MarshalPublishBatchCmd(items []StoreBatchItem) ([]byte, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("empty publish batch")
	}
	for _, item := range items {
		if len(item.Receipt) != 0 {
			return nil, fmt.Errorf("quorum publish command cannot contain frozen receipts")
		}
	}
	cmd, err := MarshalStoreBatchCmd(items)
	if err != nil {
		return nil, err
	}
	cmd[0] = byte(PublishBatchCmd)
	return cmd, nil
}
func UnmarshalPublishBatchCmd(cmd []byte) ([]StoreBatchItem, error) {
	if len(cmd) == 0 || CommandType(cmd[0]) != PublishBatchCmd {
		return nil, fmt.Errorf("invalid publish command")
	}
	// The shared decoder leaves payload/index slices in the original log buffer.
	items, err := unmarshalStoreBatch(cmd, PublishBatchCmd)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("empty publish batch")
	}
	return items, nil
}

type PublishRejectionReason byte

const (
	PublishNoDestinations PublishRejectionReason = iota + 1
	PublishTooManyDestinations
)

// PublishRejectedError is a business rejection, not an Update failure. Raft
// advances its applied index, while the rejected publish writes no message data.
type PublishRejectedError struct {
	Reason PublishRejectionReason
	Topic  string
}

func (e *PublishRejectedError) Error() string {
	if e.Reason == PublishTooManyDestinations {
		return fmt.Sprintf("topic %q exceeds %d destinations", e.Topic, MaxDeliveryRecipients)
	}
	return fmt.Sprintf("topic %q has no registered destinations", e.Topic)
}
func rejectPublish(reason PublishRejectionReason, topic string) statemachine.Result {
	return statemachine.Result{Data: append([]byte{byte(reason)}, topic...)}
}

// CheckPublishResult also rejects malformed/unexpected results, so a publisher
// never acknowledges a partially accepted batch or an undecided admission.
func CheckPublishResult(result statemachine.Result, expected int) error {
	if expected > 0 && result.Value == uint64(expected) && len(result.Data) == 0 {
		return nil
	}
	if result.Value == 0 && len(result.Data) > 1 {
		reason := PublishRejectionReason(result.Data[0])
		if reason == PublishNoDestinations || reason == PublishTooManyDestinations {
			return &PublishRejectedError{Reason: reason, Topic: string(result.Data[1:])}
		}
	}
	return fmt.Errorf("unexpected publish admission result")
}

// subscriptionRecipients merges the persisted topic range with mutations from
// earlier entries in this Update. A DB-only scan would miss create/remove or
// unsubscribe/recreate commands that have not reached the storage commit yet.
func (b *DeliveryBatch) subscriptionRecipients(topic string) ([]string, error) {
	prefix := make([]byte, len(SubscriptionPrefix)+4)
	copy(prefix, SubscriptionPrefix)
	binary.BigEndian.PutUint32(prefix[len(SubscriptionPrefix):], uint32(len(topic)))
	prefix = append(prefix, topic...)
	records := make(map[string]*deliverypb.SubscriptionRecord)
	err := b.db.Scan(&storage.IterOptions{LowerBound: prefix, UpperBound: append(append([]byte(nil), prefix...), 0xff)}, func(key, value []byte) error {
		record := new(deliverypb.SubscriptionRecord)
		if err := proto.Unmarshal(value, record); err != nil {
			return err
		}
		records[string(key)] = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	for key, record := range b.subscriptions {
		if bytes.HasPrefix([]byte(key), prefix) {
			records[key] = record
		}
	}
	names := make([]string, 0, len(records))
	for _, record := range records {
		if record != nil && record.Active {
			names = append(names, record.Recipient)
		}
	}
	sort.Strings(names)
	return names, nil
}

// bindPublishRecipients validates the entire batch before adding any storage
// mutations. Later subscription entries cannot change an admitted message's
// receipt; all messages on a topic share its view at this publish log entry.
func (b *DeliveryBatch) bindPublishRecipients(items []StoreBatchItem) (statemachine.Result, bool, error) {
	receipts := make(map[string][]byte)
	for i := range items {
		var message deliverypb.StoredMessageMetadata
		if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(items[i].Msg, &message); err != nil {
			return statemachine.Result{}, false, fmt.Errorf("invalid publish message: %w", err)
		}
		if message.Topic == "" {
			return statemachine.Result{}, false, fmt.Errorf("publish message lacks topic")
		}
		receipt, exists := receipts[message.Topic]
		if !exists {
			names, err := b.subscriptionRecipients(message.Topic)
			if err != nil {
				return statemachine.Result{}, false, err
			}
			if len(names) == 0 {
				return rejectPublish(PublishNoDestinations, message.Topic), false, nil
			}
			if len(names) > MaxDeliveryRecipients {
				return rejectPublish(PublishTooManyDestinations, message.Topic), false, nil
			}
			receipt, err = MarshalPublishReceipt(0, names)
			if err != nil {
				return statemachine.Result{}, false, err
			}
			receipts[message.Topic] = receipt
		}
		items[i].Receipt = receipt
	}
	return statemachine.Result{}, true, nil
}
