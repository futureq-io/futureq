package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPebbleWALSyncConfiguration(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, LogDBTan, cfg.Cluster.Raft.LogDB.Backend)
	require.Equal(t, 500*time.Microsecond, cfg.Cluster.Raft.LogDB.PebbleWALMinSyncInterval)
	require.Equal(t, 500*time.Microsecond, cfg.Storage.Pebble.WALMinSyncInterval)

	t.Setenv("FUTUREQ_CLUSTER_ENABLED", "true")
	t.Setenv("FUTUREQ_API_GRPC_ADVERTISE", "127.0.0.1:8443")
	t.Setenv("FUTUREQ_CLUSTER_RAFT_ADVERTISE", "127.0.0.1:50005")
	t.Setenv("FUTUREQ_CLUSTER_RAFT_LOGDB_BACKEND", "pebble")
	t.Setenv("FUTUREQ_CLUSTER_RAFT_LOGDB_PEBBLEWALMINSYNCINTERVAL", "750us")
	t.Setenv("FUTUREQ_STORAGE_PEBBLE_WALMINSYNCINTERVAL", "0s")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, LogDBPebble, cfg.Cluster.Raft.LogDB.Backend)
	logDB, err := cfg.Cluster.Raft.LogDB.DragonboatConfig()
	require.NoError(t, err)
	require.Equal(t, 750*time.Microsecond, logDB.PebbleWALMinSyncInterval)
	require.Zero(t, cfg.Storage.Pebble.WALMinSyncInterval)

	for _, tc := range []struct {
		key, value, errContains string
	}{
		{"FUTUREQ_CLUSTER_RAFT_LOGDB_BACKEND", "bolt", "cluster.raft.logDB.backend"},
		{"FUTUREQ_CLUSTER_RAFT_LOGDB_PEBBLEWALMINSYNCINTERVAL", "-1us", "cluster.raft.logDB.pebbleWALMinSyncInterval"},
		{"FUTUREQ_STORAGE_PEBBLE_WALMINSYNCINTERVAL", "-1us", "storage.pebble.walMinSyncInterval"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			_, err := Load("")
			require.ErrorContains(t, err, tc.errContains)
		})
	}
}
