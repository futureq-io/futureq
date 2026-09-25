package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

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
	clusterID       uint64
	nodeID          uint64
	db              storage.DB
	repo            *repository.EventRepository
	lastApplied     uint64
	lastPersistedID uint64
	logger          *zap.Logger
	// OnDeleteKeys is called after a DeleteBatchCmd is applied, with copies
	// of each deleted key. Used to remove entries from the dispatcher's in-flight
	// map. Safe to be nil.
	OnDeleteKeys func(keys [][]byte)
}

// NewEventStateMachineFactory returns the factory function that Dragonboat
// passes (clusterID, nodeID) to when it instantiates a new replica.
func NewEventStateMachineFactory(db storage.DB, repo *repository.EventRepository, onDeleteKeys func(keys [][]byte), logger *zap.Logger) func(uint64, uint64) statemachine.IOnDiskStateMachine {
	return func(clusterID uint64, nodeID uint64) statemachine.IOnDiskStateMachine {
		return &EventStateMachine{
			clusterID:    clusterID,
			nodeID:       nodeID,
			db:           db,
			repo:         repo,
			logger:       logger.Named("event_sm"),
			OnDeleteKeys: onDeleteKeys,
		}
	}
}

func (s *EventStateMachine) Open(stopc <-chan struct{}) (uint64, error) {
	index, err := readStoredUint64(s.db, appliedIndexKey)
	if err != nil {
		return 0, err
	}
	lastID, err := readStoredUint64(s.db, repository.LastIDKey)
	if err != nil {
		return 0, err
	}
	s.lastApplied = index
	s.lastPersistedID = lastID
	s.repo.ObserveID(lastID)
	return s.lastApplied, nil
}

