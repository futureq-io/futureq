package dispatcher

import (
	"math/bits"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/metrics"
)

const (
	heapWorkScale         = 256
	heapWorkMinOperations = 256
	// The scheduler rebuilds synchronously. Keep each rebuild bounded even
	// when the retained cache is configured for much larger heaps.
	heapWorkMaxRebuild = 8192
)

// candidateHeapWork records how far real sifts move relative to the heap's
// height. Normalizing makes the same observations usable for other arities.
type candidateHeapWork struct {
	pushes, pops int
	up, down     int
	savings      int64
}

func (p *topicPreload) pushCandidate(candidate preloadCandidate) {
	landing := p.items.pushCandidate(candidate, p.heapShift)
	if p.trackHeapWork() {
		p.heapWork.pushes++
		if tail := len(p.items) - 1; landing != tail {
			depth := candidateLevel(tail, p.heapShift)
			p.heapWork.up += (depth - candidateLevel(landing, p.heapShift)) * heapWorkScale / depth
		}
		p.considerHeapShift()
	}
}

func (p *topicPreload) removeCandidateHead() {
	landing := p.items.removeHead(p.heapShift)
	if !p.adaptiveHeap {
		return
	}
	if len(p.items) == 0 {
		p.heapWork = candidateHeapWork{}
		p.setHeapShift(binaryHeapShift)
		return
	}
	if p.trackHeapWork() {
		p.heapWork.pops++
		if landing > 0 {
			height := candidateLevel(len(p.items)-1, p.heapShift)
			p.heapWork.down += candidateLevel(landing, p.heapShift) * heapWorkScale / height
		}
		p.considerHeapShift()
	}
}

func (p *topicPreload) trackHeapWork() bool {
	if !p.adaptiveHeap {
		return false
	}
	if len(p.items) > heapWorkMaxRebuild {
		p.heapWork = candidateHeapWork{}
		return false
	}
	return true
}

func (p *topicPreload) considerHeapShift() {
	n := len(p.items)
	work := p.heapWork
	if work.pushes+work.pops < max(heapWorkMinOperations, n/2) {
		return
	}
	p.heapWork = candidateHeapWork{}
	alternate := quadHeapShift
	if p.heapShift == quadHeapShift {
		alternate = binaryHeapShift
	}
	current := work.cost(n, p.heapShift)
	savings := current - work.cost(n, alternate)
	// Require a clear improvement across consecutive windows. Accumulate
	// savings until they cover the estimated rebuild work. An indecisive
	// window clears the accumulated savings.
	if savings <= current/8 {
		return
	}
	work.savings += savings
	if work.savings >= int64(2*n*heapWorkScale) {
		p.setHeapShift(alternate)
		p.items.initCandidates(p.heapShift)
	} else {
		p.heapWork.savings = work.savings
	}
}

func (p *topicPreload) setHeapShift(shift uint) {
	if p.heapShift == shift {
		return
	}
	p.heapShift = shift
	kind := config.HeapBinary
	if shift == quadHeapShift {
		kind = config.Heap4Ary
	}
	metrics.DeliveryHeapSwitchesTotal.WithLabelValues(string(kind)).Inc()
}

// Level zero contains the root. For width d, level h starts at
// (d^h - 1)/(d-1), so multiplying by d-1 gives a fast depth calculation.
func candidateLevel(index int, shift uint) int {
	width := 1 << shift
	return (bits.Len64(uint64(index)*uint64(width-1)+1) - 1) / int(shift)
}

// cost estimates comparisons and candidate moves for the observed operation
// mix projected onto this arity's populated levels. A push compares to one
// parent per level it rises. A pop scans siblings, so the deepest, partially
// occupied level contributes in proportion to its fill.
func (w candidateHeapWork) cost(n int, shift uint) int64 {
	if n < 2 {
		return 0
	}
	width := 1 << shift
	height := candidateLevel(n-1, shift)
	rootChildren := min(width, n-1)
	deeperChecks := 0
	if height > 1 {
		bottomCapacity := 1 << (height * int(shift))
		bottomStart := (bottomCapacity - 1) / (width - 1)
		deeperChecks = (height-2)*width*heapWorkScale + (n-bottomStart)*width*heapWorkScale/bottomCapacity
	}
	base := w.pushes*heapWorkScale + 2*w.up*height + w.pops*rootChildren*heapWorkScale + w.down*height
	return int64(base) + int64(w.down)*int64(deeperChecks)/heapWorkScale
}
