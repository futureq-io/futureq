package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

type EventStateMachineSuite struct {
	suite.Suite
	db   storage.DB
	repo *repository.EventRepository
}

func TestEventStateMachineSuite(t *testing.T) {
	suite.Run(t, new(EventStateMachineSuite))
}

func (s *EventStateMachineSuite) SetupTest() {
	db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	s.Require().NoError(err)
	s.db = db

	repo, err := repository.NewEventRepository(db, zap.NewNop(), 1*time.Second)
	s.Require().NoError(err)
	s.repo = repo
}

func (s *EventStateMachineSuite) TearDownTest() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *EventStateMachineSuite) newSM(onDelete func([][]byte)) *EventStateMachine {
	factory := NewEventStateMachineFactory(s.db, s.repo, onDelete, zap.NewNop())
	sm, _ := factory(1, 1).(*EventStateMachine)
	return sm
}

// ─── Open ─────────────────────────────────────────────────────────────────────

func (s *EventStateMachineSuite) TestOpen_FreshDB_ReturnsZero() {
	require := s.Require()

	sm := s.newSM(nil)

	idx, err := sm.Open(nil)
	require.NoError(err)
	require.Equal(uint64(0), idx)
}

func (s *EventStateMachineSuite) TestOpen_RestoresAppliedIndex() {
	require := s.Require()

	// Seed applied index into DB.
	b := s.db.NewBatch()
	idxBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(idxBytes, 12345)
	require.NoError(b.Set(appliedIndexKey, idxBytes))
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	sm := s.newSM(nil)
	idx, err := sm.Open(nil)
	require.NoError(err)
	require.Equal(uint64(12345), idx)
}

// ─── Update: StoreBatchCmd ────────────────────────────────────────────────────

func (s *EventStateMachineSuite) TestUpdate_StoreBatch_AppliesItems() {
	require := s.Require()

	sm := s.newSM(nil)

	items := []StoreBatchItem{
		{ID: 1, Bucket: 1, TopicHash: 100, Msg: []byte("msg-a")},
		{ID: 2, Bucket: 2, TopicHash: 100, Msg: []byte("msg-b")},
	}
	cmd, err := MarshalStoreBatchCmd(items)
	require.NoError(err)

	entries := []statemachine.Entry{{Index: 1, Cmd: cmd}}
	results, err := sm.Update(entries)
	require.NoError(err)
	require.Len(results, 1)
	require.Equal(uint64(2), results[0].Result.Value, "should report 2 items applied")

	// Keys must use the leader-assigned IDs from the command verbatim.
	var foundIDs []uint64
	err = s.db.Scan(nil, func(k, v []byte) error {
		if len(k) == 24 {
			_, _, id, err := parseKeyEventID(k)
			require.NoError(err)
			foundIDs = append(foundIDs, id)
		}
		return nil
	})
	require.NoError(err)
	require.ElementsMatch([]uint64{1, 2}, foundIDs)

	// Applied IDs must be observed: next reserved ID must be past them.
	require.Equal(uint64(3), s.repo.NextID())
}

// Helper to parse eventID from a key — small local helper to avoid circular import.
func parseKeyEventID(key []byte) (uint64, uint64, uint64, error) {
	if len(key) != 24 {
		return 0, 0, 0, bytes.ErrTooLarge
	}
	topicHash := binary.BigEndian.Uint64(key[0:8])
	bucket := binary.BigEndian.Uint64(key[8:16])
	eventID := binary.BigEndian.Uint64(key[16:24])
	return topicHash, bucket, eventID, nil
}

func (s *EventStateMachineSuite) TestUpdate_EmptyCmd_Skipped() {
	require := s.Require()

	sm := s.newSM(nil)

	entries := []statemachine.Entry{{Index: 1, Cmd: []byte{}}}
	results, err := sm.Update(entries)
	require.NoError(err)
	require.Len(results, 1)
	require.Equal(uint64(0), results[0].Result.Value)
}

