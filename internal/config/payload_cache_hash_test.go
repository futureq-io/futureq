package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPayloadCacheHashConfiguration(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, PayloadCacheHashFNV, cfg.Storage.PayloadCacheHash)
	for _, algorithm := range []string{PayloadCacheHashFNV, PayloadCacheHashXXHash64, "xxhash128", "", "typo"} {
		t.Run(algorithm, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("storage:\n  payloadCacheHash: '"+algorithm+"'\n"), 0600))
			cfg, err := Load(path)
			if algorithm != PayloadCacheHashFNV && algorithm != PayloadCacheHashXXHash64 {
				require.ErrorContains(t, err, "storage.payloadCacheHash")
				return
			}
			require.NoError(t, err)
			require.Equal(t, algorithm, cfg.Storage.PayloadCacheHash)
		})
	}
}

func TestPayloadCacheHashEnvironmentOverride(t *testing.T) {
	t.Setenv("FUTUREQ_STORAGE_PAYLOADCACHEHASH", PayloadCacheHashXXHash64)
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, PayloadCacheHashXXHash64, cfg.Storage.PayloadCacheHash)
	t.Setenv("FUTUREQ_STORAGE_PAYLOADCACHEHASH", "invalid")
	_, err = Load("")
	require.ErrorContains(t, err, "storage.payloadCacheHash")
}
