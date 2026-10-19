package storage

import (
	"fmt"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPebbleCompactionConcurrency(t *testing.T) {
	for _, concurrency := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			db, err := NewPebble(config.Pebble{Mode: "memory", CompactionConcurrency: concurrency}, zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			require.Equal(t, max(1, concurrency), db.db.GetAllowedWithoutPermission())
		})
	}
	_, err := NewPebble(config.Pebble{Mode: "memory", CompactionConcurrency: -1}, zap.NewNop())
	require.Error(t, err)
}
