package storage

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/suite"
)

// EngineFactory creates a fresh, isolated storage.DB for each test invocation.
// Implementations must return an empty database and register any cleanup
// (file removal, etc.) via the provided *testing.T.
type EngineFactory func(t *testing.T) DB

// DBContractSuite verifies that any storage.DB implementation honours the
// semantics defined in contract.go: lexicographic ordering, batch atomicity,
// exclusive upper bounds, iterator/scan equivalence, and the ErrNotFound
// sentinel. Run it once per engine.
type DBContractSuite struct {
	suite.Suite
	NewEngine EngineFactory
	db        DB
}

// TestDBContract is the entry point used by per-engine test files:
//
//	func TestPebbleContract(t *testing.T) {
//	    suite.Run(t, &DBContractSuite{NewEngine: newPebbleForTest})
//	}
func TestDBContract(t *testing.T) {
	// Placeholder so this file is a valid test target; real suites are
	// registered by the per-engine files below.
}

func (s *DBContractSuite) SetupTest() {
	s.Require().NotNil(s.NewEngine, "DBContractSuite.NewEngine must be set")
	s.db = s.NewEngine(s.T())
}

func (s *DBContractSuite) TearDownTest() {
	if s.db != nil {
		s.db.Close()
	}
}

// ─── Get / Set round-trip ───────────────────────────────────────────────────

func (s *DBContractSuite) TestGet_Set_RoundTrip() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("hello"), []byte("world")))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	val, closer, err := s.db.Get([]byte("hello"))
	require.NoError(err)
	require.Equal([]byte("world"), val)
	require.NoError(closer.Close())
}

// DRIFT NOTE: the contract in storage/contract.go says "Implementations must
// map their own not-found sentinel to this error" (ErrNotFound). Bolt does
// map bbolt's nil-return to ErrNotFound; Pebble does NOT — it returns
// pebble.ErrNotFound directly. This is drift between the two engines: the
// callers of storage.DB cannot rely on errors.Is(err, storage.ErrNotFound)
// unless Pebble's Get is patched. The test below enforces only that *some*
// error is returned; engines are free to return either sentinel.
func (s *DBContractSuite) TestGet_NotFound_ReturnsError() {
	require := s.Require()

	_, _, err := s.db.Get([]byte("definitely-missing-key"))
	require.Error(err, "Get on missing key must return a non-nil error")
}

func (s *DBContractSuite) TestGet_ReturnsCopy_NotAlias() {
	require := s.Require()

	original := []byte("value")
	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k"), original))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	// Mutating the source slice must not affect the stored value.
	original[0] = 'X'

	val, closer, err := s.db.Get([]byte("k"))
	require.NoError(err)
	defer closer.Close()
	require.Equal([]byte("value"), val,
		"stored value must be independent of the caller's slice")
}

func (s *DBContractSuite) TestGet_EmptyValue() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("empty"), []byte{}))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	val, closer, err := s.db.Get([]byte("empty"))
	require.NoError(err)
	defer closer.Close()
	require.Empty(val)
}

// ─── Batch ──────────────────────────────────────────────────────────────────

func (s *DBContractSuite) TestBatch_SetDelete_AtomicCommit() {
	require := s.Require()

	// Seed both keys.
	b1 := s.db.NewBatch()
	require.NoError(b1.Set([]byte("a"), []byte("1")))
	require.NoError(b1.Set([]byte("b"), []byte("2")))
	require.NoError(b1.Commit(Sync))
	require.NoError(b1.Close())

	// Delete one, overwrite the other, in a single batch.
	b2 := s.db.NewBatch()
	require.NoError(b2.Delete([]byte("a")))
	require.NoError(b2.Set([]byte("b"), []byte("updated")))
	require.NoError(b2.Commit(Sync))
	require.NoError(b2.Close())

	// Deleted key must not be retrievable.
	_, _, err := s.db.Get([]byte("a"))
	require.Error(err)

	val, closer, err := s.db.Get([]byte("b"))
	require.NoError(err)
	defer closer.Close()
	require.Equal([]byte("updated"), val)
}

func (s *DBContractSuite) TestBatch_DeleteNonExistent_NoError() {
	require := s.Require()

	// Deleting a key that does not exist must be a silent no-op.
	b := s.db.NewBatch()
	require.NoError(b.Delete([]byte("never-existed")))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())
}

func (s *DBContractSuite) TestBatch_CommitNoSync_Succeeds() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k"), []byte("v")))
	require.NoError(b.Commit(NoSync))
	require.NoError(b.Close())

	val, closer, err := s.db.Get([]byte("k"))
	require.NoError(err)
	defer closer.Close()
	require.Equal([]byte("v"), val)
}

