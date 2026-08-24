package raft

import (
	"bytes"
	"encoding/binary"
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
		{Bucket: 1, TopicHash: 100, Msg: []byte("msg-a")},
		{Bucket: 2, TopicHash: 100, Msg: []byte("msg-b")},
	}
	cmd, err := MarshalStoreBatchCmd(items)
	require.NoError(err)

	entries := []statemachine.Entry{{Index: 1, Cmd: cmd}}
	results, err := sm.Update(entries)
	require.NoError(err)
	require.Len(results, 1)
	require.Equal(uint64(2), results[0].Result.Value, "should report 2 items applied")

	// Verify items are in storage via the repo's last-ID counter.
	// Two items stored → repo's lastID should be 2.
	// We verify indirectly by storing one more and checking its key's eventID.
	b := s.db.NewBatch()
	key, err := s.repo.StoreRawWithBatch(b, 1, 100, nil, []byte("third"))
	require.NoError(err)
	require.NoError(b.Commit(storage.Sync))
	require.NoError(b.Close())

	// The 3rd store should have eventID=3 (since 1,2 were used).
	_, _, id, err := parseKeyEventID(key)
	require.NoError(err)
	require.Equal(uint64(3), id)
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

func (s *EventStateMachineSuite) TestUpdate_UnknownCmd_ReturnsZero() {
	require := s.Require()

	sm := s.newSM(nil)

	entries := []statemachine.Entry{{Index: 1, Cmd: []byte{0xFF, 0x01}}}
	results, err := sm.Update(entries)
	require.NoError(err)
	require.Equal(uint64(0), results[0].Result.Value)
}

// ─── Update: DeleteBatchCmd ───────────────────────────────────────────────────

func (s *EventStateMachineSuite) TestUpdate_DeleteBatch_RemovesKeys() {
	require := s.Require()

	sm := s.newSM(nil)

	// First store a message.
	items := []StoreBatchItem{{Bucket: 1, TopicHash: 5, Msg: []byte("to-delete")}}
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

	items := []StoreBatchItem{{Bucket: 1, TopicHash: 1, Msg: []byte("m")}}
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

	items := []StoreBatchItem{{Bucket: 1, TopicHash: 1, Msg: []byte("m")}}
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

func (s *EventStateMachineSuite) TestPrepareSnapshot_ReturnsLastApplied() {
	require := s.Require()

	sm := s.newSM(nil)
	result, err := sm.PrepareSnapshot()
	require.NoError(err)
	require.Equal(uint64(0), result) // fresh SM
}

// ─── Snapshot round-trip ──────────────────────────────────────────────────────

// TestSnapshot_RoundTrip verifies that SaveSnapshot + RecoverFromSnapshot
// preserve both the data records and the applied-index metadata.
func (s *EventStateMachineSuite) TestSnapshot_RoundTrip() {
	require := s.Require()

	sm := s.newSM(nil)

	// Seed data: applies one StoreBatchCmd at raft index 5.
	items := []StoreBatchItem{
		{Bucket: 1, TopicHash: 7, Msg: []byte("snap-msg")},
	}
	cmd, _ := MarshalStoreBatchCmd(items)
	_, err := sm.Update([]statemachine.Entry{{Index: 5, Cmd: cmd}})
	require.NoError(err)

	// Save snapshot.
	var buf bytes.Buffer
	err = sm.SaveSnapshot(nil, &buf, nil)
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
}

// ─── Close ────────────────────────────────────────────────────────────────────

func (s *EventStateMachineSuite) TestClose_NoError() {
	require := s.Require()

	sm := s.newSM(nil)
	require.NoError(sm.Close())
}
