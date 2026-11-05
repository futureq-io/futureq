package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func emptyRangeDB(t testing.TB) *Pebble {
	t.Helper()
	db, err := NewPebble(config.Pebble{Mode: "memory", MemtableSize: "128MiB"}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func setEmptyRangeKey(t testing.TB, db *Pebble, key string) {
	t.Helper()
	b := db.NewBatch()
	require.NoError(t, b.Set([]byte(key), []byte("value")))
	require.NoError(t, b.Commit(NoSync))
	require.NoError(t, b.Close())
}

func scanEmptyRange(t testing.TB, db *Pebble, lower, upper string) []string {
	t.Helper()
	iter, err := db.NewIter(&IterOptions{LowerBound: []byte(lower), UpperBound: []byte(upper)})
	require.NoError(t, err)
	defer iter.Close()
	var keys []string
	for valid := iter.First(); valid; valid = iter.Next() {
		keys = append(keys, string(iter.Key()))
	}
	require.NoError(t, iter.Error())
	return keys
}

func TestPebbleEmptyRangeLateInsertAndGrowingBounds(t *testing.T) {
	db := emptyRangeDB(t)
	require.Empty(t, scanEmptyRange(t, db, "a", "m"))
	require.Empty(t, scanEmptyRange(t, db, "b", "c"), "a cached gap can cover the entire requested range")
	setEmptyRangeKey(t, db, "z")
	require.Equal(t, []string{"z"}, scanEmptyRange(t, db, "a", "zz"), "moving upper bounds still discover the tail")
	setEmptyRangeKey(t, db, "b")
	require.Equal(t, []string{"b", "z"}, scanEmptyRange(t, db, "a", "zz"), "late inserts behind discovery must be visible immediately")
	require.Equal(t, []string{"b", "z"}, scanEmptyRange(t, db, "a", "zz"), "live keys must never become watermarks")
	batch := db.NewBatch()
	require.NoError(t, batch.Delete([]byte("b")))
	require.NoError(t, batch.Commit(NoSync))
	require.NoError(t, batch.Close())
	require.Equal(t, []string{"z"}, scanEmptyRange(t, db, "a", "zz"))
	setEmptyRangeKey(t, db, "b")
	require.Equal(t, []string{"b", "z"}, scanEmptyRange(t, db, "a", "zz"), "restore/reinsert must invalidate old negative knowledge")
}

func TestPebbleEmptyRangeOldSnapshotCannotHideInsert(t *testing.T) {
	db := emptyRangeDB(t)
	old, err := db.NewIter(&IterOptions{LowerBound: []byte("a"), UpperBound: []byte("z")})
	require.NoError(t, err)
	setEmptyRangeKey(t, db, "m")
	require.False(t, old.First(), "iterator retains its original snapshot")
	require.NoError(t, old.Close())
	require.Equal(t, []string{"m"}, scanEmptyRange(t, db, "a", "z"), "old snapshot must not repopulate the raced gap")
	// Repeated First must not learn from an old snapshot after its probe ends.
	old, err = db.NewIter(&IterOptions{LowerBound: []byte("b"), UpperBound: []byte("l")})
	require.NoError(t, err)
	require.False(t, old.First())
	setEmptyRangeKey(t, db, "c")
	require.False(t, old.First())
	require.NoError(t, old.Close())
	require.Equal(t, []string{"c", "m"}, scanEmptyRange(t, db, "a", "z"))
}

func TestPebbleEmptyRangePendingAndFailedWrites(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			db := emptyRangeDB(t)
			require.Empty(t, scanEmptyRange(t, db, "a", "z"))
			span := pebbleKeySpan{}
			span.include([]byte("m"))
			finish := db.empty.begin(&span) // pause a commit before engine publication
			old, err := db.NewIter(&IterOptions{LowerBound: []byte("a"), UpperBound: []byte("z")})
			require.NoError(t, err)
			require.False(t, old.First(), "pending Set has not reached the engine")
			if committed {
				require.NoError(t, db.db.Set([]byte("m"), []byte("value"), pebble.NoSync))
			}
			finish()
			require.NoError(t, old.Close())
			if committed {
				require.Equal(t, []string{"m"}, scanEmptyRange(t, db, "a", "z"))
			} else {
				require.Empty(t, scanEmptyRange(t, db, "a", "z"))
			}
		})
	}
}

func TestPebbleEmptyRangeConcurrentWritesMatchSnapshot(t *testing.T) {
	db := emptyRangeDB(t)
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := range 400 {
			b := db.NewBatch()
			key := []byte(fmt.Sprintf("k%03d", i%80))
			if i%3 == 0 {
				require.NoError(t, b.Delete(key))
			} else {
				require.NoError(t, b.Set(key, key))
			}
			require.NoError(t, b.Commit(NoSync))
			require.NoError(t, b.Close())
		}
	}()
	defer writers.Wait()
	for range 400 {
		it, err := db.NewIter(&IterOptions{LowerBound: []byte("k"), UpperBound: []byte("l")})
		require.NoError(t, err)
		cached := it.(*pebbleIterator)
		raw, err := cached.snap.NewIter(&pebble.IterOptions{LowerBound: []byte("k"), UpperBound: []byte("l")})
		require.NoError(t, err)
		got, want := it.First(), raw.First()
		for got && want {
			require.Equal(t, raw.Key(), it.Key())
			require.Equal(t, raw.Value(), it.Value())
			got, want = it.Next(), raw.Next()
		}
		require.Equal(t, want, got)
		require.NoError(t, raw.Error())
		require.NoError(t, it.Error())
		require.NoError(t, raw.Close())
		require.NoError(t, it.Close())
	}
	require.Empty(t, db.empty.probes)
	require.Empty(t, db.empty.pending)
}

