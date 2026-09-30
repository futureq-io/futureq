package dispatcher

import (
	"encoding/json"
	"sync"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/lni/dragonboat/v4/statemachine"
)

// DeliveryLedger coordinates fan-out completion through the event Raft shard,
// or an atomic local batch in standalone mode. A manifest is committed before
// the first send, so an early ACK cannot erase another replica's interest.
type DeliveryLedger struct {
	db         storage.DB
	recipients func(string) (uint64, []string, bool)
	propose    func([]byte) (statemachine.Result, error)
	backend    DeleteBackend
	mu         sync.Mutex // serializes standalone prepare/ACK/explicit delete
	OnDelete   func([][]byte)
}

func NewDeliveryLedger(db storage.DB, recipients func(string) (uint64, []string, bool), backend DeleteBackend, propose func([]byte) (statemachine.Result, error)) *DeliveryLedger {
	return &DeliveryLedger{db: db, recipients: recipients, backend: backend, propose: propose}
}

func (l *DeliveryLedger) Prepare(topic string, key []byte) (*raft.DeliveryState, error) {
	epoch, recipients, active := l.recipients(topic)
	if !active {
		return nil, nil
	}
	state, err := raft.ReadDeliveryState(l.db, key)
	if err != nil {
		return nil, err
	}
	if state != nil && state.Epoch >= epoch {
		return state, nil
	}
	cmd, err := raft.MarshalPrepareDeliveryCmd(key, epoch, recipients)
	if err != nil {
		return nil, err
	}
	result, err := l.apply(cmd)
	if err != nil || len(result.Data) == 0 {
		return nil, err
	}
	var prepared raft.DeliveryState
	if err := json.Unmarshal(result.Data, &prepared); err != nil {
		return nil, err
	}
	return &prepared, nil
}

func (l *DeliveryLedger) Acknowledge(acks []raft.DeliveryAck) error {
	cmd, err := raft.MarshalAckDeliveryBatchCmd(acks)
	if err != nil {
		return err
	}
	_, err = l.apply(cmd)
	return err
}

func (l *DeliveryLedger) apply(cmd []byte) (statemachine.Result, error) {
	if l.propose != nil {
		return l.propose(cmd)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	batch := l.db.NewBatch()
	defer batch.Close() //nolint:errcheck
	result, deleted, err := raft.NewDeliveryBatch(l.db, batch).Apply(cmd)
	if err != nil {
		return statemachine.Result{}, err
	}
	if err := batch.Commit(storage.Sync); err != nil {
		return statemachine.Result{}, err
	}
	if len(deleted) > 0 && l.OnDelete != nil {
		l.OnDelete(deleted)
	}
	return result, nil
}

func (l *DeliveryLedger) DeleteKeys(keys [][]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.backend.DeleteKeys(keys)
}