func (s *EventStateMachineSuite) TestUpdate_InvalidCommandAbortsWholeBatch() {
	require := s.Require()
	var deleted [][]byte
	sm := s.newSM(func(keys [][]byte) { deleted = append(deleted, keys...) })
	store := func(id uint64) []byte {
		cmd, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: id, Bucket: 1, TopicHash: 1, Msg: []byte("m")}})
		require.NoError(err)
		return cmd
	}
	_, err := sm.Update([]statemachine.Entry{{Index: 1, Cmd: store(1)}})
	require.NoError(err)

	var storedKey []byte
	require.NoError(s.db.Scan(nil, func(key, _ []byte) error {
		if len(key) == 24 {
			storedKey = append([]byte(nil), key...)
		}
		return nil
	}))
	require.Len(storedKey, 24)
	deleteCmd, err := MarshalDeleteBatchCmd([][]byte{storedKey})
	require.NoError(err)

	for _, tc := range []struct {
		name string
		cmd  []byte
	}{
		{name: "truncated store", cmd: []byte{byte(StoreBatchCmd), 1}},
		{name: "truncated delete", cmd: []byte{byte(DeleteBatchCmd), 1}},
		{name: "unknown type", cmd: []byte{0xff, 1}},
	} {
		s.Run(tc.name, func() {
			_, err := sm.Update([]statemachine.Entry{
				{Index: 2, Cmd: store(2)},
				{Index: 3, Cmd: deleteCmd},
				{Index: 4, Cmd: tc.cmd},
			})
			require.Error(err)
			require.Equal(uint64(1), sm.lastApplied)
			require.Equal(uint64(1), sm.lastPersistedID)
			require.Empty(deleted)

			for _, key := range [][]byte{appliedIndexKey, repository.LastIDKey} {
				value, closer, err := s.db.Get(key)
				require.NoError(err)
				require.Equal(uint64(1), binary.BigEndian.Uint64(value))
				require.NoError(closer.Close())
			}
			var ids []uint64
			require.NoError(s.db.Scan(nil, func(key, _ []byte) error {
				if len(key) == 24 {
					ids = append(ids, binary.BigEndian.Uint64(key[16:24]))
				}
				return nil
			}))
			require.Equal([]uint64{1}, ids)
		})
	}
	require.Equal(uint64(2), s.repo.NextID())
}

// ─── Update: DeleteBatchCmd ───────────────────────────────────────────────────

func (s *EventStateMachineSuite) TestUpdate_DeleteBatch_RemovesKeys() {
	require := s.Require()

	sm := s.newSM(nil)

	// First store a message.
	items := []StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 5, Msg: []byte("to-delete")}}
	storeCmd, _ := MarshalStoreBatchCmd(items)
	_, err := sm.Update([]statemachine.Entry{{Index: 1, Cmd: storeCmd}})
	require.NoError(err)

	// Find the stored key.
	var storedKey []byte
	err = s.db.Scan(nil, func(k, v []byte) error {
		if len(k) == 24 {
			storedKey = append([]byte(nil), k...)
		}
		return nil
	})
	require.NoError(err)
	require.NotNil(storedKey)

	// Now delete it.
	deleteCmd, err := MarshalDeleteBatchCmd([][]byte{storedKey})
	require.NoError(err)

	results, err := sm.Update([]statemachine.Entry{{Index: 2, Cmd: deleteCmd}})
	require.NoError(err)
	require.Equal(uint64(1), results[0].Result.Value, "should report 1 key deleted")

	// Verify it's gone.
	_, _, err = s.db.Get(storedKey)
	require.Error(err)
}

func (s *EventStateMachineSuite) TestUpdate_DeleteBatch_CallsOnDeleteKeys() {
	require := s.Require()

	var captured [][]byte
	sm := s.newSM(func(keys [][]byte) { captured = keys })

	key1 := make([]byte, 24)
	key1[0] = 0x01
	key2 := make([]byte, 24)
	key2[0] = 0x02

	deleteCmd, _ := MarshalDeleteBatchCmd([][]byte{key1, key2})
	_, err := sm.Update([]statemachine.Entry{{Index: 1, Cmd: deleteCmd}})
	require.NoError(err)

	require.Len(captured, 2)
}

