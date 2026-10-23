package storage

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"path/filepath"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPayloadCacheHashEngines(t *testing.T) {
	for _, algorithm := range []string{"", config.PayloadCacheHashFNV, config.PayloadCacheHashXXHash64} {
		for _, engine := range []string{"pebble", "bolt"} {
			t.Run(engine+"/"+algorithm, func(t *testing.T) {
				options := Options{PayloadCacheHash: algorithm}
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
				write := func(remove bool) {
					batch := db.NewBatch()
					defer batch.Close() //nolint:errcheck
					if remove {
						require.NoError(t, batch.Delete(key))
					} else {
						require.NoError(t, batch.Set(key, []byte("value")))
					}
					require.NoError(t, batch.Commit(Sync))
				}
				write(false)
				iter, err := db.NewIter(nil)
				require.NoError(t, err)
				require.True(t, iter.First())
				snapshot := iter.(CachedIterator).CacheToken(key)
				require.NoError(t, iter.Close())
				value, closer, read, err := db.(CachedReader).GetWithCacheToken(key)
				require.NoError(t, err)
				require.Equal(t, "value", string(value))
				require.NoError(t, closer.Close())
				want := uint32(4073) // existing FNV-1a assignment, retained by default
				if algorithm == config.PayloadCacheHashXXHash64 {
					want = uint32(xxhash.Sum64(key)) & (config.DefaultPayloadCacheSlots - 1)
				}
				require.Equal(t, want, snapshot.slot)
				require.Equal(t, want, read.slot)
				require.True(t, snapshot.Valid())
				require.True(t, read.Valid())
				write(false)
				require.False(t, snapshot.Valid(), "overwrite invalidates iterator proofs")
				require.False(t, read.Valid(), "overwrite invalidates Get proofs")
				_, closer, read, err = db.(CachedReader).GetWithCacheToken(key)
				require.NoError(t, err)
				require.NoError(t, closer.Close())
				require.True(t, read.Valid())
				write(true)
				require.False(t, read.Valid(), "delete invalidates refreshed proofs")
			})
		}
	}
}

func TestCacheBitmapDeduplicatesAndBoundsMemory(t *testing.T) {
	for _, algorithm := range []string{config.PayloadCacheHashFNV, config.PayloadCacheHashXXHash64} {
		for _, slots := range []int{64, 4096, 131072, config.MaxPayloadCacheSlots} {
			t.Run(fmt.Sprintf("%s/%d", algorithm, slots), func(t *testing.T) {
				versions, err := newCacheVersions(Options{PayloadCacheSlots: slots, PayloadCacheHash: algorithm})
				require.NoError(t, err)
				var mutations cacheMutations
				mutations.add([]byte("metadata/delivery/receipt"), versions)
				require.Nil(t, mutations, "metadata-only batches allocate no bitmap")
				unique := make(map[uint32]bool)
				for i := 0; i < 4096; i++ {
					var key [24]byte
					binary.BigEndian.PutUint64(key[16:], uint64(i%97))
					mutations.add(key[:], versions)
					unique[versions.slotHash(key[:], versions.mask)] = true
				}
				require.Len(t, mutations, slots/64)
				require.Equal(t, slots/64, cap(mutations), "duplicates cannot grow batch tracking")
				set := 0
				for _, word := range mutations {
					set += bits.OnesCount64(word)
				}
				require.Equal(t, len(unique), set)
				before := versions.snapshot()
				finishFirst := versions.begin(&mutations)
				finishSecond := versions.begin(&mutations)
				for slot := range unique {
					require.Equal(t, uint32(2), versions.pending[slot])
				}
				finishFirst()
				for slot := range unique {
					token := CacheToken{versions: versions, slot: slot, sequence: versions.sequence}
					require.False(t, token.Valid(), "overlapping write still pending")
				}
				finishSecond()
				for slot := range unique {
					require.Zero(t, versions.pending[slot])
					token := CacheToken{versions: versions, slot: slot, sequence: before.sequence}
					require.False(t, token.Valid(), "old snapshots remain invalid even after a failed commit")
					token.sequence = versions.sequence
					require.True(t, token.Valid())
				}
			})
		}
	}
}

func TestPayloadCacheHashConstructorsRejectUnknown(t *testing.T) {
	options := Options{PayloadCacheHash: "xxhash128"}
	_, err := newCacheVersions(options)
	require.ErrorContains(t, err, "payload cache hash")
	_, err = NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop(), options)
	require.ErrorContains(t, err, "payload cache hash")
	_, err = NewBoltDB(config.Bolt{File: filepath.Join(t.TempDir(), "events.db")}, options)
	require.ErrorContains(t, err, "payload cache hash")
}

func BenchmarkCacheBitmapTracking(b *testing.B) {
	for _, algorithm := range []string{config.PayloadCacheHashFNV, config.PayloadCacheHashXXHash64} {
		for _, count := range []int{64, 4096, 8192} {
			b.Run(fmt.Sprintf("%s/keys%d", algorithm, count), func(b *testing.B) {
				versions, err := newCacheVersions(Options{PayloadCacheHash: algorithm})
				if err != nil {
					b.Fatal(err)
				}
				keys := make([][24]byte, count)
				for i := range keys {
					binary.BigEndian.PutUint64(keys[i][16:], uint64(i))
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					var mutations cacheMutations
					for i := range keys {
						mutations.add(keys[i][:], versions)
					}
					versions.begin(&mutations)()
				}
			})
		}
	}
}
