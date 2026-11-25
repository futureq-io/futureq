package raft

import (
	"encoding/binary"

	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/pkg/utils"
)

const receiptCacheBlockSize = 1024

type cachedReceipt struct {
	key   string
	state *DeliveryState
	bytes int64
	older int32 // slot index plus one, with zero as the end of the list
	newer int32
}

// Raft apply and snapshot recovery own this cache. Event IDs advance in apply
// order. The low ID bits give constant-time lookup with a full-key check.
// Slot links track admission order for eviction under the byte budget.
type receiptCache struct {
	blocks        [][]cachedReceipt
	maxBytes      int64
	maxEntries    int
	bytes         int64
	entries       int
	oldest        int32
	newest        int32
	slotEvictions int
	byteEvictions int
	budgetRejects int
}

func newReceiptCache(maxBytes int64, maxEntries int) receiptCache {
	if maxBytes == 0 {
		return receiptCache{}
	}
	return receiptCache{
		blocks:     make([][]cachedReceipt, (maxEntries+receiptCacheBlockSize-1)/receiptCacheBlockSize),
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
	}
}

func receiptCacheSlot(key string, maxEntries int) int {
	id := binary.BigEndian.Uint64([]byte(key[utils.EventKeySize-8:]))
	return int(id & uint64(maxEntries-1))
}

func (c *receiptCache) get(key string) *DeliveryState {
	if c.maxBytes == 0 || len(key) != utils.EventKeySize {
		return nil
	}
	slot := receiptCacheSlot(key, c.maxEntries)
	block := c.blocks[slot/receiptCacheBlockSize]
	if block == nil || block[slot%receiptCacheBlockSize].key != key {
		return nil
	}
	return block[slot%receiptCacheBlockSize].state
}

func (c *receiptCache) commit(batch *DeliveryBatch) {
	if c.maxBytes > 0 {
		bytesBefore, entriesBefore := c.bytes, c.entries
		// Free space from committed deletes before other changes can evict live receipts.
		for key, state := range batch.states {
			if state == nil {
				c.store(key, nil)
			}
		}
		for key, state := range batch.states {
			if state != nil {
				c.store(key, state)
			}
		}
		if c.bytes != bytesBefore || c.entries != entriesBefore {
			c.observeSize()
		}
		if c.slotEvictions > 0 {
			metrics.DeliveryApplyReceiptCacheEvictionsTotal.WithLabelValues("slot_collision").Add(float64(c.slotEvictions))
		}
		if c.byteEvictions > 0 {
			metrics.DeliveryApplyReceiptCacheEvictionsTotal.WithLabelValues("byte_limit").Add(float64(c.byteEvictions))
		}
		if c.budgetRejects > 0 {
			metrics.DeliveryApplyReceiptCacheBudgetRejectionsTotal.Add(float64(c.budgetRejects))
		}
		c.slotEvictions, c.byteEvictions, c.budgetRejects = 0, 0, 0
	}
	if batch.receiptHits > 0 {
		metrics.DeliveryApplyReceiptLookupsTotal.WithLabelValues("cache").Add(float64(batch.receiptHits))
	}
	if batch.receiptMisses > 0 {
		metrics.DeliveryApplyReceiptLookupsTotal.WithLabelValues("pebble").Add(float64(batch.receiptMisses))
	}
}

func (c *receiptCache) store(key string, state *DeliveryState) {
	if c.maxBytes == 0 || len(key) != utils.EventKeySize {
		return
	}
	slot := receiptCacheSlot(key, c.maxEntries)
	blockIndex, index := slot/receiptCacheBlockSize, slot%receiptCacheBlockSize
	block := c.blocks[blockIndex]
	var old *cachedReceipt
	if block != nil {
		old = &block[index]
	}
	sameKey := old != nil && old.key == key
	if state == nil {
		if sameKey {
			c.remove(old)
		}
		return
	}

	// Account for the retained key and decoded roster. The slot blocks have
	// their own limit and are allocated only when the first receipt fits.
	size := 80 + int64(len(key)) + int64(cap(state.Acknowledged)) + 16*int64(cap(state.Recipients))
	for _, recipient := range state.Recipients {
		size += int64(len(recipient))
	}
	if size > c.maxBytes {
		c.budgetRejects++
		if sameKey {
			// A committed update must invalidate the previous cached state.
			c.remove(old)
			c.byteEvictions++
		}
		return
	}
	if old != nil && old.state != nil {
		if sameKey && size == old.bytes {
			old.state = state
			return
		}
		c.remove(old)
		if !sameKey {
			c.slotEvictions++
		}
	}
	for size > c.maxBytes-c.bytes {
		c.remove(c.entry(c.oldest))
		c.byteEvictions++
	}

	if block == nil {
		block = make([]cachedReceipt, min(receiptCacheBlockSize, c.maxEntries-blockIndex*receiptCacheBlockSize))
		c.blocks[blockIndex] = block
	}
	link := int32(slot + 1)
	block[index] = cachedReceipt{key: key, state: state, bytes: size, older: c.newest}
	if c.newest == 0 {
		c.oldest = link
	} else {
		c.entry(c.newest).newer = link
	}
	c.newest = link
	c.bytes += size
	c.entries++
}

func (c *receiptCache) entry(link int32) *cachedReceipt {
	slot := int(link - 1)
	return &c.blocks[slot/receiptCacheBlockSize][slot%receiptCacheBlockSize]
}

func (c *receiptCache) remove(entry *cachedReceipt) {
	if entry.older == 0 {
		c.oldest = entry.newer
	} else {
		c.entry(entry.older).newer = entry.newer
	}
	if entry.newer == 0 {
		c.newest = entry.older
	} else {
		c.entry(entry.newer).older = entry.older
	}
	c.bytes -= entry.bytes
	c.entries--
	*entry = cachedReceipt{}
}

func (c *receiptCache) reset() {
	clear(c.blocks)
	c.bytes, c.entries = 0, 0
	c.oldest, c.newest = 0, 0
	c.slotEvictions, c.byteEvictions, c.budgetRejects = 0, 0, 0
	c.observeSize()
}

func (c *receiptCache) observeSize() {
	metrics.DeliveryApplyReceiptCacheRetainedBytes.Set(float64(c.bytes))
	metrics.DeliveryApplyReceiptCacheEntries.Set(float64(c.entries))
}
