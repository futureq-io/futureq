package dispatcher

import (
	"context"
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
	if err := raft.ValidateDeliveryPrepares(items); err != nil {
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
	metrics.DeliveryPrepareProposalsTotal.Inc()
	metrics.DeliveryPrepareKeys.Observe(float64(len(missing)))
	var prepared []raft.DeliveryPrepared
	var err error
	if l.propose == nil && l.ProposeContext == nil {
		err = l.applyLocal(storage.Sync, func(batch *raft.DeliveryBatch) ([][]byte, error) {
			var deleted [][]byte
			var err error
			prepared, deleted, err = batch.PrepareBatch(missing)
			return deleted, err
		})
	} else {
		var cmd []byte
		cmd, err = raft.MarshalPrepareDeliveryBatchCmd(missing)
		if err != nil {
			return nil, err
		}
		var result statemachine.Result
		if l.ProposeContext != nil {
			result, err = l.ProposeContext(ctx, cmd)
		} else {
			result, err = l.apply(cmd)
		}
		if err == nil {
			prepared, err = raft.UnmarshalDeliveryPrepared(result.Data)
		}
	}
	if err != nil {
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
	if l.propose == nil {
		if err := raft.ValidateDeliveryPrepares([]raft.DeliveryPrepare{{Key: key, Epoch: epoch, Recipients: recipients}}); err != nil {
			return nil, err
		}
		var prepared *raft.DeliveryState
		err := l.applyLocal(storage.Sync, func(batch *raft.DeliveryBatch) ([][]byte, error) {
			var removed bool
			var err error
			prepared, removed, err = batch.Prepare(raft.DeliveryPrepare{Key: key, Epoch: epoch, Recipients: recipients})
			if removed {
				return [][]byte{key}, err
			}
			return nil, err
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}
	cmd, err := raft.MarshalPrepareDeliveryCmd(key, epoch, recipients)
	if err != nil {
		return nil, err
	}
	result, err := l.apply(cmd)
	if err != nil || len(result.Data) == 0 {
		return nil, err
	}
	return raft.UnmarshalDeliveryState(result.Data)
}

func (l *DeliveryLedger) Acknowledge(acks []raft.DeliveryAck) error {
	if l.propose == nil {
		return l.applyLocal(storage.NoSync, func(batch *raft.DeliveryBatch) ([][]byte, error) {
			return batch.Acknowledge(acks)
		})
	}
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
	mode := storage.Sync
	if raft.CommandType(cmd[0]) == raft.AckDeliveryBatchCmd || raft.CommandType(cmd[0]) == raft.ProtoAckDeliveryBatchCmd {
		mode = storage.NoSync
	}
	var result statemachine.Result
	err := l.applyLocal(mode, func(batch *raft.DeliveryBatch) ([][]byte, error) {
		var deleted [][]byte
		var err error
		result, deleted, err = batch.Apply(cmd)
		return deleted, err
	})
	if err != nil {
		return statemachine.Result{}, err
	}
	return result, nil
}

func (l *DeliveryLedger) applyLocal(mode storage.SyncMode, apply func(*raft.DeliveryBatch) ([][]byte, error)) error {
	stage := "prepare"
	if mode == storage.NoSync {
		stage = "ack"
	}
	lockStarted := time.Now()
	l.mu.Lock()
	metrics.DeliveryPrepareDurationMs.WithLabelValues(stage + "_lock_wait").Observe(float64(time.Since(lockStarted).Nanoseconds()) / 1e6)
	defer l.mu.Unlock()
	batch := l.db.NewBatch()
	defer batch.Close() //nolint:errcheck
	applyStarted := time.Now()
	delivery := raft.NewDeliveryBatch(l.db, batch)
	deleted, err := apply(delivery)
	if err == nil {
		err = delivery.Flush()
	}
	metrics.DeliveryPrepareDurationMs.WithLabelValues(stage + "_apply").Observe(float64(time.Since(applyStarted).Nanoseconds()) / 1e6)
	if err != nil {
		return err
	}

	// Preparation must be durable before delivery; only ACK persistence may
	// trade crash durability for throughput (duplicates remain permissible).
	commitStarted := time.Now()
	commitErr := batch.Commit(mode)
	metrics.DeliveryPrepareDurationMs.WithLabelValues(stage + "_commit").Observe(float64(time.Since(commitStarted).Nanoseconds()) / 1e6)
	if commitErr != nil {
		return commitErr
	}
	if len(deleted) > 0 && l.OnDelete != nil {
		l.OnDelete(deleted)
	}
	return nil
}

func (l *DeliveryLedger) DeleteKeys(keys [][]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.backend.DeleteKeys(keys)
}