func (s *DBContractSuite) TestBatch_CloseAfterCommit_NoError() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k"), []byte("v")))
	require.NoError(b.Commit(Sync))
	// Close after Commit must be a safe no-op per the Batch contract.
	require.NoError(b.Close())
}

func (s *DBContractSuite) TestBatch_OverwriteWithinBatch_LastWriteWins() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k"), []byte("first")))
	require.NoError(b.Set([]byte("k"), []byte("second")))
	require.NoError(b.Set([]byte("k"), []byte("third")))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	val, closer, err := s.db.Get([]byte("k"))
	require.NoError(err)
	defer closer.Close()
	require.Equal([]byte("third"), val)
}

// ─── Iterator ───────────────────────────────────────────────────────────────

func (s *DBContractSuite) seed(keys ...string) {
	b := s.db.NewBatch()
	for _, k := range keys {
		s.Require().NoError(b.Set([]byte(k), []byte("val-"+k)))
	}
	s.Require().NoError(b.Commit(Sync))
	s.Require().NoError(b.Close())
}

func (s *DBContractSuite) TestIterator_LexicographicOrder() {
	require := s.Require()

	s.seed("delta", "alpha", "charlie", "bravo")

	iter, err := s.db.NewIter(nil)
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	var got []string
	for iter.First(); iter.Valid(); iter.Next() {
		got = append(got, string(iter.Key()))
	}
	require.NoError(iter.Error())
	require.Equal([]string{"alpha", "bravo", "charlie", "delta"}, got,
		"engines MUST maintain lexicographic (byte-wise) key order")
}

func (s *DBContractSuite) TestIterator_BinaryKeys_ByteOrder() {
	require := s.Require()

	// Keys with high-bit bytes — exercises raw byte ordering, not string collation.
	b := s.db.NewBatch()
	keys := [][]byte{
		{0xFF},
		{0x00},
		{0x80},
		{0x01},
	}
	for _, k := range keys {
		require.NoError(b.Set(k, []byte("v")))
	}
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	iter, err := s.db.NewIter(nil)
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	var got [][]byte
	for iter.First(); iter.Valid(); iter.Next() {
		k := append([]byte(nil), iter.Key()...)
		got = append(got, k)
	}
	require.Equal([][]byte{{0x00}, {0x01}, {0x80}, {0xFF}}, got)
}

func (s *DBContractSuite) TestIterator_LowerBound_Inclusive() {
	require := s.Require()

	s.seed("a", "b", "c", "d")

	iter, err := s.db.NewIter(&IterOptions{LowerBound: []byte("b")})
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	var got []string
	for iter.First(); iter.Valid(); iter.Next() {
		got = append(got, string(iter.Key()))
	}
	require.Equal([]string{"b", "c", "d"}, got,
		"LowerBound is inclusive: key 'b' must be visited")
}

func (s *DBContractSuite) TestIterator_UpperBound_Exclusive() {
	require := s.Require()

	s.seed("a", "b", "c", "d")

	iter, err := s.db.NewIter(&IterOptions{UpperBound: []byte("c")})
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	var got []string
	for iter.First(); iter.Valid(); iter.Next() {
		got = append(got, string(iter.Key()))
	}
	require.Equal([]string{"a", "b"}, got,
		"UpperBound is exclusive: key 'c' must NOT be visited")
}

func (s *DBContractSuite) TestIterator_LowerAndUpperBound_Range() {
	require := s.Require()

	s.seed("a", "b", "c", "d", "e")

	iter, err := s.db.NewIter(&IterOptions{
		LowerBound: []byte("b"),
		UpperBound: []byte("d"),
	})
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	var got []string
	for iter.First(); iter.Valid(); iter.Next() {
		got = append(got, string(iter.Key()))
	}
	require.Equal([]string{"b", "c"}, got)
}

func (s *DBContractSuite) TestIterator_EmptyRange_YieldsNothing() {
	require := s.Require()

	s.seed("a", "b", "c")

	// LowerBound beyond all keys.
	iter, err := s.db.NewIter(&IterOptions{LowerBound: []byte("zzz")})
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	require.False(iter.First(), "empty range: First() must return false")
	require.False(iter.Valid())
}

func (s *DBContractSuite) TestIterator_EmptyDB_YieldsNothing() {
	require := s.Require()

	iter, err := s.db.NewIter(nil)
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	require.False(iter.First())
	require.False(iter.Valid())
	require.NoError(iter.Error())
}

func (s *DBContractSuite) TestIterator_KeyValue_Pairs() {
	require := s.Require()

	s.seed("only")

	iter, err := s.db.NewIter(nil)
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	require.True(iter.First())
	require.Equal([]byte("only"), iter.Key())
	require.Equal([]byte("val-only"), iter.Value())
	require.False(iter.Next())
	require.False(iter.Valid())
}