func (s *EventStateMachineSuite) TestUpdate_NoDeleteCallback_DoesNotPanic() {
	require := s.Require()

	sm := s.newSM(nil) // nil OnDeleteKeys

	key := make([]byte, 24)
	deleteCmd, _ := MarshalDeleteBatchCmd([][]byte{key})
	_, err := sm.Update([]statemachine.Entry{{Index: 1, Cmd: deleteCmd}})
	require.NoError(err) // must not panic
}

// ─── Update: lastApplied tracking ─────────────────────────────────────────────

func (s *EventStateMachineSuite) TestUpdate_PersistsAppliedIndex() {
	require := s.Require()

	sm := s.newSM(nil)

	items := []StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 1, Msg: []byte("m")}}
	cmd, _ := MarshalStoreBatchCmd(items)

	_, err := sm.Update([]statemachine.Entry{{Index: 42, Cmd: cmd}})
	require.NoError(err)

	// Read the applied index back from storage.
	val, closer, err := s.db.Get(appliedIndexKey)
	require.NoError(err)
	defer closer.Close()
	require.Equal(uint64(42), binary.BigEndian.Uint64(val))
}

func (s *EventStateMachineSuite) TestUpdate_MultipleEntries_AdvancesAppliedIndex() {
	require := s.Require()

	sm := s.newSM(nil)

	items := []StoreBatchItem{{ID: 1, Bucket: 1, TopicHash: 1, Msg: []byte("m")}}
	cmd, _ := MarshalStoreBatchCmd(items)

	entries := []statemachine.Entry{
		{Index: 10, Cmd: cmd},
		{Index: 11, Cmd: cmd},
		{Index: 12, Cmd: cmd},
	}
	_, err := sm.Update(entries)
	require.NoError(err)

	val, closer, err := s.db.Get(appliedIndexKey)
	require.NoError(err)
	defer closer.Close()
	require.Equal(uint64(12), binary.BigEndian.Uint64(val))
}

func (s *EventStateMachineSuite) TestUpdate_OutOfOrderIDsKeepDurableMaximum() {
	require := s.Require()
	sm := s.newSM(nil)
	store := func(id uint64) []byte {
		cmd, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: id, Bucket: 1, TopicHash: 1, Msg: []byte("m")}})
		require.NoError(err)
		return cmd
	}

	_, err := sm.Update([]statemachine.Entry{
		{Index: 1, Cmd: store(20)},
		{Index: 2, Cmd: store(10)},
	})
	require.NoError(err)
	_, err = sm.Update([]statemachine.Entry{{Index: 3, Cmd: store(9)}})
	require.NoError(err)

	val, closer, err := s.db.Get(repository.LastIDKey)
	require.NoError(err)
	require.Equal(uint64(20), binary.BigEndian.Uint64(val))
	require.NoError(closer.Close())

	restartedRepo, err := repository.NewEventRepository(s.db, zap.NewNop(), time.Second)
	require.NoError(err)
	restartedSM, ok := NewEventStateMachineFactory(s.db, restartedRepo, nil, zap.NewNop())(1, 1).(*EventStateMachine)
	require.True(ok)
	index, err := restartedSM.Open(nil)
	require.NoError(err)
	require.Equal(uint64(3), index)
	require.Equal(uint64(20), restartedSM.lastPersistedID)
	require.Equal(uint64(21), restartedRepo.NextID())
}

type failingCommitDB struct{ storage.DB }
type failingCommitBatch struct{ storage.Batch }

func (db failingCommitDB) NewBatch() storage.Batch {
	return failingCommitBatch{Batch: db.DB.NewBatch()}
}

func (failingCommitBatch) Commit(storage.SyncMode) error { return errors.New("commit failed") }

