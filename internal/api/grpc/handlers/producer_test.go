package handlers

import (
	"testing"
	"time"

	"github.com/futureq-io/futureq/pkg/utils"
)

// TestCalculateBucket verifies the new bucket-index semantics.
//
// CalculateBucket returns floor(unixMs / bucketSizeMs), i.e. the integer bucket
// index — not a millisecond-aligned boundary. This is intentional: the bucket
// is used as a lexicographic key prefix in Pebble; its absolute value is not
// meaningful to consumers.
//
// Mapping:
//   - 17000ms / 1000ms = bucket 17
//   - 17001ms / 1000ms = bucket 17   (same bucket as 17000)
//   - 17999ms / 1000ms = bucket 17   (still bucket 17)
//   - 18000ms / 1000ms = bucket 18
//   - 1500ms  / 500ms  = bucket 3
//   - 1501ms  / 500ms  = bucket 3
//
// The dispatcher scans all keys with bucket <= currentBucket so messages
// are dispatched as soon as their bucket index is reached.
func TestCalculateBucket(t *testing.T) {
	tests := []struct {
		name       string
		executeAt  int64
		bucketSize time.Duration
		expected   uint64
	}{
		{
			name:       "exact multiple of 1s",
			executeAt:  17000,
			bucketSize: 1 * time.Second,
			expected:   17, // 17000 / 1000 = 17
		},
		{
			name:       "slightly over bucket boundary",
			executeAt:  17001,
			bucketSize: 1 * time.Second,
			expected:   17, // 17001 / 1000 = 17 (floor division)
		},
		{
			name:       "just under next bucket boundary",
			executeAt:  17999,
			bucketSize: 1 * time.Second,
			expected:   17, // 17999 / 1000 = 17 (floor division)
		},
		{
			name:       "exactly 0",
			executeAt:  0,
			bucketSize: 1 * time.Second,
			expected:   0,
		},
		{
			name:       "negative value treated as 0 bucket",
			executeAt:  -100,
			bucketSize: 1 * time.Second,
			expected:   0, // negative → bucket 0 (earliest possible)
		},
		{
			name:       "bucket size is 0 (raw ms)",
			executeAt:  17300,
			bucketSize: 0,
			expected:   17300, // bucketSize=0 → return raw ms as bucket
		},
		{
			name:       "bucket size is 500ms, exact multiple",
			executeAt:  1500,
			bucketSize: 500 * time.Millisecond,
			expected:   3, // 1500 / 500 = 3
		},
		{
			name:       "bucket size is 500ms, one ms over",
			executeAt:  1501,
			bucketSize: 500 * time.Millisecond,
			expected:   3, // 1501 / 500 = 3 (floor division)
		},
		{
			name:       "bucket size is 500ms, just under next boundary",
			executeAt:  1999,
			bucketSize: 500 * time.Millisecond,
			expected:   3, // 1999 / 500 = 3
		},
		{
			name:       "bucket size is 500ms, at next boundary",
			executeAt:  2000,
			bucketSize: 500 * time.Millisecond,
			expected:   4, // 2000 / 500 = 4
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := utils.CalculateBucket(tt.executeAt, tt.bucketSize)
			if got != tt.expected {
				t.Errorf("CalculateBucket(%d, %v) = %d; want %d",
					tt.executeAt, tt.bucketSize, got, tt.expected)
			}
		})
	}
}
