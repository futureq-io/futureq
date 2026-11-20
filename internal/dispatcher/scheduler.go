package dispatcher

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	"go.uber.org/zap"
)

// Discovery has its own slots: preparation/quorum latency never prevents a
// ready heap deadline or another topic's scan from being scheduled. Concurrent
// scans of a topic own disjoint shards and independent cursors.
type discoveryJob struct {
	revision              uint64
	repairGeneration      uint64
	assigned              map[[24]byte]bool // immutable, bounded delivery batch
	topic                 string
	owner                 *topicPreload
	cursor                scanCursor
	futureAfter           []byte
	futureSlots, dueSlots int
	shardLane             int
	shards                *shardScanRange
	examined, bytes       int
	source                storage.IteratorSource
	now                   int64
	probeLayouts          bool
	tracksRevision        bool
}
type discoveryResult struct {
	futureExhausted bool
	job             discoveryJob
	chunk           deliveryChunk
	future          candidateHeap
	cursor          scanCursor
	after           []byte
	layouts         int
	layoutsFound    bool
}
type discoveryPassState struct {
	remaining int
	admitted  bool
}
type deliveryJob struct {
	chunk  deliveryChunk
	owner  *topicPreload
	states map[string]*raft.DeliveryState // non-nil only when resuming prepared work
}
type deliveryResult struct {
	outcome string
	job     deliveryJob
	sent    int
	err     error
	pending bool
}

type permitRenewal struct {
	ok      bool
	elapsed time.Duration
}

func (d *Dispatcher) discover(job discoveryJob) discoveryResult {
	if job.probeLayouts {
		layouts, err := discoverShardLayouts(d.db, utils.TopicHash(job.topic))
		if err != nil {
			d.logger.Warn("delivery shard layout probe failed", zap.Error(err))
		}
		return discoveryResult{job: job, layouts: layouts, layoutsFound: err == nil}
	}
	// Construct a scanner adapter rather than copying a Dispatcher (sync.Map).
	scanner := &Dispatcher{scanOnly: true, skipKeys: func(key [24]byte) bool { return job.assigned[key] || job.owner.hasKey(key) }, db: d.db, hub: d.hub, deleter: d.deleter, logger: d.logger,
		TimeBucket: d.TimeBucket, Limits: d.Limits, PreloadLookahead: d.PreloadLookahead,
		PreloadMaxMessages: job.futureSlots, MaintenanceRecipients: d.MaintenanceRecipients,
		cursors: map[string]*scanCursor{job.topic: &job.cursor}}
	scanner.shardScan, scanner.scanSource = job.shards, job.source
	scanner.scanRevision, scanner.tracksScanRevision = job.revision, job.tracksRevision
	scanner.Limits.Candidates = job.dueSlots
	if job.shards != nil {
		scanner.Limits.Examined, scanner.Limits.Bytes = job.examined, job.bytes
	}
	now := job.now
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	var chunk deliveryChunk
	if job.topic == maintenanceTopic {
		chunk = scanner.collectMaintenance()
	} else if job.dueSlots > 0 {
		metrics.DeliveryRepairScansTotal.Inc()
		chunk = scanner.collectTopic(job.topic, now)
	}
	p := &topicPreload{epoch: job.owner.epoch, after: job.futureAfter, heapShift: job.owner.heapShift}
	// Future scans have independent work budgets and run even with productive
	// due traffic, provided the scheduler reserved future admission first.
	exhausted := false
	if job.topic != maintenanceTopic && job.futureSlots > 0 && d.PreloadLookahead > 0 {
		if job.now == 0 {
			now = time.Now().UnixMilli()
		}
		exhausted = scanner.preloadTopic(job.topic, p, d.hub.InterestView(job.topic), now)
	}
	job.source = nil
	return discoveryResult{futureExhausted: exhausted, job: job, chunk: chunk, future: p.items, cursor: *scanner.cursors[job.topic], after: p.after}
}

// admitCandidate is shared by synchronous adapters and asynchronous discovery.
// A popped candidate is fenced by the per-topic delivery job and Hub recipient
// reservations, not by an unbounded history of previously discovered keys.
func (d *Dispatcher) admitCandidate(p *topicPreload, c preloadCandidate) bool {
	if p.hasKey(c.key) {
		return false
	}
	p.addKey(c.key)
	p.items.pushCandidate(c, p.heapShift)
	if c.ready {
		p.readyCount++
		d.readyCount++
	} else {
		d.preloadCount++
	}
	return true
}

