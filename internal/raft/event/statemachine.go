package raft

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/cockroachdb/pebble/v2"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/lni/dragonboat/v4/statemachine"
	"go.uber.org/zap"
)

var appliedIndexKey = []byte("metadata/raft/applied-index")

// EventStateMachine implements statemachine.IOnDiskStateMachine.
// Pebble is used as the durable backing store; its WAL is intentionally
// disabled in clustered mode because the Dragonboat Raft log acts as the
// authoritative write-ahead log.  On restart, Dragonboat replays any log
// entries that were committed but not yet applied, so no data is lost.
type EventStateMachine struct {
	clusterID   uint64
	nodeID      uint64
	db          storage.DB
	repo        *repository.EventRepository
	lastApplied uint64
	// OnDeleteKeys is called after a DeleteBatchCmd is applied, with copies
	// of each deleted key. Used to remove entries from the dispatcher's in-flight
	// map. Safe to be nil.
	OnDeleteKeys func(keys [][]byte)
}

// NewEventStateMachineFactory returns the factory function that Dragonboat
// passes (clusterID, nodeID) to when it instantiates a new replica.
func NewEventStateMachineFactory(db storage.DB, repo *repository.EventRepository, onDeleteKeys func(keys [][]byte), logger *zap.Logger) func(uint64, uint64) statemachine.IOnDiskStateMachine {
	return func(clusterID uint64, nodeID uint64) statemachine.IOnDiskStateMachine {
		_ = logger
		return &EventStateMachine{
			clusterID:    clusterID,
			nodeID:       nodeID,
			db:           db,
			repo:         repo,
			OnDeleteKeys: onDeleteKeys,
		}
	}
}

func (s *EventStateMachine) Open(stopc <-chan struct{}) (uint64, error) {
	val, closer, err := s.db.Get(appliedIndexKey)

	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			s.lastApplied = 0
			return 0, nil
		}
		return 0, err
	}

	defer closer.Close() //nolint:errcheck

	s.lastApplied = binary.BigEndian.Uint64(val)
	return s.lastApplied, nil
}

// applyEntry applies a single Raft log entry to the batch and returns the result
// and any keys that were deleted (for DeleteBatchCmd).
//
// For StoreBatchCmd: every item carries the leader-assigned event ID. Replicas
// apply it verbatim — the state machine never generates IDs — so all replicas
// converge on identical keys.
//
// Any storage error aborts the entire Update: the batch is only committed at
// the end of Update, so returning an error means nothing partial is written.
// A replica that cannot write must stop (Dragonboat halts it on Update error)
// rather than silently diverge with a watermark lagging its stored keys.
func (s *EventStateMachine) applyEntry(batch storage.Batch, cmd []byte) (statemachine.Result, [][]byte, error) {
	if len(cmd) == 0 {
		return statemachine.Result{Value: 0}, nil, nil
	}

	switch CommandType(cmd[0]) {
	case StoreBatchCmd:
		items, err := UnmarshalStoreBatchCmd(cmd)
		if err != nil {
			// Corrupt command: deterministic across replicas, safe to skip.
			log.Printf("raft: failed to unmarshal StoreBatchCmd: %v", err)
			return statemachine.Result{Value: 0}, nil, nil
		}
		var maxID uint64
		for _, it := range items {
			if _, err := s.repo.StoreRawWithBatch(batch, it.ID, it.Bucket, it.TopicHash, it.Indexes, it.Msg); err != nil {
				return statemachine.Result{}, nil, fmt.Errorf("raft: StoreRawWithBatch: %w", err)
			}
			if it.ID > maxID {
				maxID = it.ID
			}
		}
		// Advance the in-memory counter (a follower promoted to leader must
		// never reuse IDs) and persist the high-water mark in the same batch —
		// no extra fsync, and the key rides inside snapshots.
		s.repo.ObserveID(maxID)
		if err := s.repo.PersistLastID(batch, maxID); err != nil {
			return statemachine.Result{}, nil, fmt.Errorf("raft: PersistLastID: %w", err)
		}
		return statemachine.Result{Value: uint64(len(items))}, nil, nil

	case DeleteBatchCmd:
		keys, err := UnmarshalDeleteBatchCmd(cmd)
		if err != nil {
			log.Printf("raft: failed to unmarshal DeleteBatchCmd: %v", err)
			return statemachine.Result{Value: 0}, nil, nil
		}
		deleted := make([][]byte, 0, len(keys))
		for _, k := range keys {
			kCopy := make([]byte, len(k))
			copy(kCopy, k)
			if err := batch.Delete(kCopy); err != nil {
				return statemachine.Result{}, nil, fmt.Errorf("raft: batch.Delete: %w", err)
			}
			deleted = append(deleted, kCopy)
		}
		return statemachine.Result{Value: uint64(len(deleted))}, deleted, nil

	default:
		log.Printf("raft: unknown command type: %d", cmd[0])
		return statemachine.Result{Value: 0}, nil, nil
	}
}

