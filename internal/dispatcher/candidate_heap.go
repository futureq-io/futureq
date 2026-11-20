package dispatcher

import (
	"bytes"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/metrics"
)

const (
	binaryHeapShift = 1
	quadHeapShift   = 2

	// Separate thresholds avoid rebuilding when a topic hovers near a cutoff.
	adaptiveHeapGrowAt   = 2048
	adaptiveHeapShrinkAt = 1024
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

func (p *topicPreload) pushCandidate(candidate preloadCandidate) {
	p.items.pushCandidate(candidate, p.heapShift)
	// Check after each push so even a large discovery result cannot make the
	// upgrade rebuild more than adaptiveHeapGrowAt entries.
	if p.adaptiveHeap && p.heapShift == binaryHeapShift && len(p.items) == adaptiveHeapGrowAt {
		p.heapShift = quadHeapShift
		p.items.initCandidates(p.heapShift)
		metrics.DeliveryHeapSwitchesTotal.WithLabelValues("4ary").Inc()
	}
}

// rebuildAfterDrain also handles filtering, which requires a rebuild even
// when the arity stays the same. Ordinary pops already maintain heap order.
func (p *topicPreload) rebuildAfterDrain(needsRebuild bool) {
	if p.adaptiveHeap && p.heapShift == quadHeapShift && len(p.items) <= adaptiveHeapShrinkAt {
		p.heapShift = binaryHeapShift
		needsRebuild = true
		metrics.DeliveryHeapSwitchesTotal.WithLabelValues("binary").Inc()
	}
	if needsRebuild {
		p.items.initCandidates(p.heapShift)
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
func (h *candidateHeap) pushCandidate(candidate preloadCandidate, shift uint) {
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
}

// Callers check for an eligible head before popping. Clear the removed slot in
// every implementation so drained backing arrays cannot retain payloads.
func (h *candidateHeap) popCandidate(shift uint) preloadCandidate {
	items := *h
	first := items[0]
	last := len(items) - 1
	replacement := items[last]
	items[last] = preloadCandidate{}
	items = items[:last]
	if last > 0 {
		items.siftCandidateDown(0, replacement, max(1, shift))
	}
	*h = items
	return first
}

func (h candidateHeap) siftCandidateDown(parent int, candidate preloadCandidate, shift uint) {
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
