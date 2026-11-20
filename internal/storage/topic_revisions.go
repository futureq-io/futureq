package storage

import (
	"encoding/binary"
	"sync"
)

const retainedTopicRevisions = 4096

// Track topics observed by readers. Writes to unobserved topics do not evict
// entries. An evicted topic gets a fresh token when next observed, so a
// stale reader cannot reuse a hint across an eviction and an unseen write.
type topicRevisions struct {
	mu       sync.RWMutex
	versions map[uint64]uint64
	order    []uint64
	next     int
	sequence uint64
}

func (r *topicRevisions) revision(hash uint64) uint64 {
	r.mu.RLock()
	version, ok := r.versions[hash]
	r.mu.RUnlock()
	if ok {
		return version
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if version, ok := r.versions[hash]; ok {
		return version
	}
	if r.versions == nil {
		r.versions = make(map[uint64]uint64)
	}
	if len(r.order) < retainedTopicRevisions {
		r.order = append(r.order, hash)
	} else {
		delete(r.versions, r.order[r.next])
		r.order[r.next] = hash
		r.next = (r.next + 1) % retainedTopicRevisions
	}
	r.sequence++
	r.versions[hash] = r.sequence
	return r.sequence
}

// Advance after a successful commit. The single-topic batch path allocates
// no map, and a multi-topic batch advances each observed topic at most once.
type eventTopicWrites struct {
	first  uint64
	set    bool
	others map[uint64]struct{}
}

func (w *eventTopicWrites) add(key []byte) {
	if len(key) != 24 {
		return
	}
	hash := binary.BigEndian.Uint64(key[:8])
	if !w.set {
		w.first, w.set = hash, true
	} else if hash != w.first {
		if w.others == nil {
			w.others = make(map[uint64]struct{})
		}
		w.others[hash] = struct{}{}
	}
}

func (r *topicRevisions) commit(w eventTopicWrites) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	if _, ok := r.versions[w.first]; ok {
		r.versions[w.first] = r.sequence
	}
	for hash := range w.others {
		if _, ok := r.versions[hash]; ok {
			r.versions[hash] = r.sequence
		}
	}
}