func (s *EventStateMachine) Update(entries []statemachine.Entry) ([]statemachine.Entry, error) {
	batch := s.db.NewBatch()

	defer batch.Close() //nolint:errcheck

	var allDeletedKeys [][]byte

	for i := range entries {
		result, deletedKeys, err := s.applyEntry(batch, entries[i].Cmd)
		if err != nil {
			// Abort before commit: the deferred batch.Close discards every
			// partial Set from this and earlier entries in the batch.
			return nil, err
		}
		entries[i].Result = result
		if len(deletedKeys) > 0 {
			allDeletedKeys = append(allDeletedKeys, deletedKeys...)
		}
		s.lastApplied = entries[i].Index
	}

	idxBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(idxBytes, s.lastApplied)
	if err := batch.Set(appliedIndexKey, idxBytes); err != nil {
		return nil, err
	}

	if err := batch.Commit(storage.NoSync); err != nil {
		return nil, err
	}

	if s.OnDeleteKeys != nil && len(allDeletedKeys) > 0 {
		s.OnDeleteKeys(allDeletedKeys)
	}

	return entries, nil
}

func (s *EventStateMachine) Sync() error {
	return s.db.Flush()
}

func (s *EventStateMachine) Lookup(query interface{}) (interface{}, error) {
	return nil, nil
}

func (s *EventStateMachine) PrepareSnapshot() (interface{}, error) {
	return s.lastApplied, nil
}

func (s *EventStateMachine) SaveSnapshot(_ interface{}, w io.Writer, stopc <-chan struct{}) error {
	return s.db.Scan(nil, func(key []byte, value []byte) error {
		select {
		case <-stopc:
			return statemachine.ErrSnapshotStopped
		default:
		}

		// k := make([]byte, len(key))
		// v := make([]byte, len(value))
		// FIXME: no sure if I should copy the key/val or not

		if err := binary.Write(w, binary.LittleEndian, uint32(len(key))); err != nil {
			return err
		}
		if _, err := w.Write(key); err != nil {
			return err
		}
		if err := binary.Write(w, binary.LittleEndian, uint32(len(value))); err != nil {
			return err
		}
		if _, err := w.Write(value); err != nil {
			return err
		}

		return nil
	})
}

func (s *EventStateMachine) RecoverFromSnapshot(r io.Reader, stopc <-chan struct{}) error {
	if err := s.clearDB(stopc); err != nil {
		return err
	}

	batch := s.db.NewBatch()

	defer batch.Close() //nolint:errcheck

	for {
		select {
		case <-stopc:
			return statemachine.ErrSnapshotStopped
		default:
		}

		var klen uint32
		if err := binary.Read(r, binary.LittleEndian, &klen); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		k := make([]byte, klen)
		if _, err := io.ReadFull(r, k); err != nil {
			return err
		}

		var vlen uint32
		if err := binary.Read(r, binary.LittleEndian, &vlen); err != nil {
			return err
		}

		v := make([]byte, vlen)
		if _, err := io.ReadFull(r, v); err != nil {
			return err
		}

		if err := batch.Set(k, v); err != nil {
			return err
		}
	}

	if err := batch.Commit(storage.Sync); err != nil {
		return err
	}

	val, closer, err := s.db.Get(appliedIndexKey)
	if err == nil {
		s.lastApplied = binary.BigEndian.Uint64(val)
		defer closer.Close() //nolint:errcheck
	} else if !errors.Is(err, pebble.ErrNotFound) {
		return err
	}

	// The snapshot carries the durable high-water mark (full DB scan). The repo
	// was constructed before recovery, so its startup load missed it — observe
	// it now or a restarted leader would hand out colliding IDs.
	lv, lCloser, err := s.db.Get(repository.LastIDKey)
	if err == nil {
		s.repo.ObserveID(binary.BigEndian.Uint64(lv))
		lCloser.Close() //nolint:errcheck
	} else if !errors.Is(err, pebble.ErrNotFound) {
		return err
	}

	return nil
}

func (s *EventStateMachine) clearDB(stopc <-chan struct{}) error {
	iter, err := s.db.NewIter(nil)
	if err != nil {
		return err
	}
	defer iter.Close() //nolint:errcheck

	batch := s.db.NewBatch()

	defer batch.Close() //nolint:errcheck

	for iter.First(); iter.Valid(); iter.Next() {
		select {
		case <-stopc:
			return statemachine.ErrSnapshotStopped
		default:
		}
		k := make([]byte, len(iter.Key()))
		copy(k, iter.Key())
		if err := batch.Delete(k); err != nil {
			return err
		}
	}
	if err := iter.Error(); err != nil {
		return err
	}

	return batch.Commit(storage.NoSync)
}

func (s *EventStateMachine) Close() error {
	return s.db.Flush()
}
