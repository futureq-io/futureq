package storage

import (
	"fmt"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

// pebbleCleanup is the shared in-memory pebble opener used across sub-tests.
func openMemPebble(t *testing.T) *Pebble {
	t.Helper()
	cfg := config.Pebble{DataPath: ""} // empty path → in-memory FS
	logger := zap.NewNop()

	p, err := NewPebble(cfg, logger)
	if err != nil {
		t.Fatalf("failed to open in-memory pebble: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Close()
	})
	return p
}

// ─── PebbleSuite ────────────────────────────────────────────────────────────

type PebbleSuite struct {
	suite.Suite
	db *Pebble
}

func TestPebbleSuite(t *testing.T) {
	suite.Run(t, new(PebbleSuite))
}

func (s *PebbleSuite) SetupTest() {
	cfg := config.Pebble{DataPath: ""}
	logger := zap.NewNop()

	db, err := NewPebble(cfg, logger)
	s.Require().NoError(err)
	s.db = db
}

func (s *PebbleSuite) TearDownTest() {
	if s.db != nil {
		s.db.Close()
	}
}

// ─── Constructor ────────────────────────────────────────────────────────────

func (s *PebbleSuite) TestNewPebble_InMemory_Succeeds() {
	require := s.Require()

	cfg := config.Pebble{DataPath: ""}
	db, err := NewPebble(cfg, zap.NewNop())
	require.NoError(err)
	require.NotNil(db)
	require.NoError(db.Close())
}

// ─── Get / NewBatch basic round-trip ────────────────────────────────────────

func (s *PebbleSuite) TestGet_Set_RoundTrip() {
	require := s.Require()

	key := []byte("mykey")
	value := []byte("myvalue")

	b := s.db.NewBatch()
	require.NoError(b.Set(key, value))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	got, closer, err := s.db.Get(key)
	require.NoError(err)
	require.Equal(value, got)
	require.NoError(closer.Close())
}

func (s *PebbleSuite) TestGet_NotFound_ReturnsError() {
	require := s.Require()

	_, _, err := s.db.Get([]byte("does-not-exist"))
	require.Error(err)
}

// ─── Batch ──────────────────────────────────────────────────────────────────

func (s *PebbleSuite) TestBatch_SetDelete_Commit_NoSync() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("a"), []byte("1")))
	require.NoError(b.Set([]byte("b"), []byte("2")))
	require.NoError(b.Delete([]byte("a")))
	require.NoError(b.Commit(NoSync))
	require.NoError(b.Close())

	// "a" was deleted — should NOT exist
	_, _, err := s.db.Get([]byte("a"))
	require.Error(err)

	// "b" should exist
	got, closer, err := s.db.Get([]byte("b"))
	require.NoError(err)
	require.Equal([]byte("2"), got)
	closer.Close()
}

func (s *PebbleSuite) TestBatch_SetDelete_Commit_Sync() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("x"), []byte("y")))
	require.NoError(b.Delete([]byte("x")))
	require.NoError(b.Commit(Sync))
	require.NoError(b.Close())

	_, _, err := s.db.Get([]byte("x"))
	require.Error(err)
}

func (s *PebbleSuite) TestBatch_Close_AfterCommit_IsSafe() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k"), []byte("v")))
	require.NoError(b.Commit(Sync))
	// Calling Close after Commit must not panic or return an error.
	require.NoError(b.Close())
}

// ─── NewIter / iterator ────────────────────────────────────────────────────

func (s *PebbleSuite) TestIterator_Unbounded_FullScan() {
	require := s.Require()

	// Seed data.
	b := s.db.NewBatch()
	keys := []string{"apple", "banana", "cherry"}
	for _, k := range keys {
		require.NoError(b.Set([]byte(k), []byte("v-"+k)))
	}
	require.NoError(b.Commit(Sync))
	b.Close()

	iter, err := s.db.NewIter(nil)
	require.NoError(err)

	defer iter.Close() //nolint:errcheck

	var got []string
	for iter.First(); iter.Valid(); iter.Next() {
		got = append(got, string(iter.Key()))
	}
	require.NoError(iter.Error())
	require.Equal(keys, got, "iterator must walk keys in lex order")
}