func (s *EventStateMachineSuite) TestUpdate_FailedCommitDoesNotAdvanceWatermarks() {
	require := s.Require()
	db := failingCommitDB{DB: s.db}
	sm := &EventStateMachine{db: db, repo: s.repo}
	cmd, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 20, Bucket: 1, TopicHash: 1, Msg: []byte("m")}})
	require.NoError(err)
	_, err = sm.Update([]statemachine.Entry{{Index: 5, Cmd: cmd}})
	require.ErrorContains(err, "commit failed")
	require.Zero(sm.lastApplied)
	require.Zero(sm.lastPersistedID)
	require.Equal(uint64(1), s.repo.NextID())
}

// ─── Sync / Lookup / PrepareSnapshot ─────────────────────────────────────────

func (s *EventStateMachineSuite) TestSync_NoError() {
	require := s.Require()

	sm := s.newSM(nil)
	require.NoError(sm.Sync())
}

func (s *EventStateMachineSuite) TestLookup_ReturnsNil() {
	require := s.Require()

	sm := s.newSM(nil)
	result, err := sm.Lookup(nil)
	require.NoError(err)
	require.Nil(result)
}

func (s *EventStateMachineSuite) TestPrepareSnapshot_CapturesLastApplied() {
	require := s.Require()

	sm := s.newSM(nil)
	ctx, err := sm.PrepareSnapshot()
	require.NoError(err)
	snapshot, ok := ctx.(*eventSnapshot)
	require.True(ok)
	require.Equal(uint64(0), snapshot.index)
	require.NoError(snapshot.iter.Close())
}

// ─── Snapshot round-trip ──────────────────────────────────────────────────────

// TestSnapshot_RoundTrip verifies that SaveSnapshot + RecoverFromSnapshot
// preserve both the data records and the applied-index metadata.
func (s *EventStateMachineSuite) TestSnapshot_RoundTrip() {
	require := s.Require()

	sm := s.newSM(nil)

	// Seed data: applies one StoreBatchCmd at raft index 5.
	items := []StoreBatchItem{
		{ID: 1, Bucket: 1, TopicHash: 7, Msg: []byte("snap-msg")},
	}
	cmd, _ := MarshalStoreBatchCmd(items)
	_, err := sm.Update([]statemachine.Entry{{Index: 5, Cmd: cmd}})
	require.NoError(err)

	// Save snapshot.
	var buf bytes.Buffer
	ctx, err := sm.PrepareSnapshot()
	require.NoError(err)
	err = sm.SaveSnapshot(ctx, &buf, nil)
	require.NoError(err)
	require.NotZero(buf.Len(), "snapshot must not be empty")

	appliedKeyBytes := []byte("metadata/raft/applied-index")
	require.True(bytes.Contains(buf.Bytes(), appliedKeyBytes),
		"snapshot must faithfully serialize the applied-index key")

	// Recover into a fresh DB.
	db2, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	require.NoError(err)
	defer db2.Close()

	repo2, err := repository.NewEventRepository(db2, zap.NewNop(), 1*time.Second)
	require.NoError(err)

	factory2 := NewEventStateMachineFactory(db2, repo2, nil, zap.NewNop())
	sm2, _ := factory2(1, 1).(*EventStateMachine)

	err = sm2.RecoverFromSnapshot(&buf, nil)
	require.NoError(err)

	// Applied index must be restored.
	val, closer, err := db2.Get(appliedIndexKey)
	require.NoError(err)
	defer closer.Close()
	require.Equal(uint64(5), binary.BigEndian.Uint64(val),
		"recovered DB must contain the same applied index as was saved")

	// The stored message bytes must also be recoverable.
	var found []byte
	require.NoError(db2.Scan(nil, func(k, v []byte) error {
		if len(k) == 24 {
			found = append([]byte(nil), v...)
		}
		return nil
	}))
	require.Equal([]byte("snap-msg"), found,
		"recovered DB must contain the same message bytes as was saved")

	// The high-water mark must survive recovery: the repo attached to the
	// recovered SM was constructed before recovery, so recovery itself must
	// have observed the restored last-id. A new ID must not collide with 1.
	require.Equal(uint64(2), repo2.NextID())
}

