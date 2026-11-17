package dispatcher

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/stretchr/testify/require"
)

var candidateHeapTypes = []config.HeapType{config.HeapBinary, config.HeapTypedBinary, config.Heap4Ary, config.Heap8Ary}

func heapTestCandidates(count int, equalDeadlines bool) candidateHeap {
	items := make(candidateHeap, count)
	rng := rand.New(rand.NewPCG(42, 7))
	for i := range items {
		// Vary every key word to exercise the complete byte-wise tie breaker.
		binary.BigEndian.PutUint64(items[i].key[:8], rng.Uint64())
		binary.BigEndian.PutUint64(items[i].key[8:16], rng.Uint64())
		binary.BigEndian.PutUint64(items[i].key[16:], uint64(i))
		if !equalDeadlines {
			items[i].due = rng.Int64N(1000) - 500
		}
		items[i].payload.message = &deliverypb.StoredDeliveryMessage{Payload: []byte{byte(i)}}
		items[i].ready = i%3 == 0
	}
	return items
}

func sortedHeapCandidates(items candidateHeap) candidateHeap {
	ordered := slices.Clone(items)
	slices.SortFunc(ordered, func(a, b preloadCandidate) int {
		if a.due < b.due {
			return -1
		}
		if a.due > b.due {
			return 1
		}
		return bytes.Compare(a.key[:], b.key[:])
	})
	return ordered
}

func TestCandidateHeapOrderingAndPayloadRelease(t *testing.T) {
	for _, kind := range candidateHeapTypes {
		for _, ties := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ties=%t", kind, ties), func(t *testing.T) {
				shift := candidateHeapShift(kind)
				items := heapTestCandidates(10000, ties)
				want := sortedHeapCandidates(items)
				h := make(candidateHeap, 0, len(items))
				for _, item := range items {
					h.pushCandidate(item, shift)
				}
				backing := h[:cap(h)]
				for _, expected := range want {
					require.Equal(t, expected, h[0], "peek must match pop ordering")
					require.Equal(t, expected, h.popCandidate(shift))
					require.Equal(t, preloadCandidate{}, backing[len(h)], "popped slot must release payload")
				}
				require.Empty(t, h)
				// Reusing an emptied heap must also handle the single-element case.
				h.pushCandidate(items[0], shift)
				require.Equal(t, items[0], h.popCandidate(shift))
			})
		}
	}
}

func TestCandidateHeapInterleavesEarlierArrivals(t *testing.T) {
	for _, kind := range candidateHeapTypes {
		t.Run(string(kind), func(t *testing.T) {
			shift := candidateHeapShift(kind)
			var h candidateHeap
			var pending candidateHeap
			for i, item := range heapTestCandidates(2048, false) {
				h.pushCandidate(item, shift)
				pending = append(pending, item)
				if i%3 == 0 {
					pending = sortedHeapCandidates(pending)
					require.Equal(t, pending[0], h.popCandidate(shift))
					pending = pending[1:]
				}
			}
			for _, expected := range sortedHeapCandidates(pending) {
				require.Equal(t, expected, h.popCandidate(shift))
			}
		})
	}
}

func TestCandidateHeapRebuildAndCompaction(t *testing.T) {
	for _, kind := range candidateHeapTypes {
		t.Run(string(kind), func(t *testing.T) {
			p := &topicPreload{heapShift: candidateHeapShift(kind)}
			d := &Dispatcher{}
			var future candidateHeap
			for _, item := range heapTestCandidates(1024, false) {
				require.True(t, d.admitCandidate(p, item))
				require.False(t, d.admitCandidate(p, item), "admission must deduplicate")
				if !item.ready {
					future = append(future, item)
				}
			}
			d.releaseReady("orders", p)
			require.Zero(t, d.readyCount)
			require.Zero(t, p.readyCount)
			require.Equal(t, len(future), d.preloadCount)
			require.Equal(t, uint64(1), p.repairGeneration)
			for _, expected := range sortedHeapCandidates(future) {
				require.Equal(t, expected, p.items.popCandidate(p.heapShift))
				p.removeKey(expected.key)
				p.compact()
				if len(p.items) > 0 {
					require.Less(t, cap(p.items), 4*len(p.items)+4)
				}
			}
			require.Nil(t, p.items)
			require.Nil(t, p.keys)
			// Rebuilds of empty/single-element heaps must not index a missing parent.
			p.items.initCandidates(p.heapShift)
			p.items = heapTestCandidates(1, false)
			p.items.initCandidates(p.heapShift)
		})
	}
}

func TestTypedCandidateHeapOperationsDoNotAllocate(t *testing.T) {
	for _, kind := range candidateHeapTypes[1:] {
		t.Run(string(kind), func(t *testing.T) {
			shift := candidateHeapShift(kind)
			h := heapTestCandidates(16384, false)
			h.initCandidates(shift)
			allocs := testing.AllocsPerRun(100, func() {
				item := h.popCandidate(shift)
				item.due += 1000
				h.pushCandidate(item, shift)
			})
			require.Zero(t, allocs)
		})
	}
}

// Benchmarks include the production selection dispatch and identical candidate
// layouts. They measure heap operations only, excluding receipt reads and sends.
func BenchmarkCandidateHeap(b *testing.B) {
	for _, count := range []int{10000, 16384} {
		for _, ties := range []bool{false, true} {
			items := heapTestCandidates(count, ties)
			// Model real event keys: one topic, then due bucket, then event ID.
			// New equal-deadline arrivals receive increasing IDs, so pop/push
			// cycles through the backlog instead of reinserting the same minimum.
			for i := range items {
				binary.BigEndian.PutUint64(items[i].key[:8], 1)
				binary.BigEndian.PutUint64(items[i].key[8:16], uint64(items[i].due+500))
				binary.BigEndian.PutUint64(items[i].key[16:], uint64(i))
			}
			for _, kind := range candidateHeapTypes {
				b.Run(fmt.Sprintf("%s/n=%d/ties=%t/pop-push", kind, count, ties), func(b *testing.B) {
					shift := candidateHeapShift(kind)
					h := slices.Clone(items)
					h.initCandidates(shift)
					nextID := uint64(count)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						item := h.popCandidate(shift)
						if !ties {
							item.due += 1000
							binary.BigEndian.PutUint64(item.key[8:16], uint64(item.due+500))
						}
						binary.BigEndian.PutUint64(item.key[16:], nextID)
						nextID++
						h.pushCandidate(item, shift)
					}
				})
				b.Run(fmt.Sprintf("%s/n=%d/ties=%t/build-drain", kind, count, ties), func(b *testing.B) {
					shift := candidateHeapShift(kind)
					h := make(candidateHeap, 0, count)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						for _, item := range items {
							h.pushCandidate(item, shift)
						}
						for len(h) > 0 {
							h.popCandidate(shift)
						}
					}
				})
			}
		}
	}
}
