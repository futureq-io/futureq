package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPayloadCacheSlotConfiguration(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, DefaultPayloadCacheSlots, cfg.Storage.PayloadCacheSlots)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("storage:\n  payloadCacheSlots: 16384\n"), 0600))
	cfg, err = Load(path)
	require.NoError(t, err)
	require.Equal(t, 16384, cfg.Storage.PayloadCacheSlots)
	t.Setenv("FUTUREQ_STORAGE_PAYLOADCACHESLOTS", "131072")
	cfg, err = Load(path)
	require.NoError(t, err)
	require.Equal(t, 131072, cfg.Storage.PayloadCacheSlots, "environment overrides YAML")
}

func TestPayloadCacheSlotBounds(t *testing.T) {
	for _, slots := range []int{-1, 0, 1, 63, 65, 4097, MaxPayloadCacheSlots + 1, 1 << 21} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			t.Setenv("FUTUREQ_STORAGE_PAYLOADCACHESLOTS", strconv.Itoa(slots))
			_, err := Load("")
			require.ErrorContains(t, err, "storage.payloadCacheSlots")
		})
	}
	for _, slots := range []int{MinPayloadCacheSlots, DefaultPayloadCacheSlots, 131072, MaxPayloadCacheSlots} {
		t.Run("valid-"+strconv.Itoa(slots), func(t *testing.T) {
			t.Setenv("FUTUREQ_STORAGE_PAYLOADCACHESLOTS", strconv.Itoa(slots))
			cfg, err := Load("")
			require.NoError(t, err)
			require.Equal(t, slots, cfg.Storage.PayloadCacheSlots)
		})
	}
}