func TestSnapshot_PrepareFreezesAppliedState(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			newDB := func() storage.DB {
				t.Helper()
				if engine == "bolt" {
					db, err := storage.NewBoltDB(config.Bolt{DataPath: filepath.Join(t.TempDir(), "events.db")})
					if err != nil {
						t.Fatal(err)
					}
					return db
				}
				db, err := storage.NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
				if err != nil {
					t.Fatal(err)
				}
				return db
			}
			db := newDB()
			defer db.Close()
			repo, err := repository.NewEventRepository(db, zap.NewNop(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			sm := &EventStateMachine{db: db, repo: repo}
			store := func(index, id uint64) {
				t.Helper()
				cmd, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: id, Bucket: 1, TopicHash: 1, Msg: []byte{byte(id)}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sm.Update([]statemachine.Entry{{Index: index, Cmd: cmd}}); err != nil {
					t.Fatal(err)
				}
			}
			store(5, 1)
			ctx, err := sm.PrepareSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			cmd, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 2, Bucket: 1, TopicHash: 1, Msg: []byte{2}}})
			if err != nil {
				t.Fatal(err)
			}
			updateDone := make(chan error, 1)
			go func() {
				_, err := sm.Update([]statemachine.Entry{{Index: 6, Cmd: cmd}})
				updateDone <- err
			}()
			if engine == "pebble" {
				if err := <-updateDone; err != nil {
					t.Fatal(err)
				}
			}
			var snapshot bytes.Buffer
			if err := sm.SaveSnapshot(ctx, &snapshot, nil); err != nil {
				t.Fatal(err)
			}
			if engine == "bolt" {
				if err := <-updateDone; err != nil {
					t.Fatal(err)
				}
			}

			recoveredDB := newDB()
			defer recoveredDB.Close()
			recoveredRepo, err := repository.NewEventRepository(recoveredDB, zap.NewNop(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			recovered := &EventStateMachine{db: recoveredDB, repo: recoveredRepo}
			if err := recovered.RecoverFromSnapshot(&snapshot, nil); err != nil {
				t.Fatal(err)
			}
			if recovered.lastApplied != 5 || recovered.lastPersistedID != 1 {
				t.Fatalf("recovered watermarks: index=%d id=%d, want 5 and 1", recovered.lastApplied, recovered.lastPersistedID)
			}
			var foundIDs []uint64
			if err := recoveredDB.Scan(nil, func(key, _ []byte) error {
				if len(key) == 24 {
					foundIDs = append(foundIDs, binary.BigEndian.Uint64(key[16:24]))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(foundIDs) != 1 || foundIDs[0] != 1 {
				t.Fatalf("recovered event IDs = %v, want [1]", foundIDs)
			}
		})
	}
}

type trackingIterator struct {
	storage.Iterator
	closed bool
}

func (it *trackingIterator) Close() error {
	it.closed = true
	return it.Iterator.Close()
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func (s *EventStateMachineSuite) TestSaveSnapshot_ClosesIteratorOnFailure() {
	require := s.Require()
	sm := s.newSM(nil)
	_, err := sm.Update([]statemachine.Entry{{Index: 1}})
	require.NoError(err)
	ctx, err := sm.PrepareSnapshot()
	require.NoError(err)
	snapshot := ctx.(*eventSnapshot)
	tracked := &trackingIterator{Iterator: snapshot.iter}
	snapshot.iter = tracked
	require.ErrorIs(sm.SaveSnapshot(snapshot, failingWriter{}, nil), io.ErrClosedPipe)
	require.True(tracked.closed)

	ctx, err = sm.PrepareSnapshot()
	require.NoError(err)
	snapshot = ctx.(*eventSnapshot)
	tracked = &trackingIterator{Iterator: snapshot.iter}
	snapshot.iter = tracked
	stop := make(chan struct{})
	close(stop)
	require.ErrorIs(sm.SaveSnapshot(snapshot, io.Discard, stop), statemachine.ErrSnapshotStopped)
	require.True(tracked.closed)
}

// ─── Close ────────────────────────────────────────────────────────────────────

func (s *EventStateMachineSuite) TestClose_NoError() {
	require := s.Require()

	sm := s.newSM(nil)
	require.NoError(sm.Close())
}
