package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAckNoSyncConfiguration(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.False(t, cfg.Delivery.AckNoSync)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("delivery:\n  ackNoSync: true\n"), 0600))
	cfg, err = Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Delivery.AckNoSync)
	t.Setenv("FUTUREQ_DELIVERY_ACKNOSYNC", "false")
	cfg, err = Load(path)
	require.NoError(t, err)
	require.False(t, cfg.Delivery.AckNoSync, "environment overrides the file")
	t.Setenv("FUTUREQ_DELIVERY_ACKNOSYNC", "true")
	cfg, err = Load("")
	require.NoError(t, err)
	require.True(t, cfg.Delivery.AckNoSync, "env-only configuration must be bound")
	t.Setenv("FUTUREQ_DELIVERY_ACKNOSYNC", "invalid")
	_, err = Load("")
	require.Error(t, err)
}

func TestAckNoSyncRejectsUnsupportedModes(t *testing.T) {
	for _, settings := range []string{
		"storage:\n  engine: bolt\n",
		"cluster:\n  enabled: true\n  raft:\n    advertise: 'node1.example:50005'\napi:\n  grpc:\n    advertise: 'node1.example:8443'\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(settings+"delivery:\n  ackNoSync: true\n"), 0600))
		_, err := Load(path)
		require.ErrorContains(t, err, "delivery.ackNoSync requires standalone mode with storage.engine pebble")
	}
}
