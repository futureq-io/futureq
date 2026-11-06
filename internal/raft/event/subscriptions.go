package raft

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/futureq-io/futureq/internal/storage"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/lni/dragonboat/v4/statemachine"
	"google.golang.org/protobuf/proto"
)

const (
	SubscriptionCreate uint32 = iota + 1
	SubscriptionRemove
	SubscriptionConnect
	SubscriptionDisconnect
	SubscriptionRenew
	SubscriptionExpire
	SubscriptionCleanup
)

var SubscriptionPrefix = []byte("metadata/subscriptions/")

var retiredSubscriptionKey = []byte("metadata/has-retired-subscriptions")

const subscriptionGenerationPrefix = "metadata/subscription-generations/"
const subscriptionRecipientPrefix = "g:s:"

func SubscriptionKey(topic, group, subscriber string) ([]byte, error) {
	if topic == "" || (group == "") == (subscriber == "") || !utf8.ValidString(topic+group+subscriber) || len(topic)+len(group)+len(subscriber) > 4096 {
		return nil, fmt.Errorf("topic and exactly one of group_id or subscriber_id are required (maximum 4096 bytes)")
	}
	identity := "g:" + group
	if subscriber != "" {
		identity = "u:" + subscriber
	}
	key := make([]byte, len(SubscriptionPrefix)+4)
	copy(key, SubscriptionPrefix)
	binary.BigEndian.PutUint32(key[len(SubscriptionPrefix):], uint32(len(topic)))
	return append(append(key, topic...), identity...), nil
}

