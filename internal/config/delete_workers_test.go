package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeleteWorkersConfiguration(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, 16, cfg.Delivery.DeleteWorkers)

	for _, workers := range []int{-1, 0, 1, 32, 256, 257} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("delivery:\n  deleteWorkers: %d\n", workers)), 0600))
			cfg, err := Load(path)
			if workers < 1 || workers > 256 {
				require.ErrorContains(t, err, "delivery.deleteWorkers")
			} else {
				require.NoError(t, err)
				require.Equal(t, workers, cfg.Delivery.DeleteWorkers)
			}
		})
	}
	t.Run("environment", func(t *testing.T) {
		t.Setenv("FUTUREQ_DELIVERY_DELETEWORKERS", "24")
		cfg, err := Load("")
		require.NoError(t, err)
		require.Equal(t, 24, cfg.Delivery.DeleteWorkers)
	})
}