func (s *PebbleSuite) TestIterator_LowerBound() {
	require := s.Require()

	b := s.db.NewBatch()
	for _, k := range []string{"a", "b", "c", "d"} {
		require.NoError(b.Set([]byte(k), []byte("1")))
	}
	require.NoError(b.Commit(Sync))
	b.Close()

	iter, err := s.db.NewIter(&IterOptions{LowerBound: []byte("b")})
	require.NoError(err)

	defer iter.Close() //nolint:errcheck

	var got []string
	for iter.First(); iter.Valid(); iter.Next() {
		got = append(got, string(iter.Key()))
	}
	require.Equal([]string{"b", "c", "d"}, got)
}

func (s *PebbleSuite) TestIterator_UpperBound_Exclusive() {
	require := s.Require()

	b := s.db.NewBatch()
	for _, k := range []string{"a", "b", "c", "d"} {
		require.NoError(b.Set([]byte(k), []byte("1")))
	}
	require.NoError(b.Commit(Sync))
	b.Close()

	iter, err := s.db.NewIter(&IterOptions{UpperBound: []byte("c")})
	require.NoError(err)

	defer iter.Close() //nolint:errcheck

	var got []string
	for iter.First(); iter.Valid(); iter.Next() {
		got = append(got, string(iter.Key()))
	}
	// "c" must be excluded.
	require.Equal([]string{"a", "b"}, got)
}

func (s *PebbleSuite) TestIterator_LowerAndUpperBound() {
	require := s.Require()

	b := s.db.NewBatch()
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		require.NoError(b.Set([]byte(k), []byte("v")))
	}
	require.NoError(b.Commit(Sync))
	b.Close()

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

func (s *PebbleSuite) TestIterator_Key_Value_Pair() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k1"), []byte("val1")))
	require.NoError(b.Commit(Sync))
	b.Close()

	iter, err := s.db.NewIter(nil)
	require.NoError(err)
	defer iter.Close() //nolint:errcheck

	require.True(iter.First())
	require.Equal([]byte("k1"), iter.Key())
	require.Equal([]byte("val1"), iter.Value())
	require.False(iter.Next()) // only one key
}

// ─── Scan ───────────────────────────────────────────────────────────────────

func (s *PebbleSuite) TestScan_Visits_AllKeys() {
	require := s.Require()

	b := s.db.NewBatch()
	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("key%d", i)
		require.NoError(b.Set([]byte(k), []byte("v"+string(rune('0'+i)))))
	}
	require.NoError(b.Commit(Sync))
	b.Close()

	var visited []string
	err := s.db.Scan(nil, func(k, v []byte) error {
		visited = append(visited, string(k))
		return nil
	})
	require.NoError(err)
	require.Len(visited, 5)
}

func (s *PebbleSuite) TestScan_WithBounds() {
	require := s.Require()

	b := s.db.NewBatch()
	for _, k := range []string{"a", "b", "c", "d"} {
		require.NoError(b.Set([]byte(k), []byte("1")))
	}
	require.NoError(b.Commit(Sync))
	b.Close()

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

func (s *PebbleSuite) TestScan_YieldError_IsPropagated() {
	require := s.Require()

	b := s.db.NewBatch()
	require.NoError(b.Set([]byte("k"), []byte("v")))
	require.NoError(b.Commit(Sync))
	b.Close()

	sentinel := fmt.Errorf("yield sentinel")
	err := s.db.Scan(nil, func(k, v []byte) error {
		return sentinel
	})
	require.Error(err)
}

// ─── Flush / Close ─────────────────────────────────────────────────────────

func (s *PebbleSuite) TestFlush_NoError() {
	require := s.Require()

	require.NoError(s.db.Flush())
}
