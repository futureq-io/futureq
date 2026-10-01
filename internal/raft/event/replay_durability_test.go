package raft

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func replayDurabilityEntries(t *testing.T) []statemachine.Entry {
	t.Helper()
	keyA, keyB := utils.EventKey(1, 2, 3), utils.EventKey(1, 2, 4)
	storeA, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("retained")}})
	require.NoError(t, err)
	prepareA, err := MarshalPrepareDeliveryCmd(keyA, 1, []string{"g:a", "g:b"})
	require.NoError(t, err)
	ackA, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: keyA, Recipient: "g:a"}})
	require.NoError(t, err)
	storeB, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 4, Bucket: 1, TopicHash: 2, Msg: []byte("completed")}})
	require.NoError(t, err)
	prepareB, err := MarshalPrepareDeliveryCmd(keyB, 1, []string{"g:a"})
	require.NoError(t, err)
	ackB, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: keyB, Recipient: "g:a"}})
	require.NoError(t, err)
	commands := [][]byte{storeA, prepareA, ackA, storeB, prepareB, ackB}
	entries := make([]statemachine.Entry, len(commands))
	for i, cmd := range commands {
		entries[i] = statemachine.Entry{Index: uint64(i + 1), Cmd: cmd}
	}
	return entries
}

func openReplayDurabilitySM(t *testing.T, dir string) (*EventStateMachine, storage.DB) {
	t.Helper()
	db, err := storage.NewPebble(config.Pebble{
		Mode: "disk", DataDir: dir, WALEnabled: false, MemtableSize: "64MiB",
	}, zap.NewNop())
	require.NoError(t, err)
	repo, err := repository.NewEventRepository(db, zap.NewNop(), time.Second)
	require.NoError(t, err)
	sm := NewEventStateMachineFactory(db, repo, nil, zap.NewNop())(1, 1).(*EventStateMachine)
	return sm, db
}

// This tests FutureQ's replay contract using a retained committed-entry slice.
// It does not test Dragonboat's log durability or simulate loss of OS caches.
func TestWALDisabledReplayAfterAbruptExit(t *testing.T) {
	if dir := os.Getenv("FUTUREQ_REPLAY_DURABILITY_DIR"); dir != "" {
		prefix, err := strconv.Atoi(os.Getenv("FUTUREQ_REPLAY_DURABILITY_PREFIX"))
		require.NoError(t, err)
		sm, _ := openReplayDurabilitySM(t, dir)
		index, err := sm.Open(nil)
		require.NoError(t, err)
		require.Zero(t, index)
		entries := replayDurabilityEntries(t)
		if prefix > 0 {
			_, err = sm.Update(entries[:prefix])
			require.NoError(t, err)
			require.NoError(t, sm.Sync())
		}
		if prefix < len(entries) {
			_, err = sm.Update(entries[prefix:])
			require.NoError(t, err)
		}
		// No Close or deferred cleanup: deliberately discard unflushed state.
		os.Exit(0)
	}

	executable, err := os.Executable()
	require.NoError(t, err)
	for _, prefix := range []int{0, 3, 6} {
		t.Run("flushed-prefix="+strconv.Itoa(prefix), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "events")
			child := exec.Command(executable, "-test.run=^TestWALDisabledReplayAfterAbruptExit$")
			child.Env = append(os.Environ(), "FUTUREQ_REPLAY_DURABILITY_DIR="+dir,
				"FUTUREQ_REPLAY_DURABILITY_PREFIX="+strconv.Itoa(prefix))
			output, err := child.CombinedOutput()
			require.NoError(t, err, "writer subprocess: %s", output)

			sm, db := openReplayDurabilitySM(t, dir)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			index, err := sm.Open(nil)
			require.NoError(t, err)
			require.Equal(t, uint64(prefix), index, "data and applied index must recover together")
			keyA, keyB := utils.EventKey(1, 2, 3), utils.EventKey(1, 2, 4)
			_, closer, getErr := db.Get(keyA)
			if prefix == 0 {
				require.ErrorIs(t, getErr, storage.ErrNotFound, "unflushed state must actually be lost in this test")
			} else {
				require.NoError(t, getErr)
				require.NoError(t, closer.Close())
			}
			_, _, err = db.Get(keyB)
			require.ErrorIs(t, err, storage.ErrNotFound)

			// Dragonboat uses Open's persisted index to select the missing suffix.
			entries := replayDurabilityEntries(t)
			if index < uint64(len(entries)) {
				_, err = sm.Update(entries[index:])
				require.NoError(t, err)
			}
			require.NoError(t, sm.Sync())
			require.Equal(t, uint64(6), sm.lastApplied)
			require.Equal(t, uint64(4), sm.lastPersistedID)
			require.Equal(t, uint64(5), sm.repo.NextID(), "replay must restore the event-ID high-water mark")
			value, closer, err := db.Get(keyA)
			require.NoError(t, err)
			require.Equal(t, []byte("retained"), value)
			require.NoError(t, closer.Close())
			state, err := ReadDeliveryState(db, keyA)
			require.NoError(t, err)
			require.NotNil(t, state)
			require.False(t, state.Needs("g:a"), "committed ACK must survive replay")
			require.True(t, state.Needs("g:b"), "pending recipient must retain its payload")
			_, _, err = db.Get(keyB)
			require.ErrorIs(t, err, storage.ErrNotFound, "completed payload must stay deleted")
			state, err = ReadDeliveryState(db, keyB)
			require.NoError(t, err)
			require.Nil(t, state, "completed delivery receipts must stay deleted")
		})
	}
}

func TestStateMachineSyncReturnsFlushError(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	flushErr := os.ErrPermission
	sm.db = snapshotFlushFailureDB{DB: db, err: flushErr}
	require.ErrorIs(t, sm.Sync(), flushErr)
}
