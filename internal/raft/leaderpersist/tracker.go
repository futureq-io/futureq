// Package leaderpersist provides the ACK_LEVEL_LEADER signal for FutureQ:
// it observes Dragonboat's LogDB layer and notifies waiting producer
// handlers as soon as a specific proposal has been persisted to the
// leader's local Raft log — before replication to a quorum completes.
//
// The mechanism is a decorator around the default tan LogDB factory. After
// each successful SaveRaftState call the wrapper hashes the Cmd bytes of
// every persisted entry and signals any registered waiter for that hash.
//
// Correlation-by-hash avoids depending on unexported Dragonboat fields
// (ClientID/SeriesID/Key on RequestState). FutureQ batch commands embed
// monotonically increasing event IDs, so two in-flight proposals never
// share a payload — collisions are not a practical concern.
package leaderpersist

import (
	"hash/maphash"
	"sync"
)

// Tracker maps hashes of pending proposal payloads to a notification
// channel. The producer handler registers a hash before calling Propose;
// the LogDB decorator calls Notify for every entry it persists.
//
// A Tracker is safe for concurrent use.
type Tracker struct {
	seed    maphash.Seed
	mu      sync.Mutex
	waiters map[uint64][]chan struct{}
}

// NewTracker returns a ready-to-use Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		seed:    maphash.MakeSeed(),
		waiters: make(map[uint64][]chan struct{}),
	}
}

// Hash returns the tracker's hash of payload. The seed is process-local,
// so hashes are not stable across restarts — which is fine, since they are
// only used to correlate in-flight proposals with in-process log writes.
func (t *Tracker) Hash(payload []byte) uint64 {
	var h maphash.Hash
	h.SetSeed(t.seed)
	_, _ = h.Write(payload)
	return h.Sum64()
}

// Register returns a channel that is closed when Notify is called with the
// same hash, and a cancel function that must be called when the waiter
// stops waiting (success or failure) to avoid leaking the registration.
//
// The channel is buffered with capacity 1 and closed by Notify, never by
// the waiter, so a Notify racing a cancel is safe.
func (t *Tracker) Register(hash uint64) (<-chan struct{}, func()) {
	ch := make(chan struct{})

	t.mu.Lock()
	t.waiters[hash] = append(t.waiters[hash], ch)
	t.mu.Unlock()

	cancel := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		waiters := t.waiters[hash]
		for i, w := range waiters {
			if w == ch {
				t.waiters[hash] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
		if len(t.waiters[hash]) == 0 {
			delete(t.waiters, hash)
		}
	}

	return ch, cancel
}

// Notify closes every channel registered for hash. It is called by the
// LogDB decorator after a batch of entries containing this payload hash
// has been durably written to the leader's local Raft log.
func (t *Tracker) Notify(hash uint64) {
	t.mu.Lock()
	waiters := t.waiters[hash]
	delete(t.waiters, hash)
	t.mu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}
}
