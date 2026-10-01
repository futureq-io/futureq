package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
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
	// Installed before Run; allows a bounded preparation job to cancel its
	// Raft wait without changing the asynchronous ACK retry path.
	ProposeContext func(context.Context, []byte) (statemachine.Result, error)
}

// PrepareBatch returns committed states keyed by event key. Inputs carry one
// immutable epoch/recipient view captured by the scheduler. Timeouts are
// uncertain completion: retries use the same idempotent preparation command.
func (l *DeliveryLedger) PrepareBatch(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
	started := time.Now()
	defer func() {
		elapsed := float64(time.Since(started).Microseconds()) / 1000
		metrics.DeliveryPrepareDurationMs.WithLabelValues("batch").Observe(elapsed)
		for range items {
			metrics.DeliveryPrepareDurationMs.WithLabelValues("candidate").Observe(elapsed)
		}
	}()
	// Validate even the inputs whose manifests are already present.
	if _, err := raft.MarshalPrepareDeliveryBatchCmd(items); err != nil {
		return nil, err
	}
	states := make(map[string]*raft.DeliveryState, len(items))
	missing := make([]raft.DeliveryPrepare, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state, err := raft.ReadDeliveryState(l.db, item.Key)
		if err != nil {
			return nil, err
		}
		if state != nil && state.Epoch >= item.Epoch {
			states[string(item.Key)] = state
			metrics.DeliveryManifestHitsTotal.Inc()
		} else {
			missing = append(missing, item)
		}
	}
	if len(missing) == 0 {
		return states, nil
	}
	cmd, err := raft.MarshalPrepareDeliveryBatchCmd(missing)
	if err != nil {
		return nil, err
	}
	metrics.DeliveryPrepareProposalsTotal.Inc()
	metrics.DeliveryPrepareKeys.Observe(float64(len(missing)))
	var result statemachine.Result
	if l.ProposeContext != nil {
		result, err = l.ProposeContext(ctx, cmd)
	} else {
		result, err = l.apply(cmd)
	}
	if err != nil {
		return nil, err
	}
	var prepared []raft.DeliveryPrepared
	if err := json.Unmarshal(result.Data, &prepared); err != nil {
		return nil, err
	}
	if len(prepared) != len(missing) {
		return nil, fmt.Errorf("incomplete delivery preparation result")
	}
	expected := make(map[string]bool, len(missing))
	for _, item := range missing {
		expected[string(item.Key)] = true
	}
	for _, item := range prepared {
		if !expected[string(item.Key)] {
			return nil, fmt.Errorf("unexpected or duplicate prepared key")
		}
		delete(expected, string(item.Key))
		states[string(item.Key)] = item.State
	}
	return states, nil
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
