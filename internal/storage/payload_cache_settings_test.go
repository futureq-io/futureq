package storage

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestConfiguredCacheSlotsTrackPayloadMutations(t *testing.T) {
	for _, slots := range []int{64, 4096, 131072, config.MaxPayloadCacheSlots} {
		for _, engine := range []string{"pebble", "bolt"} {
			t.Run(fmt.Sprintf("%s/%d", engine, slots), func(t *testing.T) {
				options := Options{PayloadCacheSlots: slots}
				var db DB
				var err error
				if engine == "pebble" {
					db, err = NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop(), options)
				} else {
					db, err = NewBoltDB(config.Bolt{File: filepath.Join(t.TempDir(), "events.db")}, options)
				}
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				key := []byte("012345678901234567890123")
				mutate := func(remove bool) {
					batch := db.NewBatch()
					if remove {
						require.NoError(t, batch.Delete(key))
					} else {
						require.NoError(t, batch.Set(key, []byte("value")))
					}
					require.NoError(t, batch.Commit(Sync))
					require.NoError(t, batch.Close())
				}
				readToken := func() CacheToken {
					iter, err := db.NewIter(nil)
					require.NoError(t, err)
					require.True(t, iter.First())
					token := iter.(CachedIterator).CacheToken(key)
					require.NoError(t, iter.Close())
					require.Len(t, token.versions.slots, slots)
					if slots > 65536 {
						require.Greater(t, token.slot, uint32(65535), "exercise a slot that cannot fit in uint16")
					}
					require.True(t, token.Valid())
					return token
				}
				mutate(false)
				token := readToken()
				mutate(false)
				require.False(t, token.Valid(), "same-value overwrites must invalidate old snapshots")
				token = readToken()
				mutate(true)
				require.False(t, token.Valid(), "deletes must invalidate cached data")
			})
		}
	}
}

func TestConfiguredCacheSlotsFenceOverlappingWrites(t *testing.T) {
	for _, slots := range []int{64, 4096, 131072, config.MaxPayloadCacheSlots} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			versions, err := newCacheVersions(Options{PayloadCacheSlots: slots})
			require.NoError(t, err)
			key := []byte("012345678901234567890123")
			var mutations cacheMutations
			mutations.add([]byte("receipt"), versions)
			require.Empty(t, mutations, "metadata-only batches allocate no bitmap")
			mutations.add(key, versions)
			mutations.add(key, versions)
			require.Len(t, mutations, slots/64)
			old := versions.snapshot().token(key)
			finishFirst := versions.begin(&mutations)
			finishSecond := versions.begin(&mutations)
			require.Equal(t, uint32(2), versions.pending[old.slot], "one pending count per batch, even with duplicate keys")
			overlap := versions.snapshot().token(key)
			require.False(t, overlap.Valid())
			finishFirst()
			require.False(t, versions.snapshot().token(key).Valid(), "second write remains pending")
			finishSecond()
			require.False(t, old.Valid())
			require.False(t, overlap.Valid(), "completion fences snapshots captured during the writes")
			require.True(t, versions.snapshot().token(key).Valid())
		})
	}
}

func TestLargerCacheSlotsAvoidConservativeCollision(t *testing.T) {
	// These keys share slot 4073 at 4096 slots, but use slots 106473 and 12265
	// at 131072 slots. The first slot also exercises the widened token index.
	key := []byte("012345678901234567890123")
	other := []byte("000000000000000000006617")
	for _, slots := range []int{4096, 131072} {
		versions, err := newCacheVersions(Options{PayloadCacheSlots: slots})
		require.NoError(t, err)
		token := versions.snapshot().token(key)
		var mutations cacheMutations
		mutations.add(other, versions)
		versions.begin(&mutations)()
		require.Equal(t, slots == 131072, token.Valid())
	}
}

func BenchmarkPayloadCacheTracking(b *testing.B) {
	for _, slots := range []int{4096, 16384, 65536, 131072, config.MaxPayloadCacheSlots} {
		b.Run(strconv.Itoa(slots), func(b *testing.B) {
			versions, err := newCacheVersions(Options{PayloadCacheSlots: slots})
			if err != nil {
				b.Fatal(err)
			}
			keys := make([][]byte, 64)
			for i := range keys {
				keys[i] = []byte(fmt.Sprintf("%024d", i))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var mutations cacheMutations
				for _, key := range keys {
					mutations.add(key, versions)
				}
				versions.begin(&mutations)()
			}
		})
	}
}
