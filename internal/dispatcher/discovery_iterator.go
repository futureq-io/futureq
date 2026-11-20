package dispatcher

import (
	"bytes"
	"time"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
)

// discoveryIterator uses one storage snapshot for a topic. Ordered seeks skip
// remote shards and out-of-window time buckets without visiting their payloads.
type discoveryIterator struct {
	storage.Iterator
	view                 InterestView
	hash, earliest, last uint64
	deadline             time.Time
	upper                []byte
	owned                shardSlotRanges
	loadedOwnership      bool
	seeks                int
	resumeAt             []byte
	shards               *shardScanRange
}

func openDiscoveryIterator(db storage.IteratorSource, view InterestView, hash uint64, lower, upper []byte, earliest, last uint64, deadline time.Time, shards *shardScanRange) (*discoveryIterator, error) {
	if view.sharded && len(upper) == 0 {
		// Include every possible layout, including when the topic hash is MaxUint64.
		upper = topicScanEnd(hash)
	}
	if shards != nil {
		lower, upper = shards.bounds(hash, lower, upper)
	}
	iter, err := db.NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	return &discoveryIterator{Iterator: iter, view: view, hash: hash, earliest: earliest, last: last, deadline: deadline, upper: upper, shards: shards}, nil
}

func (i *discoveryIterator) First() bool { return i.advance(i.Iterator.First()) }
func (i *discoveryIterator) Next() bool  { return i.advance(i.Iterator.Next()) }

func (i *discoveryIterator) advance(valid bool) bool {
	if !i.view.sharded {
		return valid
	}
	for valid {
		key := i.Key()
		if len(key) != utils.EventKeySize {
			return true // let the scanner charge its work budget
		}
		_, bucket, _, _ := utils.ParseEventKey(key)
		slot := utils.EventShardSlot(key)
		if slot == 0 && bucket <= i.last {
			return true // legacy lower bound is already enforced by the iterator
		}
		if !i.loadedOwnership {
			if i.shards != nil {
				i.owned = i.shards.slots
			} else {
				i.owned = i.view.ownedShardSlots(2*utils.MaxVirtualShards - 1)
			}
			i.loadedOwnership = true
		}
		if slot >= 2 && i.owned.next(slot) == slot {
			if bucket < i.earliest {
				valid = i.seek(slot)
				continue
			}
			if bucket <= i.last {
				return true
			}
		}
		next := i.owned.next(max(2, slot+1))
		if next < 0 {
			return false
		}
		valid = i.seek(next)
	}
	return false
}

func (i *discoveryIterator) seek(slot int) bool {
	// If the lower time bound cannot fit in the packed key, all virtual shards
	// are earlier than the requested time window.
	if i.earliest > utils.MaxVirtualTimeBucket {
		return false
	}
	key := utils.ShardStorageBucket(slot, i.earliest).Prefix(i.hash)
	if bytes.Compare(key, i.upper) >= 0 {
		return false
	}
	if i.seeks > 0 && !time.Now().Before(i.deadline) {
		i.resumeAt = key
		return false
	}
	i.seeks++
	return i.SeekGE(key)
}
