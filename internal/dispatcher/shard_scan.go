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
	slots         [2 * utils.MaxVirtualShards]bool
	first, last   int
	includeLegacy bool
	epoch         uint64
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
func splitShardScans(view InterestView, workers int) []shardScanState {
	// Each layout has at most this many slots to split into nonempty ranges.
	workers = min(workers, utils.MaxVirtualShards)
	owned := view.ownedShardSlots()
	lanes := make([]shardScanState, workers)
	lanes[0].rangeKeys.includeLegacy = true
	used := 1
	for count := 2; count <= utils.MaxVirtualShards; count *= 2 {
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
	if used == 1 {
		return nil
	}
	return lanes[:used]
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

func splitScanBudget(total, index, workers int) int {
	share := total / workers
	if index < total%workers {
		share++
	}
	return share
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
	var selected [utils.MaxVirtualShards]int
	n, futureLanes := 0, 0
	start := p.nextShardScan
	p.nextShardScan = (start + 1) % len(lanes)
	for attempts := 0; attempts < len(lanes) && n < parallel; attempts++ {
		index := (start + attempts) % len(lanes)
		futureReady := base.futureSlots > 0 && !now.Before(lanes[index].futureScanAt)
		if base.dueSlots == 0 && !futureReady {
			continue
		}
		selected[n] = index
		n++
		if futureReady {
			futureLanes++
		}
	}
	jobs := make([]discoveryJob, 0, n)
	futureIndex := 0
	for index, laneIndex := range selected[:n] {
		lane := &lanes[laneIndex]
		job := base
		job.shardLane, job.shards = laneIndex, &lane.rangeKeys
		job.dueSlots = splitScanBudget(base.dueSlots, index, n)
		job.futureSlots = 0
		if base.futureSlots > 0 && !now.Before(lane.futureScanAt) {
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