func readStoredUint64(db storage.DB, key []byte) (uint64, error) {
	val, closer, err := db.Get(key)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer closer.Close() //nolint:errcheck
	if len(val) != 8 {
		return 0, fmt.Errorf("raft: key %q has %d bytes, want 8", key, len(val))
	}
	return binary.BigEndian.Uint64(val), nil
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
func (s *EventStateMachine) applyEntry(batch storage.Batch, cmd []byte) (statemachine.Result, [][]byte, uint64, error) {
	if len(cmd) == 0 {
		return statemachine.Result{Value: 0}, nil, 0, nil
	}

	switch CommandType(cmd[0]) {
	case StoreBatchCmd:
		items, err := UnmarshalStoreBatchCmd(cmd)
		if err != nil {
			return statemachine.Result{}, nil, 0, fmt.Errorf("raft: invalid StoreBatchCmd: %w", err)
		}
		var maxID uint64
		for _, it := range items {
			if _, err := s.repo.StoreRawWithBatch(batch, it.ID, it.Bucket, it.TopicHash, it.Indexes, it.Msg); err != nil {
				return statemachine.Result{}, nil, 0, fmt.Errorf("raft: StoreRawWithBatch: %w", err)
			}
			if it.ID > maxID {
				maxID = it.ID
			}
		}
		return statemachine.Result{Value: uint64(len(items))}, nil, maxID, nil

	case DeleteBatchCmd:
		keys, err := UnmarshalDeleteBatchCmd(cmd)
		if err != nil {
			return statemachine.Result{}, nil, 0, fmt.Errorf("raft: invalid DeleteBatchCmd: %w", err)
		}
		deleted := make([][]byte, 0, len(keys))
		for _, k := range keys {
			kCopy := make([]byte, len(k))
			copy(kCopy, k)
			if err := batch.Delete(kCopy); err != nil {
				return statemachine.Result{}, nil, 0, fmt.Errorf("raft: batch.Delete: %w", err)
			}
			deleted = append(deleted, kCopy)
		}
		return statemachine.Result{Value: uint64(len(deleted))}, deleted, 0, nil

	default:
		return statemachine.Result{}, nil, 0, fmt.Errorf("raft: unknown command type: %d", cmd[0])
	}
}

func (s *EventStateMachine) Update(entries []statemachine.Entry) ([]statemachine.Entry, error) {
	batch := s.db.NewBatch()

	defer batch.Close() //nolint:errcheck

	var allDeletedKeys [][]byte
	lastApplied := s.lastApplied
	lastID := s.lastPersistedID

	for i := range entries {
		result, deletedKeys, entryMaxID, err := s.applyEntry(batch, entries[i].Cmd)
		if err != nil {
			return nil, err
		}
		entries[i].Result = result
		if len(deletedKeys) > 0 {
			allDeletedKeys = append(allDeletedKeys, deletedKeys...)
		}
		lastApplied = entries[i].Index
		if entryMaxID > lastID {
			lastID = entryMaxID
		}
	}

	if lastID > s.lastPersistedID {
		if err := s.repo.PersistLastID(batch, lastID); err != nil {
			return nil, fmt.Errorf("raft: PersistLastID: %w", err)
		}
	}
	idxBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(idxBytes, lastApplied)
	if err := batch.Set(appliedIndexKey, idxBytes); err != nil {
		return nil, err
	}

	if err := batch.Commit(storage.NoSync); err != nil {
		return nil, err
	}
	s.lastApplied = lastApplied
	s.lastPersistedID = lastID
	s.repo.ObserveID(lastID)

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

type eventSnapshot struct {
	iter  storage.Iterator
	index uint64
}

func (s *EventStateMachine) PrepareSnapshot() (interface{}, error) {
	iter, err := s.db.NewIter(nil)
	if err != nil {
		return nil, err
	}
	return &eventSnapshot{iter: iter, index: s.lastApplied}, nil
}

func (s *EventStateMachine) SaveSnapshot(ctx interface{}, w io.Writer, stopc <-chan struct{}) (err error) {
	snapshot, ok := ctx.(*eventSnapshot)
	if !ok || snapshot == nil || snapshot.iter == nil {
		return fmt.Errorf("raft: invalid snapshot context %T", ctx)
	}
	defer func() { err = errors.Join(err, snapshot.iter.Close()) }()

	var foundIndex bool
	for snapshot.iter.First(); snapshot.iter.Valid(); snapshot.iter.Next() {
		select {
		case <-stopc:
			return statemachine.ErrSnapshotStopped
		default:
		}

		key, value := snapshot.iter.Key(), snapshot.iter.Value()
		if bytes.Equal(key, appliedIndexKey) {
			if len(value) != 8 || binary.BigEndian.Uint64(value) != snapshot.index {
				return fmt.Errorf("raft: snapshot applied index does not match prepared index %d", snapshot.index)
			}
			foundIndex = true
		}
		if uint64(len(key)) > uint64(^uint32(0)) || uint64(len(value)) > uint64(^uint32(0)) {
			return fmt.Errorf("raft: snapshot record exceeds uint32 length")
		}
		if err := binary.Write(w, binary.LittleEndian, uint32(len(key))); err != nil {
			return err
		}
		if err := writeSnapshotBytes(w, key); err != nil {
			return err
		}
		if err := binary.Write(w, binary.LittleEndian, uint32(len(value))); err != nil {
			return err
		}
		if err := writeSnapshotBytes(w, value); err != nil {
			return err
		}
	}
	if err := snapshot.iter.Error(); err != nil {
		return err
	}
	if !foundIndex && snapshot.index != 0 {
		return fmt.Errorf("raft: snapshot missing applied index %d", snapshot.index)
	}
	return nil
}

func writeSnapshotBytes(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
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

	index, err := readStoredUint64(s.db, appliedIndexKey)
	if err != nil {
		return err
	}

	// The snapshot carries last-id; the repo loaded before recovery, so
	// observe it now.
	lastID, err := readStoredUint64(s.db, repository.LastIDKey)
	if err != nil {
		return err
	}
	s.lastApplied = index
	s.lastPersistedID = lastID
	s.repo.ObserveID(lastID)

	return nil
}

func (s *EventStateMachine) clearDB(stopc <-chan struct{}) error {
	iter, err := s.db.NewIter(nil)
	if err != nil {
		return err
	}
	iterClosed := false
	defer func() {
		if !iterClosed {
			_ = iter.Close()
		}
	}()

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
	closeErr := iter.Close()
	iterClosed = true
	if closeErr != nil {
		return closeErr
	}

	return batch.Commit(storage.NoSync)
}

func (s *EventStateMachine) Close() error {
	return s.db.Flush()
}
