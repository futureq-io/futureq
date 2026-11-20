package dispatcher

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"go.uber.org/zap"
)

type shardScanRange struct {
	slots           shardSlotRanges
	includeLegacy   bool
	epoch           uint64
	assignmentEpoch uint64
}

type shardScanState struct {
	rangeKeys    shardScanRange
	cursor       scanCursor
	generation   uint64
	futureAfter  []byte
	futureScanAt time.Time
	futureIdle   time.Duration
}

// Split each layout independently so a mixed backlog cannot put all of the
// active layout's shards in one lane. Lane zero also covers unsharded keys.
func splitShardScans(view InterestView, workers int, layouts int) []shardScanState {
	// Each layout has at most this many slots to split into nonempty ranges.
	workers = min(workers, utils.MaxVirtualShards)
	lanes := make([]shardScanState, workers)
	lanes[0].rangeKeys.includeLegacy = true
	lanes[0].rangeKeys.epoch = view.Epoch
	lanes[0].rangeKeys.assignmentEpoch = view.assignmentEpoch
	used := 1
	for count := 2; count <= utils.MaxVirtualShards; count *= 2 {
		if layouts&count == 0 {
			continue
		}
		slots := view.ownedShardSlots(count)
		n := 0
		for _, r := range slots {
			n += r.end - r.first
		}
		parallel := min(n, workers)
		used = max(used, parallel)
		rangeIdx, assigned := 0, 0
		for lane := range parallel {
			r := &lanes[lane].rangeKeys
			r.epoch = view.Epoch
			r.assignmentEpoch = view.assignmentEpoch
			for end := int(int64(lane+1) * int64(n) / int64(parallel)); assigned < end; {
				first := slots[rangeIdx].first
				size := min(end-assigned, slots[rangeIdx].end-first)
				r.slots = append(r.slots, shardSlotRange{first, first + size})
				assigned += size
				slots[rangeIdx].first += size
				if slots[rangeIdx].first == slots[rangeIdx].end {
					rangeIdx++
				}
			}
		}
	}
	return lanes[:used]
}

func (r *shardScanRange) matches(view InterestView) bool {
	return r == nil || (r.epoch == view.Epoch && r.assignmentEpoch == view.assignmentEpoch)
}

// One positioned key establishes that a layout is present. Skip directly to
// the next layout boundary, so the probe costs no more seeks than layouts.
func discoverShardLayouts(source storage.IteratorSource, hash uint64) (int, error) {
	iter, err := source.NewIter(&storage.IterOptions{LowerBound: shardScanBound(hash, 2), UpperBound: topicScanEnd(hash)})
	if err != nil {
		return 0, err
	}
	defer iter.Close() //nolint:errcheck
	layouts := 0
	for valid := iter.First(); valid; {
		slot := utils.EventShardSlot(iter.Key())
		if slot < 2 {
			valid = iter.Next()
			continue
		}
		count := utils.SlotShardCount(slot)
		layouts |= count
		if count == utils.MaxVirtualShards {
			break
		}
		valid = iter.SeekGE(shardScanBound(hash, 2*count))
	}
	return layouts, iter.Error()
}

func (r *shardScanRange) bounds(hash uint64, lower, upper []byte) ([]byte, []byte) {
	if !r.includeLegacy {
		first := shardScanBound(hash, r.slots[0].first)
		if bytes.Compare(lower, first) < 0 {
			lower = first
		}
	}
	last := 1 // include the unsharded range even when no layouts are present
	if len(r.slots) != 0 {
		last = r.slots[len(r.slots)-1].end
	}
	if last < 2*utils.MaxVirtualShards {
		end := shardScanBound(hash, last)
		if bytes.Compare(end, upper) < 0 {
			upper = end
		}
	}
	return lower, upper
}

func shardScanBound(hash uint64, slot int) []byte {
	return utils.ShardStorageBucket(slot, 0).Prefix(hash)
}

func topicScanEnd(hash uint64) []byte {
	return append(utils.ShardStorageBucket(2*utils.MaxVirtualShards-1, utils.MaxVirtualTimeBucket).EventKey(hash, ^uint64(0)), 0)
}

func splitScanBudget(total, index, workers int) int {
	share := total / workers
	if index < total%workers {
		share++
	}
	return share
}

// Plan from the persisted layouts and add layouts observed in committed writes.
// Unchanged layouts keep their lane cursors and idle hints.
func (p *topicPreload) planShardScans(view InterestView, workers, layouts int) {
	if len(p.shardScans) == 0 || p.layouts != layouts {
		p.layouts = layouts
		p.shardScans = splitShardScans(view, workers, layouts)
		p.nextShardScan, p.nextDueShardScan = 0, 0
	}
}

func (job discoveryJob) laneIdle(lane *shardScanState, dueBucket uint64) bool {
	return job.tracksRevision && lane.generation == job.repairGeneration &&
		lane.cursor.shards.idle(dueBucket, job.revision, job.shardRevisions)
}

// Idle lanes have already had their due turn. A failed layout probe can
// retry even if those lanes have no reason to wake.
func (job discoveryJob) knownLanesIdle(dueBucket uint64) bool {
	for i := range job.owner.shardScans {
		if !job.laneIdle(&job.owner.shardScans[i], dueBucket) {
			return false
		}
	}
	return true
}

