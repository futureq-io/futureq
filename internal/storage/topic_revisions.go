package storage

import (
	"encoding/binary"
	"math/bits"
	"sync"

	"github.com/futureq-io/futureq/pkg/utils"
)

const shardSlots = 2 * utils.MaxVirtualShards

// TopicRevisionSnapshot records event writes by storage slot. Slot zero
// tracks unsharded keys. Capture it before opening the matching DB snapshot.
type TopicRevisionSnapshot struct {
	Version        uint64
	WrittenLayouts int // bitmask of shard counts written during this watch
	Shards         [shardSlots]uint64
}

type topicRevisions struct {
	mu       sync.RWMutex
	active   map[uint64]*topicRevisionState
	sequence uint64
}

type topicRevisionState struct {
	version        uint64
	watchers       int
	writtenLayouts int
	shards         *[shardSlots]uint64
}

// TopicRevision retains a topic's revision state until Close. Callers own
// independent handles even when observing the same topic.
type TopicRevision struct {
	revisions *topicRevisions
	hash      uint64
	state     *topicRevisionState
}

func (r *topicRevisions) watch(hash uint64) *TopicRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = make(map[uint64]*topicRevisionState)
	}
	state := r.active[hash]
	if state == nil {
		r.sequence++
		state = &topicRevisionState{version: r.sequence}
		r.active[hash] = state
	}
	state.watchers++
	return &TopicRevision{revisions: r, hash: hash, state: state}
}

// Read returns the topic version. If dst is supplied and its version has
// changed, Read copies the per-shard versions and observed layouts under the same lock.
func (t *TopicRevision) Read(dst *TopicRevisionSnapshot) uint64 {
	t.revisions.mu.RLock()
	defer t.revisions.mu.RUnlock()
	state := t.state
	if dst != nil && dst.Version != state.version {
		dst.Version = state.version
		dst.WrittenLayouts = state.writtenLayouts
		if state.shards == nil {
			clear(dst.Shards[:])
		} else {
			dst.Shards = *state.shards
		}
	}
	return state.version
}

func (t *TopicRevision) Close() {
	t.revisions.mu.Lock()
	defer t.revisions.mu.Unlock()
	if t.state != nil {
		t.state.watchers--
		if t.state.watchers == 0 {
			delete(t.revisions.active, t.hash)
			if len(t.revisions.active) == 0 {
				t.revisions.active = nil
			}
		}
		t.state = nil
	}
}

type writtenSlots [shardSlots / 64]uint64

func (s *writtenSlots) add(slot int) {
	s[slot/64] |= 1 << (slot % 64)
}

// Inline the first topic's slots so single-topic batches need no map.
type eventTopicWrites struct {
	first  uint64
	slots  writtenSlots
	set    bool
	others map[uint64]writtenSlots
}

func (w *eventTopicWrites) add(key []byte) {
	if len(key) != 24 {
		return
	}
	hash := binary.BigEndian.Uint64(key[:8])
	slot := utils.EventShardSlot(key)
	if !w.set {
		w.first, w.set = hash, true
	}
	if hash == w.first {
		w.slots.add(slot)
		return
	}
	if w.others == nil {
		w.others = make(map[uint64]writtenSlots)
	}
	slots := w.others[hash]
	slots.add(slot)
	w.others[hash] = slots
}

func (r *topicRevisions) commit(w eventTopicWrites) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	if state := r.active[w.first]; state != nil {
		state.record(r.sequence, w.slots)
	}
	for hash, slots := range w.others {
		if state := r.active[hash]; state != nil {
			state.record(r.sequence, slots)
		}
	}
}

func (s *topicRevisionState) record(version uint64, slots writtenSlots) {
	s.version = version
	if s.shards == nil {
		s.shards = new([shardSlots]uint64)
	}
	for i, word := range slots {
		for word != 0 {
			slot := i*64 + bits.TrailingZeros64(word)
			s.shards[slot] = version
			if slot >= 2 {
				s.writtenLayouts |= utils.SlotShardCount(slot)
			}
			word &= word - 1
		}
	}
}
