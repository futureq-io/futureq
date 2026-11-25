package raft

import (
	"encoding/binary"

	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/pkg/utils"
)

const (
	receiptCacheBlockSize = 1024
	receiptCacheBlocks    = 512
	receiptCacheSlots     = receiptCacheBlockSize * receiptCacheBlocks
	receiptCacheMaxBytes  = 64 << 20
)

type cachedReceipt struct {
	key   string
	state *DeliveryState
	bytes int
}

// Raft apply and snapshot recovery own this cache. Event IDs advance in apply
// order, so using the low ID bits retains recent receipts without a linked list
// or a per-lookup eviction update. Compare the full key on every lookup.
type receiptCache struct {
	blocks [receiptCacheBlocks]*[receiptCacheBlockSize]cachedReceipt
	bytes  int
}

func receiptCacheSlot(key string) int {
	id := binary.BigEndian.Uint64([]byte(key[utils.EventKeySize-8:]))
	return int(id & (receiptCacheSlots - 1))
}

func (c *receiptCache) get(key string) *DeliveryState {
	if len(key) != utils.EventKeySize {
		return nil
	}
	slot := receiptCacheSlot(key)
	block := c.blocks[slot/receiptCacheBlockSize]
	if block == nil || block[slot%receiptCacheBlockSize].key != key {
		return nil
	}
	return block[slot%receiptCacheBlockSize].state
}

func (c *receiptCache) commit(batch *DeliveryBatch) {
	for key, state := range batch.states {
		c.store(key, state)
	}
	if batch.receiptHits > 0 {
		metrics.DeliveryApplyReceiptLookupsTotal.WithLabelValues("cache").Add(float64(batch.receiptHits))
	}
	if batch.receiptMisses > 0 {
		metrics.DeliveryApplyReceiptLookupsTotal.WithLabelValues("pebble").Add(float64(batch.receiptMisses))
	}
}

func (c *receiptCache) store(key string, state *DeliveryState) {
	if len(key) != utils.EventKeySize {
		return
	}
	slot := receiptCacheSlot(key)
	blockIndex, index := slot/receiptCacheBlockSize, slot%receiptCacheBlockSize
	block := c.blocks[blockIndex]
	if state == nil {
		if block != nil && block[index].key == key {
			c.bytes -= block[index].bytes
			block[index] = cachedReceipt{}
		}
		return
	}
	// Account for the retained key and decoded state, including its variable
	// roster. The lazily allocated slot blocks have a separate fixed bound.
	size := 80 + len(key) + cap(state.Acknowledged) + 16*cap(state.Recipients)
	for _, recipient := range state.Recipients {
		size += len(recipient)
	}
	if block != nil {
		// Invalidate old state even if its replacement exceeds the budget.
		c.bytes -= block[index].bytes
		block[index] = cachedReceipt{}
	}
	if size > receiptCacheMaxBytes-c.bytes {
		return
	}
	if block == nil {
		block = new([receiptCacheBlockSize]cachedReceipt)
		c.blocks[blockIndex] = block
	}
	block[index] = cachedReceipt{key: key, state: state, bytes: size}
	c.bytes += size
}
