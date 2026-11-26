package raft

import "github.com/futureq-io/futureq/internal/storage"

// StandaloneReceiptCache shares the receipt cache used by Raft apply. The
// standalone ledger serializes these methods with its receipt mutation lock.
type StandaloneReceiptCache struct {
	cache   receiptCache
	pending map[string]struct{}
}

func NewStandaloneReceiptCache(maxBytes int64, maxEntries int) *StandaloneReceiptCache {
	return &StandaloneReceiptCache{
		cache:   newReceiptCache(maxBytes, maxEntries),
		pending: make(map[string]struct{}),
	}
}

// BeginPublish records the new keys before the storage commit. A concurrent
// ACK or delete can invalidate these keys while the publish waits for sync.
func (c *StandaloneReceiptCache) BeginPublish(states map[string]*DeliveryState) {
	for key := range states {
		c.pending[key] = struct{}{}
	}
}

// FinishPublish admits successful publishes whose keys were not touched by a
// concurrent receipt mutation. Each publish reserves its own event keys.
func (c *StandaloneReceiptCache) FinishPublish(states map[string]*DeliveryState, committed bool) {
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
}

func (c *StandaloneReceiptCache) NewBatch(db storage.DB, batch storage.Batch) *DeliveryBatch {
	delivery := NewDeliveryBatch(db, batch)
	delivery.receipts = &c.cache
	return delivery
}

func (c *StandaloneReceiptCache) CommitBatch(batch *DeliveryBatch) {
	if len(c.pending) > 0 {
		for key := range batch.states {
			delete(c.pending, key)
		}
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
