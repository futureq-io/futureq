package raft

import (
	"bytes"
	"errors"
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

// Exit the recovery process without Close: otherwise shutdown flushing could
// hide a snapshot recovery that returned before its data was durable.
func TestSnapshotRecoveryDurableWithoutClose(t *testing.T) {
	if dir := os.Getenv("FUTUREQ_SNAPSHOT_RECOVERY_DIR"); dir != "" {
		walEnabled, err := strconv.ParseBool(os.Getenv("FUTUREQ_SNAPSHOT_RECOVERY_WAL"))
		require.NoError(t, err)
		db, err := storage.NewPebble(config.Pebble{
			Mode: "disk", DataDir: filepath.Join(dir, "events"), WALEnabled: walEnabled,
		}, zap.NewNop())
		require.NoError(t, err)
		repo, err := repository.NewEventRepository(db, zap.NewNop(), time.Second)
		require.NoError(t, err)
		sm := NewEventStateMachineFactory(db, repo, nil, zap.NewNop())(1, 1)
		snapshot, err := os.ReadFile(filepath.Join(dir, "snapshot"))
		require.NoError(t, err)
		require.NoError(t, sm.RecoverFromSnapshot(bytes.NewReader(snapshot), nil))
		os.Exit(0)
	}

	sm, _ := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 3)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	prepare, err := MarshalPrepareDeliveryCmd(key, 1, []string{"g:a", "g:b"})
	require.NoError(t, err)
	ack, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:a"}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}, {Index: 3, Cmd: ack}})
	require.NoError(t, err)
	ctx, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(ctx, &snapshot, nil))
	executable, err := os.Executable()
	require.NoError(t, err)

	for _, walEnabled := range []bool{false, true} {
		t.Run("wal="+strconv.FormatBool(walEnabled), func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot"), snapshot.Bytes(), 0600))
			child := exec.Command(executable, "-test.run=^TestSnapshotRecoveryDurableWithoutClose$")
			child.Env = append(os.Environ(), "FUTUREQ_SNAPSHOT_RECOVERY_DIR="+dir,
				"FUTUREQ_SNAPSHOT_RECOVERY_WAL="+strconv.FormatBool(walEnabled))
			output, err := child.CombinedOutput()
			require.NoError(t, err, "recovery subprocess: %s", output)

			db, err := storage.NewPebble(config.Pebble{
				Mode: "disk", DataDir: filepath.Join(dir, "events"), WALEnabled: walEnabled,
			}, zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			repo, err := repository.NewEventRepository(db, zap.NewNop(), time.Second)
			require.NoError(t, err)
			reopened := NewEventStateMachineFactory(db, repo, nil, zap.NewNop())(1, 1)
			index, err := reopened.Open(nil)
			require.NoError(t, err)
			require.Equal(t, uint64(3), index)
			require.Equal(t, uint64(4), repo.NextID())
			value, closer, err := db.Get(key)
			require.NoError(t, err)
			require.Equal(t, []byte("payload"), value)
			require.NoError(t, closer.Close())
			state, err := ReadDeliveryState(db, key)
			require.NoError(t, err)
			require.NotNil(t, state)
			require.False(t, state.Needs("g:a"))
			require.True(t, state.Needs("g:b"))
		})
	}
}

type snapshotFlushFailureDB struct {
	storage.DB
	err error
}

func (db snapshotFlushFailureDB) Flush() error { return db.err }

func TestSnapshotRecoveryReturnsFlushError(t *testing.T) {
	source, _ := newDeliveryTestSM(t)
	cmd, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	_, err = source.Update([]statemachine.Entry{{Index: 5, Cmd: cmd}})
	require.NoError(t, err)
	ctx, err := source.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, source.SaveSnapshot(ctx, &snapshot, nil))

	recovered, db := newDeliveryTestSM(t)
	flushErr := errors.New("flush failed")
	recovered.db = snapshotFlushFailureDB{DB: db, err: flushErr}
	require.ErrorIs(t, recovered.RecoverFromSnapshot(&snapshot, nil), flushErr)
	require.Zero(t, recovered.lastApplied, "failed recovery must not advance the in-memory index")
	require.Zero(t, recovered.lastPersistedID)
	require.Equal(t, uint64(1), recovered.repo.NextID())
}
