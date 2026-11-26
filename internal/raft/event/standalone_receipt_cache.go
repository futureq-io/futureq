package raft

import (
	"sync"

	"github.com/futureq-io/futureq/internal/storage"
)

const (
	// Estimated cost of retaining one event key in the pending map.
	pendingReceiptKeyBytes = 64
	retainPendingKeys      = 1024
)

// StandaloneReceiptCache uses the same cache implementation as Raft apply.
// Its lock covers in-memory bookkeeping. The ledger orders receipt mutations
// through storage commit and the corresponding cache update.
type StandaloneReceiptCache struct {
	mu           sync.Mutex
	cache        receiptCache
	pending      map[string]struct{}
	pendingLimit int
	pendingPeak  int
}

func NewStandaloneReceiptCache(maxBytes int64, maxEntries int) *StandaloneReceiptCache {
	return &StandaloneReceiptCache{
		cache:        newReceiptCache(maxBytes, maxEntries),
		pendingLimit: int(min(int64(maxEntries), maxBytes/pendingReceiptKeyBytes)),
	}
}

// BeginPublish records the new keys before the storage commit. A concurrent
// ACK or delete can invalidate these keys while the publish waits for sync. If
// tracking is full, the caller can commit and skip cache admission.
func (c *StandaloneReceiptCache) BeginPublish(states map[string]*DeliveryState) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(states) > c.pendingLimit-len(c.pending) {
		return false
	}
	if c.pending == nil {
		c.pending = make(map[string]struct{}, len(states))
	}
	for key := range states {
		c.pending[key] = struct{}{}
	}
	c.pendingPeak = max(c.pendingPeak, len(c.pending))
	return true
}

// FinishPublish admits successful publishes whose keys were not touched by a
// concurrent receipt mutation. Each publish reserves its own event keys.
func (c *StandaloneReceiptCache) FinishPublish(states map[string]*DeliveryState, committed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range states {
		_, pending := c.pending[key]
		delete(c.pending, key)
		if !pending {
			delete(states, key)
		}
	}
	if committed {
		c.cache.commit(&DeliveryBatch{states: states})
	}
	c.trimPending()
}

// Keep a small map for steady traffic and reclaim storage after bursts, even
// when some publishes are still in flight.
func (c *StandaloneReceiptCache) trimPending() {
	n := len(c.pending)
	if c.pendingPeak <= retainPendingKeys || n > c.pendingPeak/4 {
		return
	}
	var smaller map[string]struct{}
	if n > 0 {
		smaller = make(map[string]struct{}, n)
		for key := range c.pending {
			smaller[key] = struct{}{}
		}
	}
	c.pending = smaller
	c.pendingPeak = n
}

func (c *StandaloneReceiptCache) get(key string) *DeliveryState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cache.get(key)
}

func (c *StandaloneReceiptCache) NewBatch(db storage.DB, batch storage.Batch) *DeliveryBatch {
	delivery := NewDeliveryBatch(db, batch)
	delivery.standaloneReceipts = c
	return delivery
}

func (c *StandaloneReceiptCache) CommitBatch(batch *DeliveryBatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) > 0 {
		for key := range batch.states {
			delete(c.pending, key)
		}
		c.trimPending()
	}
	c.cache.commit(batch)
}

func (c *StandaloneReceiptCache) Delete(keys [][]byte) {
	states := make(map[string]*DeliveryState, len(keys))
	for _, key := range keys {
		states[string(key)] = nil
	}
	c.CommitBatch(&DeliveryBatch{states: states})
}
