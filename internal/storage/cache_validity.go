package storage

import (
	"fmt"
	"io"
	"math/bits"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/futureq-io/futureq/internal/config"
)

// Options configures tracking shared by both built-in storage engines.
// Zero slots and an empty hash use the defaults for direct constructor callers.
type Options struct {
	PayloadCacheSlots int
	PayloadCacheHash  string
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

// CachedReader optionally returns a freshness proof captured BEFORE reading.
// A proof captured after Get could incorrectly validate bytes raced by a write.
// The value and closer have the same ownership rules as DB.Get.
type CachedReader interface {
	GetWithCacheToken(key []byte) ([]byte, io.Closer, CacheToken, error)
}

// CacheRevisioner exposes payload mutation progress for speculative scan backoff.
// Both beginning and completing a payload commit advance this revision.
type CacheRevisioner interface {
	CacheRevision() uint64
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
	slotHash func([]byte, uint32) uint32 // selected once at open; shared by reads and writes
}

func newCacheVersions(options ...Options) (*cacheVersions, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("storage accepts at most one Options value")
	}
	slots := config.DefaultPayloadCacheSlots
	algorithm := config.DefaultPayloadCacheHash
	if len(options) == 1 && options[0].PayloadCacheSlots != 0 {
		slots = options[0].PayloadCacheSlots
	}
	if len(options) == 1 && options[0].PayloadCacheHash != "" {
		algorithm = options[0].PayloadCacheHash
	}

	if slots < config.MinPayloadCacheSlots || slots > config.MaxPayloadCacheSlots || slots&(slots-1) != 0 {
		return nil, fmt.Errorf("payload cache slots must be a power of two from %d to %d", config.MinPayloadCacheSlots, config.MaxPayloadCacheSlots)
	}
	slotHash := cacheSlot
	switch algorithm {
	case config.PayloadCacheHashFNV:
	case config.PayloadCacheHashXXHash64:
		slotHash = cacheSlotXXHash64
	default:
		return nil, fmt.Errorf("payload cache hash must be fnv or xxhash64, got %q", algorithm)
	}

	return &cacheVersions{
		slots: make([]uint64, slots), pending: make([]uint32, slots), mask: uint32(slots - 1),
		slotHash: slotHash,
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
	return CacheToken{versions: s.owner, slot: s.owner.slotHash(key, s.owner.mask), sequence: s.sequence}
}

func cacheSlot(key []byte, mask uint32) uint32 {
	h := uint64(14695981039346656037)
	for _, b := range key {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return uint32(h) & mask
}

func cacheSlotXXHash64(key []byte, mask uint32) uint32 {
	return uint32(xxhash.Sum64(key)) & mask
}

// A lazily allocated bitmap bounds batch metadata to one bit per slot.
// Repeated keys and hash collisions set the same bit without sorting.
type cacheMutations []uint64

func (m *cacheMutations) add(key []byte, versions *cacheVersions) {
	if len(key) == 24 && versions != nil {
		if len(*m) == 0 {
			*m = make(cacheMutations, len(versions.slots)/64)
		}
		slot := versions.slotHash(key, versions.mask)
		(*m)[slot/64] |= uint64(1) << (slot % 64)
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
			slot := i*64 + bits.TrailingZeros64(word)
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
				slot := i*64 + bits.TrailingZeros64(word)
				v.pending[slot]--
				v.slots[slot] = v.sequence
				word &= word - 1
			}
		}
		v.mu.Unlock()
	}
}

func (v *cacheVersions) revision() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sequence
}
