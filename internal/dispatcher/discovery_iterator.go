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
	owned                [2 * utils.MaxVirtualShards]bool
	loadedOwnership      bool
	seeks                int
	resumeAt             []byte
}

func openDiscoveryIterator(db storage.DB, view InterestView, hash uint64, lower, upper []byte, earliest, last uint64, deadline time.Time) (*discoveryIterator, error) {
	if view.sharded && len(upper) == 0 {
		// Include every possible layout, including when the topic hash is MaxUint64.
		upper = append(utils.EventKey(^uint64(0), hash, ^uint64(0)), 0)
	}
	iter, err := db.NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	return &discoveryIterator{Iterator: iter, view: view, hash: hash, earliest: earliest, last: last, deadline: deadline, upper: upper}, nil
}

func (i *discoveryIterator) First() bool { return i.advance(i.Iterator.First()) }
func (i *discoveryIterator) Next() bool  { return i.advance(i.Iterator.Next()) }

func (i *discoveryIterator) advance(valid bool) bool {
	if !i.view.sharded {
		return valid
	}
	for valid {
		key := i.Key()
		if len(key) != 24 {
			return true // let the scanner charge its work budget
		}
		_, bucket, _, _ := utils.ParseEventKey(key)
		slot := utils.EventShardSlot(key)
		if slot == 0 && bucket <= i.last {
			return true // legacy lower bound is already enforced by the iterator
		}
		if !i.loadedOwnership {
			i.owned = i.view.ownedShardSlots()
			i.loadedOwnership = true
		}
		if slot >= 2 && i.owned[slot] {
			if bucket < i.earliest {
				valid = i.seek(slot)
				continue
			}
			if bucket <= i.last {
				return true
			}
		}
		next := max(2, slot+1)
		for next < len(i.owned) && !i.owned[next] {
			next++
		}
		if next == len(i.owned) {
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
	key := utils.EventKey(utils.ShardStorageBucket(slot, i.earliest), i.hash, 0)[:16]
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
