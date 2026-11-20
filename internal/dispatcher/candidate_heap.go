package dispatcher

import (
	"bytes"

	"github.com/futureq-io/futureq/internal/config"
)

const (
	binaryHeapShift uint = 1
	quadHeapShift   uint = 2
)

// candidateHeapShift returns the initial arity, also used for worker-local
// discovery heaps. Only the scheduler's retained heaps adapt.
func candidateHeapShift(kind config.HeapType) uint {
	switch kind {
	case "", config.HeapBinary, config.HeapAuto:
		return binaryHeapShift
	case config.Heap4Ary:
		return quadHeapShift
	case config.Heap8Ary:
		return 3
	default:
		panic("invalid delivery heap type: " + string(kind)) // configuration is validated before startup
	}
}

func candidateBefore(a, b preloadCandidate) bool {
	if a.due == b.due {
		return bytes.Compare(a.key[:], b.key[:]) < 0
	}
	return a.due < b.due
}

// All heaps use the same entries and ordering. A shift
// selects 2, 4, or 8 children without division in the sift loops. Hole-based
// sifting moves one entry per level rather than swapping two full candidates.
func (h *candidateHeap) pushCandidate(candidate preloadCandidate, shift uint) int {
	shift = max(1, shift) // zero-value topic heaps also use typed binary operations
	items := append(*h, candidate)
	child := len(items) - 1
	for child > 0 {
		parent := (child - 1) >> shift
		if !candidateBefore(candidate, items[parent]) {
			break
		}
		items[child] = items[parent]
		child = parent
	}
	items[child] = candidate
	*h = items
	return child
}

// Callers check for an eligible head before popping. Clear the removed slot in
// every implementation so drained backing arrays cannot retain payloads.
func (h *candidateHeap) popCandidate(shift uint) preloadCandidate {
	first := (*h)[0]
	h.removeHead(shift)
	return first
}

// removeHead returns the index where the replacement landed after sifting.
func (h *candidateHeap) removeHead(shift uint) int {
	items := *h
	last := len(items) - 1
	replacement := items[last]
	items[last] = preloadCandidate{}
	items = items[:last]
	landing := 0
	if last > 0 {
		landing = items.siftCandidateDown(0, replacement, max(1, shift))
	}
	*h = items
	return landing
}

func (h candidateHeap) siftCandidateDown(parent int, candidate preloadCandidate, shift uint) int {
	for {
		child := (parent << shift) + 1
		if child >= len(h) {
			break
		}
		end := min(child+(1<<shift), len(h))
		for sibling := child + 1; sibling < end; sibling++ {
			if candidateBefore(h[sibling], h[child]) {
				child = sibling
			}
		}
		if !candidateBefore(h[child], candidate) {
			break
		}
		h[parent] = h[child]
		parent = child
	}
	h[parent] = candidate
	return parent
}

// Rebuild in place after filtering ready candidates or changing the arity.
// Copying during compaction alone preserves heap order.
func (h *candidateHeap) initCandidates(shift uint) {
	shift = max(1, shift)
	if len(*h) < 2 {
		return
	}
	for parent := (len(*h) - 2) >> shift; parent >= 0; parent-- {
		(*h).siftCandidateDown(parent, (*h)[parent], shift)
	}
}
