package app

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRaftLogDBRestart(t *testing.T) {
	for _, backend := range []string{config.LogDBTan, config.LogDBPebble} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			addr := listener.Addr().String()
			require.NoError(t, listener.Close())

			cfg, err := config.Load("")
			require.NoError(t, err)
			cfg.API.GRPC.Advertise = "127.0.0.1:8443"
			cfg.Cluster.Enabled = true
			cfg.Cluster.Raft.Listen = addr
			cfg.Cluster.Raft.Advertise = addr
			cfg.Cluster.Raft.DataDir = filepath.Join(root, "raft")
			cfg.Cluster.Raft.InitialMembers = map[uint64]string{cfg.Cluster.NodeID: addr}
			cfg.Cluster.Raft.RTT = 10 * time.Millisecond
			cfg.Cluster.Raft.LogDB.Backend = backend
			cfg.Cluster.Raft.LogDB.Workers = 2
			cfg.Cluster.Raft.LogDB.Shards = 2
			cfg.Cluster.Raft.LogDB.KVWriteBufferSize = "4MiB"
			cfg.Storage.Pebble.DataDir = filepath.Join(root, "events")

			start := func(t *testing.T, restarting bool) {
				previous := A
				a, err := Init(cfg, zap.NewNop())
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(a.Ctx, 10*time.Second)
				a.Ctx = ctx
				t.Cleanup(cancel)
				t.Cleanup(func() {
					a.cancel(nil)
					a.wg.Wait()
					if a.NodeHost != nil {
						a.NodeHost.Close()
					}
					require.NoError(t, a.DB.Close())
					A = previous
				})
				require.NoError(t, a.WithRepositories())
				require.NoError(t, a.OpenRaft())
				require.Equal(t, restarting, a.HasRaftData())
				require.Equal(t, backend == config.LogDBTan, a.LeaderTracker != nil)
				require.NoError(t, a.StartRaft(false, nil, nil, nil))
				require.True(t, a.HasRaftData())
			}

			t.Run("bootstrap", func(t *testing.T) { start(t, false) })
			cfg.Cluster.Raft.InitialMembers = nil
			t.Run("refuse backend change", func(t *testing.T) {
				switched := *cfg
				switched.Cluster.Raft.LogDB.Backend = config.LogDBPebble
				if backend == config.LogDBPebble {
					switched.Cluster.Raft.LogDB.Backend = config.LogDBTan
				}
				a := &App{cfg: &switched, Logger: zap.NewNop()}
				require.ErrorContains(t, a.OpenRaft(), "logdb type changed")
			})
			t.Run("restart", func(t *testing.T) { start(t, true) })
		})
	}
}