func ReadSubscription(db storage.DB, topic, group, subscriber string) (*deliverypb.SubscriptionRecord, error) {
	key, err := SubscriptionKey(topic, group, subscriber)
	if err != nil {
		return nil, err
	}
	return readSubscriptionKey(db, key)
}
func ReadSubscriptionRecipient(db storage.DB, recipient string) (*deliverypb.SubscriptionRecord, error) {
	return readSubscriptionKey(db, []byte(subscriptionGenerationPrefix+recipient))
}
func readSubscriptionKey(db storage.DB, key []byte) (*deliverypb.SubscriptionRecord, error) {
	data, closer, err := db.Get(key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close() //nolint:errcheck
	record := new(deliverypb.SubscriptionRecord)
	if err := proto.Unmarshal(data, record); err != nil {
		return nil, err
	}
	return record, nil
}
func ReadSubscriptionRecipients(db storage.DB, topic string) ([]string, error) {
	prefix := make([]byte, len(SubscriptionPrefix)+4)
	copy(prefix, SubscriptionPrefix)
	binary.BigEndian.PutUint32(prefix[len(SubscriptionPrefix):], uint32(len(topic)))
	prefix = append(prefix, topic...)
	var names []string
	err := db.Scan(&storage.IterOptions{LowerBound: prefix, UpperBound: append(append([]byte(nil), prefix...), 0xff)}, func(_, value []byte) error {
		record := new(deliverypb.SubscriptionRecord)
		if err := proto.Unmarshal(value, record); err != nil {
			return err
		}
		if record.Active {
			names = append(names, record.Recipient)
		}
		return nil
	})
	return names, err
}
func MarshalSubscriptionCommand(command *deliverypb.SubscriptionCommand) ([]byte, error) {
	if command.Operation < SubscriptionCreate || command.Operation > SubscriptionCleanup || command.NowMs <= 0 || len(command.Mutations) > 256 || len(command.Keys) > 256 || command.LeaseMs < 0 || command.IdleTimeoutMs < 0 {
		return nil, fmt.Errorf("invalid subscription command")
	}
	for _, mutation := range command.Mutations {
		if _, err := SubscriptionKey(mutation.Topic, mutation.GroupId, mutation.SubscriberId); err != nil {
			return nil, err
		}
		if mutation.Recipient != "" && (!strings.HasPrefix(mutation.Recipient, subscriptionRecipientPrefix) || len(mutation.Recipient) > 128) {
			return nil, fmt.Errorf("invalid subscription generation")
		}
	}
	for _, key := range command.Keys {
		if len(key) != 24 {
			return nil, fmt.Errorf("invalid cleanup key")
		}
	}
	data, err := proto.Marshal(command)
	if err != nil {
		return nil, err
	}
	return append([]byte{byte(SubscriptionCmd)}, data...), nil
}

// Values overlay earlier commands from the same Dragonboat Update, including
// create/delete/recreate and their generation tombstones.
func (b *DeliveryBatch) subscriptionRead(key []byte) (*deliverypb.SubscriptionRecord, error) {
	if record, ok := b.subscriptions[string(key)]; ok {
		return record, nil
	}
	record, err := readSubscriptionKey(b.db, key)
	if err != nil {
		return nil, err
	}
	if b.subscriptions == nil {
		b.subscriptions = make(map[string]*deliverypb.SubscriptionRecord)
	}
	b.subscriptions[string(key)] = record
	return record, nil
}
func (b *DeliveryBatch) subscriptionSave(key []byte, record *deliverypb.SubscriptionRecord) error {
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(record)
	if err != nil {
		return err
	}
	if err := b.batch.Set(key, data); err != nil {
		return err
	}
	if err := b.batch.Set([]byte(subscriptionGenerationPrefix+record.Recipient), data); err != nil {
		return err
	}
	if !record.Active {
		if err := b.batch.Set(retiredSubscriptionKey, []byte{1}); err != nil {
			return err
		}
	}
	b.subscriptions[string(key)] = record
	b.subscriptions[subscriptionGenerationPrefix+record.Recipient] = record
	return nil
}

func (b *DeliveryBatch) ApplySubscription(data []byte) (statemachine.Result, [][]byte, error) {
	command := new(deliverypb.SubscriptionCommand)
	if len(data) == 0 || CommandType(data[0]) != SubscriptionCmd {
		return statemachine.Result{}, nil, fmt.Errorf("invalid subscription tag")
	}
	if err := proto.Unmarshal(data[1:], command); err != nil {
		return statemachine.Result{}, nil, err
	}
	if _, err := MarshalSubscriptionCommand(command); err != nil {
		return statemachine.Result{}, nil, err
	}
	if command.Operation == SubscriptionCleanup {
		return b.cleanSubscriptions(command.Keys)
	}
	var result statemachine.Result
	for _, mutation := range command.Mutations {
		key, _ := SubscriptionKey(mutation.Topic, mutation.GroupId, mutation.SubscriberId)
		record, err := b.subscriptionRead(key)
		if err != nil {
			return result, nil, err
		}
		if command.Operation == SubscriptionCreate {
			if record == nil || !record.Active {
				if mutation.Recipient == "" {
					return result, nil, fmt.Errorf("missing generation")
				}
				record = &deliverypb.SubscriptionRecord{Topic: mutation.Topic, GroupId: mutation.GroupId, SubscriberId: mutation.SubscriberId, Recipient: mutation.Recipient, Active: true, DisconnectedSinceMs: command.NowMs}
			}
		} else {
			if record == nil || !record.Active || (mutation.Recipient != "" && record.Recipient != mutation.Recipient) {
				continue
			}
			// Clone: the old generation entry must not be mutated through an alias.
			record = proto.Clone(record).(*deliverypb.SubscriptionRecord)
			switch command.Operation {
			case SubscriptionRemove:
				record.Active = false
				record.Sessions = nil
			case SubscriptionConnect:
				if mutation.Session == "" || command.LeaseMs <= 0 {
					return result, nil, fmt.Errorf("invalid session lease")
				}
				if record.Sessions == nil {
					record.Sessions = make(map[string]int64)
				}
				record.Sessions[mutation.Session] = command.NowMs + command.LeaseMs
				record.DisconnectedSinceMs = 0
			case SubscriptionRenew:
				deadline, exists := record.Sessions[mutation.Session]
				if !exists || deadline <= command.NowMs || command.LeaseMs <= 0 {
					continue
				}
				record.Sessions[mutation.Session] = command.NowMs + command.LeaseMs
			case SubscriptionDisconnect:
				if _, exists := record.Sessions[mutation.Session]; !exists {
					continue
				}
				delete(record.Sessions, mutation.Session)
				for session, deadline := range record.Sessions {
					if deadline <= command.NowMs {
						delete(record.Sessions, session)
					}
				}
				if len(record.Sessions) == 0 {
					record.DisconnectedSinceMs = command.NowMs
				}
			case SubscriptionExpire:
				var latest int64
				for session, deadline := range record.Sessions {
					if deadline > latest {
						latest = deadline
					}
					if deadline <= command.NowMs {
						delete(record.Sessions, session)
					}
				}
				if len(record.Sessions) > 0 {
					break // Persist removal of expired leases without expiring live workers.
				}
				if record.DisconnectedSinceMs == 0 {
					record.DisconnectedSinceMs = latest
					if latest == 0 {
						record.DisconnectedSinceMs = command.NowMs
					}
				}
				if command.IdleTimeoutMs > 0 && command.NowMs-record.DisconnectedSinceMs >= command.IdleTimeoutMs {
					record.Active = false
				}
			}
		}
		if err := b.subscriptionSave(key, record); err != nil {
			return result, nil, err
		}
		result.Value++
		result.Data, err = (proto.MarshalOptions{Deterministic: true}).Marshal(record)
		if err != nil {
			return result, nil, err
		}
	}
	return result, nil, nil
}
func (b *DeliveryBatch) cleanSubscriptions(keys [][]byte) (statemachine.Result, [][]byte, error) {
	var deleted [][]byte
	for _, key := range keys {
		state, err := b.load(key)
		if err != nil {
			return statemachine.Result{}, nil, err
		}
		if state == nil {
			continue
		}
		changed := false
		for _, recipient := range state.Recipients {
			if !state.Needs(recipient) || !strings.HasPrefix(recipient, subscriptionRecipientPrefix) {
				continue
			}
			record, err := b.subscriptionRead([]byte(subscriptionGenerationPrefix + recipient))
			if err != nil {
				return statemachine.Result{}, nil, err
			}
			if record != nil && !record.Active {
				changed = state.acknowledge(recipient) || changed
			}
		}
		if !changed {
			continue
		}
		if state.complete() {
			if err := b.Delete(key); err != nil {
				return statemachine.Result{}, nil, err
			}
			deleted = append(deleted, key)
		} else {
			b.save(key, state)
		}
	}
	return statemachine.Result{Value: uint64(len(deleted))}, deleted, nil
}

// HasRetiredSubscriptions avoids receipt scans during the normal lifecycle when
// no subscription has ever been removed. Retirement markers survive snapshots.
func HasRetiredSubscriptions(db storage.DB) (bool, error) {
	_, closer, err := db.Get(retiredSubscriptionKey)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, closer.Close()
}
