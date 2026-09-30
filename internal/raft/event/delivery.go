package raft

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/lni/dragonboat/v4/statemachine"
)

// DeliveryState shares one payload across independent logical deliveries.
// Named group receipts survive disconnects and reassignment. Universal
// subscriptions are ephemeral and lose their interest when unregistered.
type DeliveryState struct {
	Epoch      uint64          `json:"epoch"`
	Recipients map[string]bool `json:"recipients"` // recipient -> acknowledged
}

func (s *DeliveryState) Needs(recipient string) bool {
	if s == nil {
		return false
	}
	acked, exists := s.Recipients[recipient]
	return exists && !acked
}

func (s *DeliveryState) complete() bool {
	for _, acked := range s.Recipients {
		if !acked {
			return false
		}
	}
	return true
}

type DeliveryAck struct {
	Key       []byte `json:"key"`
	Recipient string `json:"recipient"`
}

type prepareDelivery struct {
	Key        []byte   `json:"key"`
	Epoch      uint64   `json:"epoch"`
	Recipients []string `json:"recipients"`
}

func DeliveryStateKey(key []byte) []byte {
	return append([]byte("metadata/delivery/"), key...)
}

func ReadDeliveryState(db storage.DB, key []byte) (*DeliveryState, error) {
	data, closer, err := db.Get(DeliveryStateKey(key))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close() //nolint:errcheck
	var state DeliveryState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func MarshalPrepareDeliveryCmd(key []byte, epoch uint64, recipients []string) ([]byte, error) {
	data, err := json.Marshal(prepareDelivery{Key: key, Epoch: epoch, Recipients: recipients})
	return append([]byte{byte(PrepareDeliveryCmd)}, data...), err
}

func MarshalAckDeliveryBatchCmd(acks []DeliveryAck) ([]byte, error) {
	data, err := json.Marshal(acks)
	return append([]byte{byte(AckDeliveryBatchCmd)}, data...), err
}

// DeliveryBatch overlays uncommitted mutations so several prepares, ACKs, and
// deletes in one Dragonboat Update see each other, rather than stale DB reads.
type DeliveryBatch struct {
	db      storage.DB
	batch   storage.Batch
	states  map[string]*DeliveryState
	present map[string]bool
}

func NewDeliveryBatch(db storage.DB, batch storage.Batch) *DeliveryBatch {
	return &DeliveryBatch{db: db, batch: batch, states: make(map[string]*DeliveryState), present: make(map[string]bool)}
}

func (b *DeliveryBatch) ObserveStore(key []byte) { b.present[string(key)] = true }

func (b *DeliveryBatch) Delete(key []byte) error {
	if err := b.batch.Delete(key); err != nil {
		return err
	}
	if err := b.batch.Delete(DeliveryStateKey(key)); err != nil {
		return err
	}
	b.present[string(key)] = false
	b.states[string(key)] = nil
	return nil
}

func (b *DeliveryBatch) load(key []byte) (*DeliveryState, error) {
	if state, ok := b.states[string(key)]; ok {
		return state, nil
	}
	state, err := ReadDeliveryState(b.db, key)
	if err == nil {
		b.states[string(key)] = state
	}
	return state, err
}

func (b *DeliveryBatch) exists(key []byte) (bool, error) {
	if present, ok := b.present[string(key)]; ok {
		return present, nil
	}
	_, closer, err := b.db.Get(key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, closer.Close()
}

func (b *DeliveryBatch) save(key []byte, state *DeliveryState) ([]byte, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if err := b.batch.Set(DeliveryStateKey(key), data); err != nil {
		return nil, err
	}
	b.states[string(key)] = state
	return data, nil
}

func (b *DeliveryBatch) Apply(cmd []byte) (statemachine.Result, [][]byte, error) {
	if len(cmd) == 0 {
		return statemachine.Result{}, nil, fmt.Errorf("empty delivery command")
	}
	switch CommandType(cmd[0]) {
	case PrepareDeliveryCmd:
		var prepare prepareDelivery
		if err := json.Unmarshal(cmd[1:], &prepare); err != nil {
			return statemachine.Result{}, nil, err
		}
		if len(prepare.Key) != 24 {
			return statemachine.Result{}, nil, fmt.Errorf("invalid delivery key length")
		}
		state, err := b.load(prepare.Key)
		if err != nil {
			return statemachine.Result{}, nil, err
		}
		if state == nil {
			exists, err := b.exists(prepare.Key)
			if err != nil || !exists || len(prepare.Recipients) == 0 {
				return statemachine.Result{}, nil, err
			}
			state = &DeliveryState{Epoch: prepare.Epoch, Recipients: make(map[string]bool)}
			for _, recipient := range prepare.Recipients {
				state.Recipients[recipient] = false
			}
		} else if prepare.Epoch > state.Epoch {
			// Drop vanished ephemeral subscriptions. Named group interests
			// remain until ACK or explicit deletion/TTL, including while offline.
			active := make(map[string]bool, len(prepare.Recipients))
			for _, recipient := range prepare.Recipients {
				active[recipient] = true
			}
			for recipient := range state.Recipients {
				if strings.HasPrefix(recipient, "u:") && !active[recipient] {
					state.Recipients[recipient] = true
				}
			}
			state.Epoch = prepare.Epoch
		}
		if state.complete() {
			err := b.Delete(prepare.Key)
			return statemachine.Result{}, [][]byte{prepare.Key}, err
		}
		data, err := b.save(prepare.Key, state)
		return statemachine.Result{Value: 1, Data: data}, nil, err

	case AckDeliveryBatchCmd:
		var acks []DeliveryAck
		if err := json.Unmarshal(cmd[1:], &acks); err != nil {
			return statemachine.Result{}, nil, err
		}
		var deleted [][]byte
		for _, ack := range acks {
			state, err := b.load(ack.Key)
			if err != nil {
				return statemachine.Result{}, nil, err
			}
			if state == nil || !state.Needs(ack.Recipient) {
				continue
			}
			state.Recipients[ack.Recipient] = true
			if state.complete() {
				if err := b.Delete(ack.Key); err != nil {
					return statemachine.Result{}, nil, err
				}
				deleted = append(deleted, ack.Key)
			} else if _, err := b.save(ack.Key, state); err != nil {
				return statemachine.Result{}, nil, err
			}
		}
		return statemachine.Result{Value: uint64(len(deleted))}, deleted, nil
	default:
		return statemachine.Result{}, nil, fmt.Errorf("invalid delivery command %d", cmd[0])
	}
}
