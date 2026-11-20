package dispatcher

import (
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"go.uber.org/zap"
)

const noDueBucket = ^uint64(0)

type shardDueState struct {
	cursor    scanCursor
	revision  uint64
	notBefore uint64
	peek      bool // the bounded due range was empty; check its next live bucket
}

// A discovery lane owns this state until its result is returned to the
// scheduler. Each shard has a frozen endpoint and its own cyclic cursor.
type shardedDueScan struct {
	shards    []int // zero denotes the unsharded time-ordered range
	state     map[int]shardDueState
	next      int
	layouts   int
	layoutRev uint64
	idleUntil uint64
	idleRev   uint64
	watch     *storage.TopicRevision         // synchronous scan only
	revisions *storage.TopicRevisionSnapshot // synchronous scan only
}

func (d *Dispatcher) collectShardedDue(topic string, hash, last uint64, view InterestView, cursor *scanCursor, chunk *deliveryChunk, work *dueScanWork) {
	if cursor.shards == nil {
		cursor.shards = &shardedDueScan{state: make(map[int]shardDueState)}
		if d.scanSource == nil {
			cursor.shards.watch = d.watchTopic(hash)
			if cursor.shards.watch != nil {
				cursor.shards.revisions = new(storage.TopicRevisionSnapshot)
			}
		}
	}
	scan := cursor.shards
	revision, tracked, shards := d.scanRevision, d.tracksScanRevision, d.scanShardRevisions
	if d.scanSource == nil {
		revision, tracked = d.cacheRevision(scan.watch, scan.revisions)
		shards = scan.revisions
	}
	if d.shardScan == nil {
		layouts := scan.layouts
		if scan.shards == nil || (shards == nil && (!tracked || scan.layoutRev != revision)) {
			var err error
			layouts, err = discoverShardLayouts(d.discoverySource(), hash)
			if err != nil {
				d.logger.Warn("delivery shard layout probe failed", zap.Error(err))
				return
			}
		}
		if shards != nil {
			layouts |= shards.WrittenLayouts
		}
		if scan.shards == nil || layouts != scan.layouts {
			scan.shards = dueShardOrder(&splitShardScans(view, 1, layouts)[0].rangeKeys)
			scan.next, scan.idleUntil = 0, 0
		}
		scan.layouts, scan.layoutRev = layouts, revision
	} else if scan.shards == nil {
		scan.shards = dueShardOrder(d.shardScan)
	}
	if tracked && scan.idle(last, revision, shards) {
		return
	}

	start := scan.next
	idleUntil := noDueBucket
	for offset := range len(scan.shards) {
		index := (start + offset) % len(scan.shards)
		slot := scan.shards[index]
		state := scan.state[slot]
		slotRevision := scanSlotRevision(revision, shards, slot)
		if tracked && state.revision == slotRevision && state.notBefore > last {
			idleUntil = min(idleUntil, state.notBefore)
			continue
		}
		if d.dueScanFull(chunk, work) {
			idleUntil = 0
			break
		}
		scan.next = (index + 1) % len(scan.shards)
		if state.revision != slotRevision {
			state.notBefore, state.peek = 0, false
			state.revision = slotRevision
		}
		lower, upper, end := dueShardBounds(hash, last, slot)
		if !state.peek {
			examined := work.examined
			empty := d.collectDueRange(topic, view, &state.cursor, lower, upper, chunk, work)
			if examined == work.examined {
				work.examined++ // even a range without keys consumes a work unit
			}
			state.peek = tracked && empty
		}
		if state.peek && !d.dueScanFull(chunk, work) {
			work.examined++
			state.notBefore, state.peek = d.nextShardBucket(lower, end, last), false
		}
		scan.state[slot] = state
		idleUntil = min(idleUntil, state.notBefore)
	}
	if tracked {
		scan.idleRev, scan.idleUntil = revision, idleUntil
	}
}

// A lane whose earliest hint is still in the future can stay idle even when
// writes to other lanes have advanced the topic revision.
func (scan *shardedDueScan) idle(last, revision uint64, shards *storage.TopicRevisionSnapshot) bool {
	if scan == nil || scan.idleUntil <= last {
		return false
	}
	if scan.idleRev == revision {
		return true
	}
	if shards == nil {
		return false
	}
	for _, slot := range scan.shards {
		if scan.state[slot].revision != shards.Shards[slot] {
			return false
		}
	}
	return true
}

func scanSlotRevision(revision uint64, shards *storage.TopicRevisionSnapshot, slot int) uint64 {
	if shards != nil {
		return shards.Shards[slot]
	}
	return revision
}

func (d *Dispatcher) watchTopic(hash uint64) *storage.TopicRevision {
	if db, ok := d.db.(storage.TopicRevisioner); ok {
		return db.WatchTopic(hash)
	}
	return nil
}

func (d *Dispatcher) cacheRevision(watch *storage.TopicRevision, shards *storage.TopicRevisionSnapshot) (uint64, bool) {
	if watch != nil {
		return watch.Read(shards), true
	}
	if db, ok := d.db.(storage.CacheRevisioner); ok {
		return db.CacheRevision(), true
	}
	return 0, false
}

func dueShardOrder(shards *shardScanRange) []int {
	var order []int
	if shards.includeLegacy {
		order = append(order, 0)
	}
	for slot, owned := range shards.slots {
		if owned {
			order = append(order, slot)
		}
	}
	return order
}

func dueShardBounds(hash, last uint64, slot int) (lower, dueEnd, end []byte) {
	if slot == 0 {
		return utils.TopicLowerBound(hash), utils.DueUpperBound(hash, last), shardScanBound(hash, 2)
	}
	lower = shardScanBound(hash, slot)
	if slot < 2*utils.MaxVirtualShards-1 {
		end = shardScanBound(hash, slot+1)
	} else {
		end = topicScanEnd(hash)
	}
	dueEnd = end
	if last < utils.MaxVirtualTimeBucket {
		dueEnd = utils.EventKey(utils.ShardStorageBucket(slot, last+1), hash, 0)[:16]
	}
	return lower, dueEnd, end
}

// Empty-shard hints are scoped to the topic revision captured before the
// scan's snapshot. A successful insert invalidates a hint even if the insert
// is behind the cursor or earlier than the shard's old first key.
func (d *Dispatcher) nextShardBucket(lower, upper []byte, last uint64) uint64 {
	iter, err := d.discoverySource().NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return 0
	}
	defer iter.Close() //nolint:errcheck
	if iter.First() {
		_, bucket, _, err := utils.ParseEventKey(iter.Key())
		if err != nil {
			// Without a valid first event we have no useful idle bound.
			return 0
		}
		if bucket <= last {
			return 0
		}
		return bucket
	}
	if iter.Error() == nil {
		return noDueBucket
	}
	return 0
}
