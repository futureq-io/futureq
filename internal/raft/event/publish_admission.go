package raft

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/futureq-io/futureq/internal/storage"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"google.golang.org/protobuf/proto"
)

type PublishRejectionReason byte

const (
	PublishNoDestinations PublishRejectionReason = iota + 1
	PublishTooManyDestinations
	PublishTopicNotFound
	PublishStorageBudgetExceeded
)

// PublishRejectedError is a business rejection, not an Update failure. Raft
// advances its applied index, while the rejected publish writes no message data.
type PublishRejectedError struct {
	Reason PublishRejectionReason
	Topic  string
}

func (e *PublishRejectedError) Error() string {
	switch e.Reason {
	case PublishStorageBudgetExceeded:
		return "publish batch exceeds its byte budget"
	case PublishTopicNotFound:
		return fmt.Sprintf("topic %q does not exist", e.Topic)
	case PublishTooManyDestinations:
		return fmt.Sprintf("topic %q exceeds %d destinations", e.Topic, MaxDeliveryRecipients)
	default:
		return fmt.Sprintf("topic %q has no registered destinations", e.Topic)
	}
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
	if result.Value == 0 && len(result.Data) > 0 {
		reason := PublishRejectionReason(result.Data[0])
		switch reason {
		case PublishStorageBudgetExceeded:
			if len(result.Data) == 1 {
				return &PublishRejectedError{Reason: reason}
			}
		case PublishNoDestinations, PublishTooManyDestinations, PublishTopicNotFound:
			if len(result.Data) > 1 {
				return &PublishRejectedError{Reason: reason, Topic: string(result.Data[1:])}
			}
		}
	}
	return fmt.Errorf("unexpected publish admission result")
}

// PublishRecipientsQuery selects durable recipients for standalone and early-ACK
// publishes. The Raft lookup holds the recovery guard for the entire batch.
type PublishRecipientsQuery []string

type PublishRecipientsResult struct {
	Recipients map[string][]string
	Err        error
}

func (q PublishRecipientsQuery) readForLookup(db storage.DB) (any, error) {
	// Keep admission results separate from failures of the Raft read itself.
	return q.Read(db), nil
}

func (q PublishRecipientsQuery) Read(db storage.DB) PublishRecipientsResult {
	result := make(map[string][]string)
	for _, topic := range q {
		if _, seen := result[topic]; seen {
			continue
		}
		exists, err := TopicExists(db, topic)
		if err != nil {
			return PublishRecipientsResult{Err: fmt.Errorf("read topic: %w", err)}
		}
		if !exists {
			return PublishRecipientsResult{Err: &PublishRejectedError{Reason: PublishTopicNotFound, Topic: topic}}
		}
		names, err := ReadSubscriptionRecipients(db, topic)
		if err != nil {
			return PublishRecipientsResult{Err: fmt.Errorf("read durable subscriptions: %w", err)}
		}
		if len(names) == 0 {
			return PublishRecipientsResult{Err: &PublishRejectedError{Reason: PublishNoDestinations, Topic: topic}}
		}
		if len(names) > MaxDeliveryRecipients {
			return PublishRecipientsResult{Err: &PublishRejectedError{Reason: PublishTooManyDestinations, Topic: topic}}
		}
		result[topic] = names
	}
	return PublishRecipientsResult{Recipients: result}
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

// measurePublish binds any unresolved receipts and counts the expanded writes
// before the client batch can modify storage or advance the last event ID.
func (b *DeliveryBatch) measurePublish(items []StoreBatchItem, frozen bool, budget int) (int, statemachine.Result, error) {
	var size int
	receipts := make(map[string][]byte)
	for i := range items {
		if !frozen {
			result, err := b.bindPublishItem(&items[i], receipts)
			if err != nil || len(result.Data) != 0 {
				return 0, result, err
			}
		}
		itemBytes := PublishItemStorageBytes(items[i])
		if itemBytes > budget-size {
			return 0, rejectPublish(PublishStorageBudgetExceeded, ""), nil
		}
		size += itemBytes
	}
	return size, statemachine.Result{}, nil
}

func (b *DeliveryBatch) bindPublishItem(item *StoreBatchItem, receipts map[string][]byte) (statemachine.Result, error) {
	var message deliverypb.StoredMessageMetadata
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(item.Msg, &message); err != nil {
		return statemachine.Result{}, fmt.Errorf("invalid publish message: %w", err)
	}
	if message.Topic == "" {
		return statemachine.Result{}, fmt.Errorf("publish message lacks topic")
	}
	receipt, exists := receipts[message.Topic]
	if !exists {
		registered, err := b.topicExists(message.Topic)
		if err != nil {
			return statemachine.Result{}, err
		}
		if !registered {
			return rejectPublish(PublishTopicNotFound, message.Topic), nil
		}
		names, err := b.subscriptionRecipients(message.Topic)
		if err != nil {
			return statemachine.Result{}, err
		}
		if len(names) == 0 {
			return rejectPublish(PublishNoDestinations, message.Topic), nil
		}
		if len(names) > MaxDeliveryRecipients {
			return rejectPublish(PublishTooManyDestinations, message.Topic), nil
		}
		receipt, err = MarshalPublishReceipt(0, names)
		if err != nil {
			return statemachine.Result{}, err
		}
		receipts[message.Topic] = receipt
	}
	item.Receipt = receipt
	return statemachine.Result{}, nil
}
