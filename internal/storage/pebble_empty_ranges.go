package storage

import (
	"bytes"
	"sync"
)

const (
	maxPebbleEmptyRanges = 64
	maxPebbleEmptyProbes = 64
	maxPebbleEmptyKeyLen = 256
)

// Empty prefixes are negative knowledge, not delivery watermarks. A cyclic
// scan still starts at its requested lower bound, but can seek past keys proven
// absent. This avoids repeatedly walking historical point tombstones in First.
// All Sets go through begin/end; Deletes cannot introduce a key into a gap.
// The cache is disposable, bounded, and never changes the database on disk.
type pebbleEmptyRanges struct {
	mu      sync.Mutex
	ranges  []pebbleKeySpan
	probes  map[*pebbleEmptyProbe]struct{}
	pending map[*pebbleKeySpan]struct{}
	evict   int
}

type pebbleKeySpan struct{ lower, upper []byte } // [lower, upper)

// One conservative span per batch bounds write bookkeeping independently of
// batch size. A batch spanning several topics may invalidate extra gaps.
func (s *pebbleKeySpan) include(key []byte) {
	if s.upper == nil || bytes.Compare(key, s.lower) < 0 {
		s.lower = append(s.lower[:0], key...)
	}
	if s.upper == nil || bytes.Compare(key, s.upper) >= 0 {
		s.upper = append(append(s.upper[:0], key...), 0)
	}
}

type pebbleEmptyProbe struct {
	lower, upper []byte
	changed      []byte // earliest possibly inserted key since snapshot capture
}

// Called with mu held across snapshot capture. Pending commits must taint a
// new probe too: their begin invalidation may predate this snapshot.
func (c *pebbleEmptyRanges) capture(opts *IterOptions) (*pebbleEmptyProbe, []byte) {
	if opts == nil || len(opts.LowerBound) == 0 || len(opts.LowerBound) > maxPebbleEmptyKeyLen || len(opts.UpperBound) > maxPebbleEmptyKeyLen {
		return nil, nil
	}
	lower := opts.LowerBound
	for _, gap := range c.ranges {
		if bytes.Compare(lower, gap.lower) >= 0 && bytes.Compare(lower, gap.upper) < 0 {
			lower = gap.upper
			if opts.UpperBound != nil && bytes.Compare(lower, opts.UpperBound) > 0 {
				lower = opts.UpperBound
			}
			break
		}
	}
	if len(c.probes) >= maxPebbleEmptyProbes {
		return nil, lower // ordinary snapshot iterator; skip learning under pressure
	}
	p := &pebbleEmptyProbe{lower: bytes.Clone(opts.LowerBound), upper: bytes.Clone(opts.UpperBound)}
	if c.probes == nil {
		c.probes = make(map[*pebbleEmptyProbe]struct{})
	}
	c.probes[p] = struct{}{}
	for span := range c.pending {
		p.invalidate(span)
	}
	return p, lower
}

func (p *pebbleEmptyProbe) invalidate(s *pebbleKeySpan) {
	if bytes.Compare(s.upper, p.lower) <= 0 || (p.upper != nil && bytes.Compare(s.lower, p.upper) >= 0) {
		return
	}
	key := s.lower
	if bytes.Compare(key, p.lower) < 0 {
		key = p.lower
	}
	if p.changed == nil || bytes.Compare(key, p.changed) < 0 {
		p.changed = bytes.Clone(key)
	}
}

func (c *pebbleEmptyRanges) invalidate(s *pebbleKeySpan) {
	kept := c.ranges[:0]
	for _, gap := range c.ranges {
		if bytes.Compare(s.upper, gap.lower) <= 0 || bytes.Compare(s.lower, gap.upper) >= 0 {
			kept = append(kept, gap)
		} else if bytes.Compare(gap.lower, s.lower) < 0 {
			// Preserve the historical prefix when publishers insert into the
			// moving tail of an empty lookahead range. Discarding the whole gap
			// would force another tombstone walk on every publish.
			kept = append(kept, pebbleKeySpan{lower: gap.lower, upper: bytes.Clone(s.lower)})
		}
	}
	clear(c.ranges[len(kept):])
	c.ranges = kept
	for probe := range c.probes {
		probe.invalidate(s)
	}
}

// Invalidate both before and after the actual commit, even on failure. No cache
// lock spans storage I/O. Old iterators must not repopulate a gap raced by a Set.
func (c *pebbleEmptyRanges) begin(s *pebbleKeySpan) func() {
	if s.upper == nil {
		return func() {}
	}
	c.mu.Lock()
	c.invalidate(s)
	if c.pending == nil {
		c.pending = make(map[*pebbleKeySpan]struct{})
	}
	c.pending[s] = struct{}{}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.invalidate(s)
		delete(c.pending, s)
		c.mu.Unlock()
	}
}

// First proves [requested lower, first visible key) empty in its snapshot.
// When exhausted, the proof ends at the requested exclusive upper bound.
func (c *pebbleEmptyRanges) finish(p *pebbleEmptyProbe, end []byte) {
	if p == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.probes, p)
	if end == nil || len(end) > maxPebbleEmptyKeyLen {
		return
	}
	if p.changed != nil && bytes.Compare(p.changed, end) < 0 {
		end = p.changed
	}
	if bytes.Compare(p.lower, end) >= 0 {
		return
	}
	gap := pebbleKeySpan{lower: p.lower, upper: bytes.Clone(end)}
	kept := c.ranges[:0]
	for _, old := range c.ranges {
		if bytes.Compare(gap.upper, old.lower) < 0 || bytes.Compare(old.upper, gap.lower) < 0 {
			kept = append(kept, old)
			continue
		}
		if bytes.Compare(old.lower, gap.lower) < 0 {
			gap.lower = old.lower
		}
		if bytes.Compare(old.upper, gap.upper) > 0 {
			gap.upper = old.upper
		}
	}
	clear(c.ranges[len(kept):])
	c.ranges = kept
	if len(c.ranges) < maxPebbleEmptyRanges {
		c.ranges = append(c.ranges, gap)
	} else {
		c.ranges[c.evict%len(c.ranges)] = gap
		c.evict = (c.evict + 1) % maxPebbleEmptyRanges
	}
}
