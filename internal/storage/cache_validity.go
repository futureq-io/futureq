package storage

import (
	"fmt"
	"math/bits"
	"sync"

	"github.com/futureq-io/futureq/internal/config"
)

// Options configures tracking shared by both built-in storage engines.
// A zero PayloadCacheSlots uses the default for direct constructor callers.
type Options struct {
	PayloadCacheSlots int
}

// CacheToken proves a value from a snapshot has not been changed since that
// snapshot. A zero token is unsupported/uncertain and requires a fresh Get.
// Validation is the freshness point, equivalent to the former final Get: a
// mutation starting after validation can still race enqueue, just as before.
type CacheToken struct {
	versions *cacheVersions
	slot     uint32
	sequence uint64
}

func (t CacheToken) Valid() bool {
	if t.versions == nil {
		return false
	}
	t.versions.mu.Lock()
	defer t.versions.mu.Unlock()
	return t.versions.pending[t.slot] == 0 && t.versions.slots[t.slot] <= t.sequence
}

// Invalidated distinguishes a known obsolete snapshot from an unsupported
// proof. Discovery rejects obsolete results; unsupported engines use Get.
func (t CacheToken) Invalidated() bool { return t.versions != nil && !t.Valid() }

// CachedIterator is optional. Wrappers and third-party engines that do not
// expose it safely retain the ordinary payload-read fallback.
type CachedIterator interface {
	CacheToken(key []byte) CacheToken
}

// Fixed-at-open striped versions bound metadata independently of key cardinality.
// Collisions cause conservative rereads, never false validity. Only event keys
// (24 bytes) change these versions; manifest preparation cannot invalidate data.
// No tracker lock spans storage I/O. All mutations invalidate BEFORE committing.
// Pending commits make their affected snapshot slots uncertain. Failed commits
// conservatively invalidate old proofs too.
type cacheVersions struct {
	mu       sync.Mutex
	slots    []uint64
	pending  []uint32
	sequence uint64
	mask     uint32
}

func newCacheVersions(options ...Options) (*cacheVersions, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("storage accepts at most one Options value")
	}
	slots := config.DefaultPayloadCacheSlots
	if len(options) == 1 && options[0].PayloadCacheSlots != 0 {
		slots = options[0].PayloadCacheSlots
	}

	return &cacheVersions{
		slots: make([]uint64, slots), pending: make([]uint32, slots), mask: uint32(slots - 1),
	}, nil
}

// Capture an ordered sequence before opening the engine snapshot. Mutations
// advance affected slots at BOTH begin and end. Thus a mutation overlapping
// snapshot capture cannot validate, even if it finishes before token creation.
// Unrelated slots remain usable, with no version-array copy per iterator.
type cacheSnapshot struct {
	owner    *cacheVersions
	sequence uint64
}

func (v *cacheVersions) snapshot() *cacheSnapshot {
	v.mu.Lock()
	defer v.mu.Unlock()
	return &cacheSnapshot{owner: v, sequence: v.sequence}
}

func (s *cacheSnapshot) token(key []byte) CacheToken {
	if s == nil || len(key) != 24 {
		return CacheToken{}
	}
	return CacheToken{versions: s.owner, slot: cacheSlot(key, s.owner.mask), sequence: s.sequence}
}

func cacheSlot(key []byte, mask uint32) uint32 {
	h := uint64(14695981039346656037)
	for _, b := range key {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return uint32(h) & mask
}

type cacheMutations []uint64

// Metadata-only batches do not need a mutation bitmap.
func (m *cacheMutations) add(key []byte, versions *cacheVersions) {
	if len(key) == 24 && versions != nil {
		if len(*m) == 0 {
			*m = make(cacheMutations, len(versions.slots)/64)
		}
		slot := cacheSlot(key, versions.mask)
		(*m)[slot/64] |= 1 << (slot % 64)
	}
}

func (v *cacheVersions) begin(m *cacheMutations) func() {
	if len(*m) == 0 {
		return func() {}
	}
	v.mu.Lock()
	v.sequence++
	for i, word := range *m {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			slot := i*64 + bit
			v.slots[slot] = v.sequence
			v.pending[slot]++
			word &= word - 1
		}
	}

	v.mu.Unlock()
	return func() {
		v.mu.Lock()
		v.sequence++
		for i, word := range *m {
			for word != 0 {
				bit := bits.TrailingZeros64(word)
				slot := i*64 + bit
				v.pending[slot]--
				v.slots[slot] = v.sequence
				word &= word - 1
			}
		}
		v.mu.Unlock()
	}
}
