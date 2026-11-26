package raft

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func pendingTestReceipts(start, count int) map[string]*DeliveryState {
	states := make(map[string]*DeliveryState, count)
	for id := start; id < start+count; id++ {
		states[strconv.Itoa(id)] = &DeliveryState{}
	}
	return states
}

func TestStandalonePendingReceiptsRespectCacheLimits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxBytes   int64
		maxEntries int
	}{
		{"bytes", 256, 16},
		{"entries", 1 << 20, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewStandaloneReceiptCache(tc.maxBytes, tc.maxEntries)
			first := pendingTestReceipts(0, 3)
			second := pendingTestReceipts(3, 2)
			require.True(t, cache.BeginPublish(first))
			require.False(t, cache.BeginPublish(second))
			require.Len(t, cache.pending, 3)

			cache.FinishPublish(first, false)
			require.True(t, cache.BeginPublish(second))
			require.Len(t, cache.pending, 2)
			cache.FinishPublish(second, false)
			require.Empty(t, cache.pending)
		})
	}
}

func TestStandalonePendingReceiptsReclaimBurstAllocation(t *testing.T) {
	cache := NewStandaloneReceiptCache(64<<20, 4096)
	burst := pendingTestReceipts(0, 2048)
	small := pendingTestReceipts(2048, 2)
	require.True(t, cache.BeginPublish(burst))
	require.True(t, cache.BeginPublish(small))
	cache.FinishPublish(burst, false)
	require.Len(t, cache.pending, 2)
	require.Equal(t, 2, cache.pendingPeak)
	for key := range small {
		require.Contains(t, cache.pending, key)
	}
	cache.FinishPublish(small, false)

	require.True(t, cache.BeginPublish(burst))
	cache.FinishPublish(burst, false)
	require.Nil(t, cache.pending, "a drained burst should release the backing map")
	require.Zero(t, cache.pendingPeak)
}