func (d *Dispatcher) admitChunk(p *topicPreload, chunk deliveryChunk, assigned map[[24]byte]bool) int {
	admitted := 0
	for _, item := range chunk.items {
		key := [24]byte(item.Key)
		if assigned[key] {
			continue
		}
		payload := chunk.payloads[string(item.Key)]
		due := chunk.oldestDueMs
		if payload.message != nil {
			due = payload.message.EnqueuedAtUnixMs + payload.message.DelayMs
		}
		if d.admitCandidate(p, preloadCandidate{key: key, due: due, payload: payload, ready: true}) {
			admitted++
		}
	}
	return admitted
}

// runPipeline exclusively owns heaps, deduplication, reservations, cursors,
// rotation and timers. Workers perform ALL storage/quorum work and return only
// bounded immutable batches/outcomes. Retry recovery is the cyclic due scan.
func (d *Dispatcher) runPipeline(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	discoveryWorkers := max(1, d.DiscoveryWorkers)
	shardWorkers := max(1, d.ShardWorkers)
	scanJobs := make(chan []discoveryJob, discoveryWorkers)
	scans := make(chan discoveryResult, discoveryWorkers)
	jobs := make(chan deliveryJob, d.Limits.Workers)
	results := make(chan deliveryResult, d.Limits.Workers)
	renew := make(chan struct{}, 1)
	renewed := make(chan permitRenewal, 1)
	var workers sync.WaitGroup
	for range discoveryWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case pass := <-scanJobs:
					if ctx.Err() != nil {
						return
					}
					d.runDiscoveryPass(ctx, pass, scans)
				}
			}
		}()
	}
	for range d.Limits.Workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-jobs:
					if ctx.Err() != nil {
						return
					}
					result := deliveryResult{job: job, outcome: "not-ready"}
					if job.owner.valid != nil && !job.owner.valid.Load() {
						result.outcome = "permission-lost"
					} else if !d.hub.DeliveryPermitted() {
						result.err = errors.New("permission expired before preparation")
						result.outcome = "permission-lost"
						result.pending = true
					} else {
						states := job.states
						var err error
						if states == nil {
							callCtx, stop := context.WithTimeout(ctx, d.Limits.ValidationTimeout)
							states, err = d.prepareChunk(callCtx, job.chunk)
							stop()
						}
						result.err = err
						if err != nil {
							result.outcome = "prepare-failed"
						}
						if err == nil {
							if d.authorizePrepared(ctx, states) {
								result.sent = d.deliverChunk(job.chunk, states)
								if result.sent > 0 {
									result.outcome = "queued"
								}
							} else {
								result.outcome = "permission-lost"
								if !needsPreparationBarrier(states) {
									result.pending = true
									result.job.states = states
								}
							}
						}
					}
					metrics.DeliveryWorkerOutcomesTotal.WithLabelValues(result.outcome).Inc()
					// Do not retain payloads in outcome channels after enqueue. Job identity
					// is sufficient to release reservations and reject stale completions.
					if !result.pending {
						result.job.chunk.payloads = nil
						result.job.chunk.items = nil
						result.job.states = nil
					}
					select {
					case results <- result:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-renew:
				started := time.Now()
				ok := d.refreshPermit(ctx)
				select {
				case renewed <- permitRenewal{ok: ok, elapsed: time.Since(started)}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		workers.Wait()
		d.preloads, d.preloadCount, d.readyCount = nil, 0, 0
		d.cursors = make(map[string]*scanCursor)
		d.observePreload()
		metrics.DeliveryPreloadCapacity.Set(0)
		for _, queue := range []string{"prepare", "discovery", "ready"} {
			metrics.DeliveryQueueDepth.WithLabelValues(queue).Set(0)
		}
		metrics.DeliveryOldestPreparationLatenessMs.Set(0)
	}()
	if d.preloads == nil {
		d.preloads = make(map[string]*topicPreload)
	}
	busy := make(map[string]*topicPreload)
	// Waiting jobs retain their bounded reservation and validated payloads.
	// They occupy no worker and are rechecked at enqueue and actual send.
	pending := make(map[string]deliveryJob)
	scanning := make(map[string]discoveryPassState)
	scanActive := 0
	assigned := make(map[string]map[[24]byte]bool)
	nextScan := make(map[string]time.Time)
	retryAfter := make(map[string]time.Time)
	busyDue := make(map[string]int64)
	var maintenance *deliveryChunk
	var maintenanceOwner *topicPreload
	reservedFuture, reservedDue := 0, 0
	// Each topic can retain or reserve at most one batch across all its shard
	// scans. Capacity for the delivery and discovery pools preserves admission
	// for another topic when a worker is idle.
	dueCapacity := max(discoveryWorkers, d.Limits.Workers) * d.Limits.Candidates
	rotation := 0
	permitReady, renewing := false, false
	permitDuration := d.hub.DeliveryReadPermit()
	renewBudget := permitDuration / 2
	var renewAfter time.Time
	var housekeepAfter time.Time
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	schedule := func() {
		now := time.Now()
		if !now.Before(housekeepAfter) {
			d.hub.ExpireInFlight(now)
			d.hub.ObserveQueues()
			housekeepAfter = now.Add(d.interval)
		}
		topics := d.hub.ActiveTopics()
		sort.Strings(topics)
		live := make(map[string]bool, len(topics))
		for _, topic := range topics {
			live[topic] = true
		}
		d.prunePreloads(live)
		for topic := range nextScan {
			if topic != maintenanceTopic && !live[topic] {
				delete(nextScan, topic)
				delete(retryAfter, topic)
				delete(d.cursors, topic)
			}
		}
		// Start renewal early enough to overlap observed quorum latency with
		// useful delivery. Its duration is always measured from read start.
		budget := max(renewBudget, min(d.Limits.ValidationTimeout+d.Limits.WorkTime, permitDuration/2))
		if d.hub.PermitNeedsRenewal(budget) {
			if !renewing && !now.Before(renewAfter) {
				renew <- struct{}{}
				renewing = true
				if !d.hub.DeliveryPermitted() {
					permitReady = false
				}
			}
		} else if !renewing {
			permitReady = true
		}
		if !permitReady || !d.hub.DeliveryPermitted() {
			return
		}
		for topic, job := range pending {
			jobs <- job
			delete(pending, topic)
		}
		if d.MaintenanceRecipients != nil {
			topics = append(topics, maintenanceTopic)
		}
		if len(topics) == 0 {
			return
		}
		start := rotation % len(topics)
		for offset := 0; offset < len(topics) && (len(busy) < d.Limits.Workers || len(scanning) < discoveryWorkers); offset++ {
			topic := topics[(start+offset)%len(topics)]
			var p *topicPreload
			if topic == maintenanceTopic {
				if maintenanceOwner == nil {
					maintenanceOwner = &topicPreload{}
				}
				p = maintenanceOwner
			} else {
				view := d.hub.InterestView(topic)
				if !view.Active {
					// The permit may expire after the scheduling check above.
					// Preserve validated work until renewal; prunePreloads fences
					// actual epoch/activation changes independently of the permit.
					continue
				}
				p = d.preloads[topic]
				if p == nil || p.epoch != view.Epoch || p.assignmentEpoch != view.assignmentEpoch {
					d.dropPreload(topic)
					p = &topicPreload{epoch: view.Epoch, assignmentEpoch: view.assignmentEpoch, valid: &atomic.Bool{}, heapShift: candidateHeapShift(d.HeapType), sharded: view.sharded}
					p.valid.Store(true)
					d.preloads[topic] = p
				}
			}
			if busy[topic] == nil && len(busy) < d.Limits.Workers && !now.Before(retryAfter[topic]) {
				var chunk deliveryChunk
				if topic == maintenanceTopic {
					if maintenance != nil {
						chunk = *maintenance
						maintenance = nil
					}
				} else {
					chunk = d.collectPreloaded(topic, p, d.hub.InterestView(topic), now.UnixMilli())
				}
				if chunk.unfit {
					retryAfter[topic] = time.Now().Add(d.interval)
					d.releaseReady(topic, p)
				}
				if len(chunk.items) > 0 {
					busy[topic] = p
					busyDue[topic] = chunk.oldestDueMs
					keys := make(map[[24]byte]bool, len(chunk.items))
					for _, item := range chunk.items {
						keys[[24]byte(item.Key)] = true
					}
					assigned[topic] = keys
					jobs <- deliveryJob{chunk: chunk, owner: p}
					rotation = (start + offset + 1) % len(topics)
				}
			}
			// Do not repeatedly discover a full queue's rejected batch on each result.
			// Its bounded retry deadline and independent cyclic cursor guarantee repair.
			if scanning[topic].remaining != 0 || len(scanning) >= discoveryWorkers || now.Before(nextScan[topic]) || now.Before(retryAfter[topic]) {
				continue
			}
			if topic == maintenanceTopic && (maintenance != nil || busy[topic] != nil) {
				continue
			}
			// A busy topic may prefetch one batch, never the entire shared
			// allowance. Already retained entries consume its topic credit.
			var revision uint64
			var tracksRevisions bool
			if topic != maintenanceTopic {
				revision, tracksRevisions = d.cacheRevision(utils.TopicHash(topic))
				if p.revision != revision {
					p.revision = revision
					p.futureScanAfter, p.futureScanIdle = time.Time{}, 0
					for i := range p.shardScans {
						p.shardScans[i].futureScanAt, p.shardScans[i].futureIdle = time.Time{}, 0
					}
				}
			}
			dueSlots := min(max(0, d.Limits.Candidates-p.readyCount), dueCapacity-d.readyCount-reservedDue)
			futureSlots := 0
			if topic != maintenanceTopic && d.PreloadLookahead > 0 && !now.Before(p.futureScanAfter) {
				futureSlots = min(d.Limits.Candidates, max(0, d.PreloadMaxMessages-d.preloadCount-reservedFuture))
			}
			if dueSlots <= 0 && futureSlots <= 0 {
				continue
			}

			job := discoveryJob{revision: revision, repairGeneration: p.repairGeneration, assigned: assigned[topic], topic: topic, owner: p,
				cursor: scanCursor{epoch: p.epoch, assignmentEpoch: p.assignmentEpoch}, futureSlots: futureSlots, dueSlots: dueSlots, tracksRevision: tracksRevisions}
			var pass []discoveryJob
			if p.sharded && !p.dueBeforeRefresh && !now.Before(p.layoutProbeAt) && (len(p.shardScans) == 0 || !tracksRevisions || p.layoutRevision != revision) {
				job.probeLayouts = true
				job.dueSlots, job.futureSlots = 0, 0
				pass = []discoveryJob{job}
			} else if len(p.shardScans) > 0 {
				pass = d.shardDiscoveryJobs(job, now)
			} else if p.sharded {
				continue // retry the unsuccessful layout probe on a later pass
			} else {
				if current := d.cursors[topic]; current != nil {
					job.cursor = *current
				}
				job.futureAfter = p.after
				pass = []discoveryJob{job}
			}
			if len(pass) == 0 {
				continue
			}
			scanning[topic] = discoveryPassState{remaining: len(pass)}
			scanActive += len(pass)
			for _, task := range pass {
				reservedDue += task.dueSlots
				reservedFuture += task.futureSlots
				if task.dueSlots > 0 {
					// Scanning admission prevents another refresh from
					// overtaking the due work queued here.
					p.dueBeforeRefresh = false
				}
			}
			scanJobs <- pass
			rotation = (start + offset + 1) % len(topics)
		}
		d.observePreload()
		metrics.DeliveryQueueDepth.WithLabelValues("prepare").Set(float64(len(busy)))
		metrics.DeliveryQueueDepth.WithLabelValues("discovery").Set(float64(scanActive))
		metrics.DeliveryQueueDepth.WithLabelValues("ready").Set(float64(d.readyCount))
		oldest := int64(0)
		for _, due := range busyDue {
			if due > 0 {
				oldest = max(oldest, now.UnixMilli()-due)
			}
		}
		metrics.DeliveryOldestPreparationLatenessMs.Set(float64(oldest))
	}
	schedule()
	for {
		var deadline <-chan time.Time
		if permitReady && len(busy) < d.Limits.Workers && d.hub.DeliveryPermitted() {
			var next time.Time
			for topic, p := range d.preloads {
				if busy[topic] != nil || len(p.items) == 0 {
					continue
				}
				due := time.UnixMilli(p.items[0].due)
				if due.Before(retryAfter[topic]) {
					due = retryAfter[topic]
				}
				if next.IsZero() || due.Before(next) {
					next = due
				}
			}
			if !next.IsZero() {
				timer.Reset(time.Until(next))
				deadline = timer.C
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.wakeCh:
		case <-deadline:
		case renewal := <-renewed:
			renewing = false
			permitReady = renewal.ok
			renewBudget = min(permitDuration-min(d.interval, permitDuration/10), max(permitDuration/2, 2*renewal.elapsed+d.interval))
			if !renewal.ok {
				renewAfter = time.Now().Add(d.interval)
			}
		case result := <-results:
			topic := result.job.chunk.topic
			if result.pending && busy[topic] == result.job.owner {
				pending[topic] = result.job
				break
			}
			if busy[topic] == result.job.owner {
				delete(busy, topic)
				delete(busyDue, topic)
				delete(assigned, topic)
				if result.err != nil || result.sent == 0 {
					retryAfter[topic] = time.Now().Add(d.interval)
					// Backing-off topics cannot strand ready credits while workers are idle.
					d.releaseReady(topic, result.job.owner)
				}
			}
			if result.err != nil {
				d.logger.Warn("delivery preparation failed; cyclic scan will retry", zap.Error(result.err))
			}
		case result := <-scans:
			job := result.job
			reservedDue -= job.dueSlots
			reservedFuture -= job.futureSlots
			scanActive--
			pass := scanning[job.topic]
			pass.remaining--
			current := d.preloads[job.topic]
			if job.topic == maintenanceTopic {
				current = maintenanceOwner
			}
			admitted := 0
			if current == job.owner {
				if job.probeLayouts {
					current.layoutProbeAt = time.Now().Add(d.interval)
					if result.layoutsFound {
						view := d.hub.InterestView(job.topic)
						if view.Active && view.Epoch == current.epoch && view.assignmentEpoch == current.assignmentEpoch {
							current.layoutRevision = job.revision
							if len(current.shardScans) == 0 || current.layouts != result.layouts {
								current.layouts = result.layouts
								current.shardScans = splitShardScans(view, shardWorkers, result.layouts)
								current.nextShardScan = 0
							}
							pass.admitted = true // schedule the planned scans immediately
						}
					}
					current.dueBeforeRefresh = len(current.shardScans) > 0
				}
				if job.dueSlots > 0 && current.repairGeneration == job.repairGeneration {
					if job.shards == nil {
						d.cursors[job.topic] = &result.cursor
					} else {
						lane := &current.shardScans[job.shardLane]
						lane.cursor, lane.generation = result.cursor, job.repairGeneration
					}
				}
				if job.topic == maintenanceTopic {
					if len(result.chunk.items) > 0 {
						maintenance = &result.chunk
						admitted = len(result.chunk.items)
					}
				} else {
					if job.futureSlots > 0 {
						revision, _ := d.cacheRevision(utils.TopicHash(job.topic))
						backoff := job.tracksRevision && d.PreloadScanIdleMaxInterval > 0 && job.revision == revision && result.futureExhausted && len(result.future) == 0
						d.recordFutureScan(current, result, backoff)
					}
					if !time.Now().Before(retryAfter[job.topic]) {
						admitted += d.admitChunk(current, result.chunk, assigned[job.topic])
					} else if len(result.chunk.items) > 0 {
						// A scan dispatched before failure can finish while the topic is
						// backing off. Release its reservation and preserve repair.
						current.repairGeneration++
						delete(d.cursors, job.topic)
					}
					for _, candidate := range result.future {
						if !assigned[job.topic][candidate.key] {
							if d.admitCandidate(current, candidate) {
								admitted++
							}
						}
					}
				}
			}
			// Every range wraps; rejected/duplicate results never certify completion.
			pass.admitted = pass.admitted || admitted > 0
			if pass.remaining == 0 {
				delete(scanning, job.topic)
				nextScan[job.topic] = time.Time{}
				if !pass.admitted {
					nextScan[job.topic] = time.Now().Add(d.interval)
				}
			} else {
				scanning[job.topic] = pass
			}
		}
		schedule()
	}
}
