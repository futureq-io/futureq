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
	slots           [2 * utils.MaxVirtualShards]bool
	first, last     int
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
	owned := view.ownedShardSlots()
	lanes := make([]shardScanState, workers)
	lanes[0].rangeKeys.includeLegacy = true
	lanes[0].rangeKeys.epoch = view.Epoch
	lanes[0].rangeKeys.assignmentEpoch = view.assignmentEpoch
	used := 1
	for count := 2; count <= utils.MaxVirtualShards; count *= 2 {
		if layouts&count == 0 {
			continue
		}
		var slots [utils.MaxVirtualShards]int
		n := 0
		for slot := count; slot < 2*count; slot++ {
			if owned[slot] {
				slots[n] = slot
				n++
			}
		}
		parallel := min(n, workers)
		used = max(used, parallel)
		for lane := range parallel {
			r := &lanes[lane].rangeKeys
			r.epoch = view.Epoch
			r.assignmentEpoch = view.assignmentEpoch
			start := lane * n / parallel
			end := (lane + 1) * n / parallel
			if r.first == 0 {
				r.first = slots[start]
			}
			r.last = slots[end-1]
			for _, slot := range slots[start:end] {
				r.slots[slot] = true
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
		first := shardScanBound(hash, r.first)
		if bytes.Compare(lower, first) < 0 {
			lower = first
		}
	}
	if r.last+1 < len(r.slots) {
		end := shardScanBound(hash, r.last+1)
		if bytes.Compare(end, upper) < 0 {
			upper = end
		}
	}
	return lower, upper
}

func shardScanBound(hash uint64, slot int) []byte {
	return utils.EventKey(utils.ShardStorageBucket(slot, 0), hash, 0)[:16]
}

func topicScanEnd(hash uint64) []byte {
	return append(utils.EventKey(^uint64(0), hash, ^uint64(0)), 0)
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
		p.nextShardScan = 0
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

// Reserve a topic's original quota across tasks. Rotate the first lane so
// even when one budget is scarce, each lane eventually receives a share.
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
	var selected [utils.MaxVirtualShards]selectedLane
	n, dueLanes, futureLanes := 0, 0, 0
	dueBucket := utils.CalculateBucket(now.UnixMilli(), d.TimeBucket)
	start := p.nextShardScan
	p.nextShardScan = (start + 1) % len(lanes)
	for attempts := 0; attempts < len(lanes) && n < parallel; attempts++ {
		index := (start + attempts) % len(lanes)
		lane := &lanes[index]
		dueReady := base.dueSlots > 0 && !base.laneIdle(lane, dueBucket)
		futureReady := base.futureSlots > 0 && !now.Before(lane.futureScanAt)
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
		if job.dueSlots == 0 && job.futureSlots == 0 {
			continue
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
	if snapshots, ok := d.db.(storage.IteratorSnapshotter); len(jobs) > 1 && ok {
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
	run := func(job discoveryJob) {
		if ctx.Err() != nil {
			return
		}
		job.source, job.now = source, now
		result := d.discover(job)
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}
	var running sync.WaitGroup
	for _, job := range jobs[1:] {
		running.Go(func() { run(job) })
	}
	run(jobs[0])
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