func TestPebbleEmptyRangeBoundedAndAbandonedProbes(t *testing.T) {
	db := emptyRangeDB(t)
	for i := range 3 * maxPebbleEmptyRanges {
		lower := fmt.Sprintf("k%04d", i*2)
		upper := fmt.Sprintf("k%04d", i*2+1)
		require.Empty(t, scanEmptyRange(t, db, lower, upper))
	}
	require.LessOrEqual(t, len(db.empty.ranges), maxPebbleEmptyRanges)
	var iters []Iterator
	for range 2 * maxPebbleEmptyProbes {
		it, err := db.NewIter(&IterOptions{LowerBound: []byte("a"), UpperBound: []byte("z")})
		require.NoError(t, err)
		iters = append(iters, it)
	}
	require.Len(t, db.empty.probes, maxPebbleEmptyProbes)
	for _, it := range iters {
		require.NoError(t, it.Close())
	}
	require.Empty(t, db.empty.probes)
}

func TestPebbleEmptyRangeRandomizedAgainstModel(t *testing.T) {
	db := emptyRangeDB(t)
	rng := rand.New(rand.NewSource(19))
	model := make(map[byte]bool)
	for range 1000 {
		key := byte(rng.Intn(100) + 1)
		b := db.NewBatch()
		if rng.Intn(3) == 0 {
			require.NoError(t, b.Delete([]byte{key}))
			delete(model, key)
		} else {
			require.NoError(t, b.Set([]byte{key}, []byte{key}))
			model[key] = true
		}
		require.NoError(t, b.Commit(NoSync))
		require.NoError(t, b.Close())
		lo := byte(rng.Intn(100))
		hi := lo + byte(rng.Intn(102-int(lo)))
		var want []string
		for k := lo; k < hi; k++ {
			if model[k] {
				want = append(want, string([]byte{k}))
			}
		}
		require.Equal(t, want, scanEmptyRange(t, db, string([]byte{lo}), string([]byte{hi})))
	}
}

func BenchmarkPebbleTombstoneFirst(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			db := emptyRangeDB(b)
			// Retain point tombstones in the same large memtable used by the
			// profiled deployment. Reproduce the internal work, not just timing.
			batch := db.NewBatch()
			for i := range count {
				key := make([]byte, 24)
				binary.BigEndian.PutUint64(key[16:], uint64(i))
				require.NoError(b, batch.Set(key, bytes.Repeat([]byte("v"), 200)))
				require.NoError(b, batch.Delete(key))
			}
			require.NoError(b, batch.Commit(NoSync))
			require.NoError(b, batch.Close())
			opts := &IterOptions{LowerBound: make([]byte, 8), UpperBound: []byte{1}}
			// Prime one proof; subsequent cyclic scans can skip this empty range.
			it, err := db.NewIter(opts)
			require.NoError(b, err)
			require.False(b, it.First())
			require.NoError(b, it.Close())
			for _, cached := range []bool{false, true} {
				b.Run(fmt.Sprintf("cached=%t", cached), func(b *testing.B) {
					b.ReportAllocs()
					steps := 0
					for b.Loop() {
						if cached {
							it, err := db.NewIter(opts)
							require.NoError(b, err)
							require.False(b, it.First())
							steps += it.(*pebbleIterator).iter.Stats().ForwardStepCount[pebble.InternalIterCall]
							require.NoError(b, it.Close())
						} else {
							snap := db.db.NewSnapshot()
							it, err := snap.NewIter(&pebble.IterOptions{LowerBound: opts.LowerBound, UpperBound: opts.UpperBound})
							require.NoError(b, err)
							require.False(b, it.First())
							steps += it.Stats().ForwardStepCount[pebble.InternalIterCall]
							require.NoError(b, it.Close())
							require.NoError(b, snap.Close())
						}
					}
					b.ReportMetric(float64(steps)/float64(b.N), "internal-steps/op")
				})
			}
		})
	}
}