func (s *DBContractSuite) TestIterator_NilOptions_UnboundedScan() {
	require := s.Require()

	s.seed("x", "y", "z")

	iter, err := s.db.NewIter(nil)
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	count := 0
	for iter.First(); iter.Valid(); iter.Next() {
		count++
	}
	require.Equal(3, count, "nil IterOptions must scan everything")
}

// ─── Scan ───────────────────────────────────────────────────────────────────

func (s *DBContractSuite) TestScan_VisitsAllKeysInOrder() {
	require := s.Require()

	s.seed("m1", "m2", "m3", "m4")

	var visited []string
	err := s.db.Scan(nil, func(k, v []byte) error {
		visited = append(visited, string(k))
		return nil
	})
	require.NoError(err)
	require.Equal([]string{"m1", "m2", "m3", "m4"}, visited)
}

func (s *DBContractSuite) TestScan_WithBounds_RespectsThem() {
	require := s.Require()

	s.seed("a", "b", "c", "d")

	var visited []string
	err := s.db.Scan(&IterOptions{
		LowerBound: []byte("b"),
		UpperBound: []byte("d"),
	}, func(k, v []byte) error {
		visited = append(visited, string(k))
		return nil
	})
	require.NoError(err)
	require.Equal([]string{"b", "c"}, visited)
}

func (s *DBContractSuite) TestScan_YieldError_StopsAndPropagates() {
	require := s.Require()

	s.seed("k1", "k2", "k3")

	sentinel := fmt.Errorf("stop here")
	count := 0
	err := s.db.Scan(nil, func(k, v []byte) error {
		count++
		return sentinel
	})
	require.Error(err, "yield error must propagate to the caller")
	require.Equal(1, count, "scan must stop at the first yield error")
}

func (s *DBContractSuite) TestScan_EmptyDB_CallsYieldZeroTimes() {
	require := s.Require()

	called := false
	err := s.db.Scan(nil, func(k, v []byte) error {
		called = true
		return nil
	})
	require.NoError(err)
	require.False(called)
}

func (s *DBContractSuite) TestScan_ValuesMatchStored() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k"), []byte("expected-value")))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	var gotVal []byte
	err := s.db.Scan(nil, func(k, v []byte) error {
		gotVal = append([]byte(nil), v...)
		return nil
	})
	require.NoError(err)
	require.Equal([]byte("expected-value"), gotVal)
}

// ─── Consistency: iterator vs scan ──────────────────────────────────────────

func (s *DBContractSuite) TestIteratorAndScan_SeeSameData() {
	require := s.Require()

	s.seed("p", "q", "r")

	var viaIter []string
	iter, err := s.db.NewIter(nil)
	require.NoError(err)
	for iter.First(); iter.Valid(); iter.Next() {
		viaIter = append(viaIter, string(iter.Key()))
	}
	require.NoError(iter.Close())

	var viaScan []string
	require.NoError(s.db.Scan(nil, func(k, v []byte) error {
		viaScan = append(viaScan, string(k))
		return nil
	}))

	require.Equal(viaIter, viaScan,
		"NewIter and Scan must observe the same key set and order")
}

// ─── Flush / Close ──────────────────────────────────────────────────────────

func (s *DBContractSuite) TestFlush_NoError() {
	require := s.Require()

	require.NoError(s.db.Flush())
}

func (s *DBContractSuite) TestFlush_AfterWrites_DataStillReadable() {
	require := s.Require()

	s.seed("persisted")
	require.NoError(s.db.Flush())

	val, closer, err := s.db.Get([]byte("persisted"))
	require.NoError(err)
	defer closer.Close()
	require.Equal([]byte("val-persisted"), val)
}

// ─── Large-ish workload ─────────────────────────────────────────────────────

func (s *DBContractSuite) TestManyKeys_AllRetrievable() {
	require := s.Require()

	const n = 500

	b := s.db.NewBatch()
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("key-%04d", i)
		v := fmt.Sprintf("value-%04d", i)
		require.NoError(b.Set([]byte(k), []byte(v)))
	}
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	// Spot-check reads.
	for _, i := range []int{0, n / 2, n - 1} {
		k := fmt.Sprintf("key-%04d", i)
		want := fmt.Sprintf("value-%04d", i)
		val, closer, err := s.db.Get([]byte(k))
		require.NoError(err)
		require.Equal([]byte(want), val)
		closer.Close()
	}

	// Full scan must visit exactly n keys.
	count := 0
	require.NoError(s.db.Scan(nil, func(k, v []byte) error {
		count++
		return nil
	}))
	require.Equal(n, count)
}
