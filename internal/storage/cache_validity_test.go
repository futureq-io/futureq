package storage

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCacheValidityTracksCommittedMutationsAndOldSnapshots(t *testing.T) {
	for name, factory := range map[string]EngineFactory{"pebble": newPebbleEngine, "bolt": newBoltEngine} {
		t.Run(name, func(t *testing.T) {
			db := factory(t)
			defer db.Close()
			key := []byte("012345678901234567890123")
			batch := db.NewBatch()
			require.NoError(t, batch.Set(key, []byte("old")))
			require.NoError(t, batch.Commit(Sync))
			require.NoError(t, batch.Close())
			iter, err := db.NewIter(nil)
			require.NoError(t, err)
			require.True(t, iter.First())
			token := iter.(CachedIterator).CacheToken(key)
			require.True(t, token.Valid())
			require.NoError(t, iter.Close())
			// Receipt changes must not invalidate payloads.
			batch = db.NewBatch()
			require.NoError(t, batch.Set([]byte("metadata/delivery/test"), []byte("receipt")))
			require.NoError(t, batch.Commit(Sync))
			require.NoError(t, batch.Close())
			require.True(t, token.Valid())
			// Staging is not a commit. Overwrites invalidate, including replay/restore
			// with identical bytes, without depending on asynchronous callbacks.
			batch = db.NewBatch()
			require.NoError(t, batch.Set(key, []byte("new")))
			require.True(t, token.Valid())
			require.NoError(t, batch.Commit(Sync))
			require.NoError(t, batch.Close())
			require.False(t, token.Valid())
			iter, err = db.NewIter(nil)
			require.NoError(t, err)
			require.True(t, iter.First())
			token = iter.(CachedIterator).CacheToken(key)
			require.True(t, token.Valid())
			require.NoError(t, iter.Close())
			batch = db.NewBatch()
			require.NoError(t, batch.Delete(key))
			require.NoError(t, batch.Commit(Sync))
			require.NoError(t, batch.Close())
			require.False(t, token.Valid())
		})
	}
}

func TestCacheValidityRejectsPendingAndFailedCommitSnapshots(t *testing.T) {
	versions, err := newCacheVersions()
	require.NoError(t, err)
	key := []byte("012345678901234567890123")
	old := versions.snapshot()
	var mutations cacheMutations
	mutations.add(key, versions)
	done := versions.begin(&mutations)
	require.False(t, old.token(key).Valid(), "invalidate before storage work begins")
	require.False(t, versions.snapshot().token(key).Valid(), "no proof can be issued for a key racing a pending commit")
	other := []byte("112345678901234567890123")
	require.NotEqual(t, cacheSlot(key, versions.mask), cacheSlot(other, versions.mask))
	require.True(t, versions.snapshot().token(other).Valid(), "unrelated writes do not disable caching")
	overlapping := versions.snapshot()
	done() // even if the storage commit failed, old proofs stay invalid
	require.False(t, overlapping.token(key).Valid(), "completion must invalidate a snapshot captured during commit")
	require.False(t, old.token(key).Valid())
	require.True(t, versions.snapshot().token(key).Valid())
	require.False(t, CacheToken{}.Valid())
}