func TestPebbleEmptyRangeRetainsPrefixDuringPublishing(t *testing.T) {
	db := emptyRangeDB(t)
	b := db.NewBatch()
	for i := range 10000 {
		key := []byte(fmt.Sprintf("event/%08d", i))
		require.NoError(t, b.Set(key, key))
		require.NoError(t, b.Delete(key))
	}
	require.NoError(t, b.Commit(NoSync))
	require.NoError(t, b.Close())
	opts := &IterOptions{LowerBound: []byte("event/"), UpperBound: []byte("event0")}
	first, err := db.NewIter(opts)
	require.NoError(t, err)
	require.False(t, first.First())
	require.GreaterOrEqual(t, first.(*pebbleIterator).iter.Stats().ForwardStepCount[pebble.InternalIterCall], 10000)
	require.NoError(t, first.Close())
	for i := 10000; i < 10020; i++ {
		key := fmt.Sprintf("event/%08d", i)
		setEmptyRangeKey(t, db, key)
		it, err := db.NewIter(opts)
		require.NoError(t, err)
		require.True(t, it.First())
		require.Equal(t, key, string(it.Key()))
		require.Less(t, it.(*pebbleIterator).iter.Stats().ForwardStepCount[pebble.InternalIterCall], 10, "publishing must preserve the learned historical prefix")
		require.NoError(t, it.Close())
		b := db.NewBatch()
		require.NoError(t, b.Delete([]byte(key)))
		require.NoError(t, b.Commit(NoSync))
		require.NoError(t, b.Close())
	}
}

func TestPebbleEmptyRangeKeepsPayloadValidityProof(t *testing.T) {
	db := emptyRangeDB(t)
	lower := []byte("01234567")
	key := []byte("012345678901234567890123")
	opts := &IterOptions{LowerBound: lower, UpperBound: []byte("1")}
	it, err := db.NewIter(opts)
	require.NoError(t, err)
	require.False(t, it.First())
	require.NoError(t, it.Close())
	setEmptyRangeKey(t, db, string(key))
	it, err = db.NewIter(opts)
	require.NoError(t, err)
	require.True(t, it.First())
	require.Equal(t, key, it.Key())
	token := it.(CachedIterator).CacheToken(key)
	require.True(t, token.Valid())
	require.NoError(t, it.Close())
	setEmptyRangeKey(t, db, string(key))
	require.False(t, token.Valid(), "same-key writes must still invalidate the payload's snapshot proof")
}

func TestPebbleEmptyRangeSnapshotCapturedDuringCommit(t *testing.T) {
	db := emptyRangeDB(t)
	require.Empty(t, scanEmptyRange(t, db, "a", "z"))
	span := pebbleKeySpan{}
	span.include([]byte("m"))
	finish := db.empty.begin(&span)
	it, err := db.NewIter(&IterOptions{LowerBound: []byte("a"), UpperBound: []byte("z")})
	require.NoError(t, err)
	require.NoError(t, db.db.Set([]byte("m"), []byte("value"), pebble.NoSync))
	finish()
	require.False(t, it.First())
	require.NoError(t, it.Close())
	require.Equal(t, []string{"m"}, scanEmptyRange(t, db, "a", "z"))
}

func BenchmarkPebblePublishDiscoveryChurn(b *testing.B) {
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprintf("cached=%t", cached), func(b *testing.B) {
			db := emptyRangeDB(b)
			const history = 100000
			keyAt := func(id uint64) []byte {
				key := make([]byte, 24)
				binary.BigEndian.PutUint64(key[16:], id)
				return key
			}
			batch := db.NewBatch()
			value := bytes.Repeat([]byte("v"), 200)
			for i := range history {
				key := keyAt(uint64(i))
				require.NoError(b, batch.Set(key, value))
				require.NoError(b, batch.Delete(key))
			}
			require.NoError(b, batch.Commit(NoSync))
			require.NoError(b, batch.Close())
			opts := &IterOptions{LowerBound: make([]byte, 8), UpperBound: []byte{1}}
			if cached {
				it, err := db.NewIter(opts)
				require.NoError(b, err)
				require.False(b, it.First())
				require.NoError(b, it.Close())
			}
			b.ReportAllocs()
			steps := 0
			id := uint64(history)
			for b.Loop() {
				key := keyAt(id)
				batch := db.NewBatch()
				require.NoError(b, batch.Delete(keyAt(id-1)))
				require.NoError(b, batch.Set(key, value))
				require.NoError(b, batch.Commit(NoSync))
				require.NoError(b, batch.Close())
				if cached {
					it, err := db.NewIter(opts)
					require.NoError(b, err)
					require.True(b, it.First())
					require.Equal(b, key, it.Key())
					require.False(b, it.Next())
					steps += it.(*pebbleIterator).iter.Stats().ForwardStepCount[pebble.InternalIterCall]
					require.NoError(b, it.Close())
				} else {
					snap := db.db.NewSnapshot()
					it, err := snap.NewIter(&pebble.IterOptions{LowerBound: opts.LowerBound, UpperBound: opts.UpperBound})
					require.NoError(b, err)
					require.True(b, it.First())
					require.Equal(b, key, it.Key())
					require.False(b, it.Next())
					steps += it.Stats().ForwardStepCount[pebble.InternalIterCall]
					require.NoError(b, it.Close())
					require.NoError(b, snap.Close())
				}
				id++
			}
			b.ReportMetric(float64(steps)/float64(b.N), "internal-steps/op")
		})
	}
}
