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
	peek      bool // the bounded due range was empty; check for live keys
}

// A discovery lane owns this state until its result is returned to the
// scheduler. Each shard has a frozen endpoint and its own cyclic cursor.
type shardedDueScan struct {
	shards    shardSlotRanges // zero denotes the unsharded time-ordered range
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

	first := scan.shards.next(0)
	start := scan.shards.next(scan.next)
	if start < 0 {
		start = first
	}
	idleUntil := noDueBucket
	wrapped := false
	for slot := start; slot >= 0; slot = scan.next {
		state := scan.state[slot]
		slotRevision := scanSlotRevision(revision, shards, slot)
		next := slot + 1
		if !tracked || state.revision != slotRevision || state.notBefore <= last {
			if d.dueScanFull(chunk, work) {
				idleUntil = 0
				break
			}
			skipEmpty := slot >= 256
			if state.revision != slotRevision {
				state.notBefore = 0
				// The wide seek starts at this shard in a fresh view, so it
				// can run even when writes invalidate the previous due check.
				state.peek = state.peek && skipEmpty
				state.revision = slotRevision
			}
			lower, upper, end := dueShardBounds(hash, last, slot)
			if !state.peek {
				examined := work.examined
				empty := d.collectDueRange(topic, view, &state.cursor, lower, upper, chunk, work)
				if examined == work.examined {
					work.examined++ // even a range without keys consumes a work unit
				}
				state.peek = empty && (tracked || skipEmpty)
			}
			if state.peek && !d.dueScanFull(chunk, work) {
				work.examined++
				if skipEmpty {
					state.notBefore, next = d.nextLiveShard(hash, slot, last)
				} else {
					state.notBefore = d.nextShardBucket(lower, end, last)
				}
				state.peek = false
			}
			scan.state[slot] = state
			if skipEmpty && state.peek {
				// Resume the seek here if the due check used the last work unit.
				scan.next, idleUntil = slot, 0
				break
			}
		}
		idleUntil = min(idleUntil, state.notBefore)
		scan.next = scan.shards.next(next)
		if scan.next < 0 {
			scan.next, wrapped = first, true
		}
		if wrapped && scan.next >= start {
			break
		}
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
	for slot, version := range shards.Shards {
		if version > scan.idleRev && scan.shards.next(slot) == slot {
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

func dueShardOrder(shards *shardScanRange) shardSlotRanges {
	if !shards.includeLegacy {
		return shards.slots
	}
	return append(shardSlotRanges{{0, 1}}, shards.slots...)
}

func dueShardBounds(hash, last uint64, slot int) (lower, dueEnd, end []byte) {
	if slot == 0 {
		return utils.TopicLowerBound(hash), utils.DueUpperBound(hash, last), shardScanBound(hash, 1)
	}
	lower = shardScanBound(hash, slot)
	if slot < 2*utils.MaxVirtualShards-1 {
		end = shardScanBound(hash, slot+1)
	} else {
		end = topicScanEnd(hash)
	}
	dueEnd = end
	if last < utils.MaxVirtualTimeBucket {
		dueEnd = utils.ShardStorageBucket(slot, last+1).Prefix(hash)
	}
	return lower, dueEnd, end
}

// nextLiveShard finds the current slot's earliest future bucket or skips to
// the next slot with keys. New writes invalidate hints through their revision.
func (d *Dispatcher) nextLiveShard(hash uint64, slot int, last uint64) (notBefore uint64, next int) {
	iter, err := d.discoverySource().NewIter(&storage.IterOptions{LowerBound: shardScanBound(hash, slot), UpperBound: topicScanEnd(hash)})
	if err != nil {
		return 0, slot + 1
	}
	defer iter.Close() //nolint:errcheck
	if iter.First() {
		key := iter.Key()
		if liveSlot := utils.EventShardSlot(key); liveSlot > slot {
			return noDueBucket, liveSlot
		}
		if _, bucket, _, err := utils.ParseEventKey(key); err == nil && bucket > last {
			return bucket, slot + 1
		}
		return 0, slot + 1
	}
	if iter.Error() != nil {
		return 0, slot + 1
	}
	return noDueBucket, 2 * utils.MaxVirtualShards
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
