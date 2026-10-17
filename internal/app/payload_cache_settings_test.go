package app

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestInitUsesConfiguredPayloadCacheSlots(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, clustered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/clustered=%t", engine, clustered), func(t *testing.T) {
				cfg, err := config.Load("")
				require.NoError(t, err)
				cfg.Storage.Engine = engine
				cfg.Storage.PayloadCacheSlots = 131072
				cfg.Storage.Pebble.Mode = "memory"
				cfg.Storage.Bolt.File = filepath.Join(t.TempDir(), "events.db")
				cfg.Cluster.Enabled = clustered
				previous := A
				a, err := Init(cfg, zap.NewNop())
				require.NoError(t, err)
				t.Cleanup(func() {
					a.cancel(context.Canceled)
					require.NoError(t, a.DB.Close())
					A = previous
				})
				key := []byte("012345678901234567890123")
				write := func(key []byte) {
					batch := a.DB.NewBatch()
					require.NoError(t, batch.Set(key, []byte("payload")))
					require.NoError(t, batch.Commit(storage.Sync))
					require.NoError(t, batch.Close())
				}
				write(key)
				iter, err := a.DB.NewIter(nil)
				require.NoError(t, err)
				require.True(t, iter.First())
				token := iter.(storage.CachedIterator).CacheToken(key)
				require.NoError(t, iter.Close())
				// This key collides with the cached key at the default count, but
				// not at 131072. A valid token proves startup passed the setting.
				write([]byte("000000000000000000006617"))
				require.True(t, token.Valid())
				write(key)
				require.False(t, token.Valid())
			})
		}
	}
}