// Reserve a lane for due work first. Rotate that priority separately from
// the scan order used to fill the remaining lanes.
func (d *Dispatcher) shardDiscoveryJobs(base discoveryJob, now time.Time) []discoveryJob {
	p := base.owner
	lanes := p.shardScans
	parallel := min(len(lanes), max(base.dueSlots, base.futureSlots), d.Limits.Examined, d.Limits.Bytes)
	if parallel <= 0 {
		return nil
	}
	type selectedLane struct {
		index       int
		due, future bool
	}
	selected := make([]selectedLane, parallel)
	n, dueLanes, futureLanes := 0, 0, 0
	dueBucket := utils.CalculateBucket(now.UnixMilli(), d.TimeBucket)
	lead := -1
	if base.dueSlots > 0 {
		for offset := range lanes {
			index := (p.nextDueShardScan + offset) % len(lanes)
			lane := &lanes[index]
			if !base.laneIdle(lane, dueBucket) {
				lead = index
				future := base.futureSlots > 0 && !now.Before(lane.futureScanAt)
				selected[0] = selectedLane{index: index, due: true, future: future}
				n, dueLanes = 1, 1
				if future {
					futureLanes = 1
				}
				p.nextDueShardScan = (index + 1) % len(lanes)
				break
			}
		}
	}
	start := p.nextShardScan
	p.nextShardScan = (start + 1) % len(lanes)
	for attempts := 0; attempts < len(lanes) && n < parallel; attempts++ {
		index := (start + attempts) % len(lanes)
		if index == lead {
			continue
		}
		lane := &lanes[index]
		dueReady := dueLanes < base.dueSlots && !base.laneIdle(lane, dueBucket)
		futureReady := futureLanes < base.futureSlots && !now.Before(lane.futureScanAt)
		if !dueReady && !futureReady {
			continue
		}
		selected[n] = selectedLane{index: index, due: dueReady, future: futureReady}
		n++
		if dueReady {
			dueLanes++
		}
		if futureReady {
			futureLanes++
		}
	}
	jobs := make([]discoveryJob, 0, n)
	dueIndex, futureIndex := 0, 0
	for index, slot := range selected[:n] {
		lane := &lanes[slot.index]
		job := base
		job.shardLane, job.shards = slot.index, &lane.rangeKeys
		job.dueSlots, job.futureSlots = 0, 0
		if slot.due {
			job.dueSlots = splitScanBudget(base.dueSlots, dueIndex, dueLanes)
			dueIndex++
		}
		if slot.future {
			job.futureSlots = splitScanBudget(base.futureSlots, futureIndex, futureLanes)
			futureIndex++
		}
		job.examined = splitScanBudget(d.Limits.Examined, index, n)
		job.bytes = splitScanBudget(d.Limits.Bytes, index, n)
		if lane.generation == p.repairGeneration && lane.cursor.epoch == p.epoch {
			job.cursor = lane.cursor
		} else if prior := lane.cursor.shards; prior != nil {
			// Retry rewinds the shard positions without resetting which shard
			// gets the next turn.
			job.cursor.shards = &shardedDueScan{shards: prior.shards, next: prior.next, state: make(map[int]shardDueState)}
		}
		job.futureAfter = lane.futureAfter
		jobs = append(jobs, job)
	}
	return jobs
}

// One discovery worker owns the topic pass. Its shard scans report separately
// and close their iterators before the shared snapshot is released.
func (d *Dispatcher) runDiscoveryPass(ctx context.Context, jobs []discoveryJob, results chan<- discoveryResult) {
	var source storage.IteratorSource = d.db
	snapshots, canShareSnapshot := d.db.(storage.IteratorSnapshotter)
	sharedSnapshot := len(jobs) > 1 && canShareSnapshot
	if sharedSnapshot {
		snapshot := snapshots.NewIteratorSnapshot()
		defer func() {
			if err := snapshot.Close(); err != nil {
				d.logger.Warn("closing discovery snapshot failed", zap.Error(err))
			}
		}()
		source = snapshot
	}
	var now int64
	if len(jobs) > 1 {
		now = time.Now().UnixMilli()
	}
	budget := &scanPayloadBudget{limit: int64(d.Limits.Bytes)}
	budget.remaining.Store(budget.limit)
	lead := scanPayloadAdmission{budget: budget, ctx: ctx}
	follower := lead
	if len(jobs) > 1 {
		first := make(chan struct{})
		lead.release = sync.OnceFunc(func() { close(first) })
		follower.wait = first
	}
	run := func(job discoveryJob, admission scanPayloadAdmission) {
		if admission.release != nil {
			defer admission.release()
		}
		if ctx.Err() != nil {
			return
		}
		// A separate read transaction can block another lane from opening
		// its iterator when bbolt needs to remap. Wait before opening it.
		if !sharedSnapshot && !admission.awaitTurn() {
			return
		}
		job.source, job.now = source, now
		job.payloadAdmission = admission
		result := d.discover(job)
		if admission.release != nil {
			admission.release()
		}
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}
	var running sync.WaitGroup
	for _, job := range jobs[1:] {
		running.Go(func() { run(job, follower) })
	}
	run(jobs[0], lead)
	running.Wait()
}

func (d *Dispatcher) discoverySource() storage.IteratorSource {
	if d.scanSource != nil {
		return d.scanSource
	}
	return d.db
}

func (d *Dispatcher) recordFutureScan(p *topicPreload, result discoveryResult, backoff bool) {
	after, next, idle := &p.after, &p.futureScanAfter, &p.futureScanIdle
	if result.job.shards != nil {
		lane := &p.shardScans[result.job.shardLane]
		after, next, idle = &lane.futureAfter, &lane.futureScanAt, &lane.futureIdle
	}
	*after = result.after
	if !backoff {
		*next, *idle = time.Time{}, 0
		return
	}
	limit := max(d.interval, d.PreloadScanIdleMaxInterval)
	delay := min(limit, max(d.interval, *idle))
	*idle = delay + min(delay, limit-delay)
	*next = time.Now().Add(*idle)
}
