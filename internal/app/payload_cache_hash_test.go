package app

import (
	"path/filepath"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestInitPassesPayloadCacheHashToStorage(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, algorithm := range []string{config.PayloadCacheHashFNV, config.PayloadCacheHashXXHash64} {
			t.Run(engine+"/"+algorithm, func(t *testing.T) {
				cfg, err := config.Load("")
				require.NoError(t, err)
				cfg.Storage.Engine = engine
				cfg.Storage.Pebble.Mode = "memory"
				cfg.Storage.Bolt.File = filepath.Join(t.TempDir(), "events.db")
				cfg.Storage.PayloadCacheHash = algorithm
				previous := A
				a, err := Init(cfg, zap.NewNop())
				require.NoError(t, err)
				t.Cleanup(func() {
					a.cancel(nil)
					require.NoError(t, a.DB.Close())
					A = previous
				})
				// These keys collide under the default FNV hash, but not XXH64.
				key := []byte("012345678901234567890123")
				other := []byte("000000000000000000006617")
				mask := uint64(config.DefaultPayloadCacheSlots - 1)
				require.NotEqual(t, xxhash.Sum64(key)&mask, xxhash.Sum64(other)&mask)
				write := func(k []byte) {
					batch := a.DB.NewBatch()
					defer batch.Close() //nolint:errcheck
					require.NoError(t, batch.Set(k, []byte("value")))
					require.NoError(t, batch.Commit(storage.Sync))
				}
				write(key)
				_, closer, token, err := a.DB.(storage.CachedReader).GetWithCacheToken(key)
				require.NoError(t, err)
				require.NoError(t, closer.Close())
				require.True(t, token.Valid())
				write(other)
				require.Equal(t, algorithm == config.PayloadCacheHashXXHash64, token.Valid(), "startup must apply the configured hash")
			})
		}
	}
}
