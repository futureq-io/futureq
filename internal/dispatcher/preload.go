package dispatcher

import (
	"bytes"
	"container/heap"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// Entries own decoded protobuf data. Default proto.Unmarshal copies byte fields;
// no iterator-owned memory or recipient decisions survive discovery.
type preloadCandidate struct {
	key     [24]byte
	due     int64
	payload cachedPayload
	ready   bool // charged to the independent overdue admission allowance
}

type cachedPayload struct {
	message  *storagepb.StoredMessage
	validity storage.CacheToken
}

func cachePayload(iter storage.Iterator, key, value []byte) (cachedPayload, error) {
	var msg storagepb.StoredMessage
	metrics.DeliveryPayloadDecodesTotal.WithLabelValues("discovery").Inc()
	if err := proto.Unmarshal(value, &msg); err != nil {
		return cachedPayload{}, err
	}
	payload := cachedPayload{message: &msg}
	if cached, ok := iter.(storage.CachedIterator); ok {
		payload.validity = cached.CacheToken(key)
	}
	return payload, nil
}

type candidateHeap []preloadCandidate

func (h candidateHeap) Len() int { return len(h) }
func (h candidateHeap) Less(i, j int) bool {
	if h[i].due == h[j].due {
		return bytes.Compare(h[i].key[:], h[j].key[:]) < 0
	}
	return h[i].due < h[j].due
}
func (h candidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(v any)   { *h = append(*h, v.(preloadCandidate)) }
func (h *candidateHeap) Pop() any {
	last := len(*h) - 1
	v := (*h)[last]
	(*h)[last] = preloadCandidate{} // do not retain popped payloads in the backing array
	*h = (*h)[:last]
	return v
}

type topicPreload struct {
	valid    *atomic.Bool // optional generation fence, set by the production owner
	epoch    uint64
	after    []byte // independent of the live due cursor; never a watermark
	items    candidateHeap
	keys     map[[24]byte]bool
	scanNext bool
}

// collectScheduledTopic alternates cached chunks with live due scans. Even a
// continuously full cache cannot hide a newly inserted overdue message, a NACK,
// or a failed preparation behind a future scan position. The ordinary cyclic
// scan remains the recovery path for every discarded or failed candidate.
func (d *Dispatcher) collectScheduledTopic(topic string, nowMs int64) deliveryChunk {
	view := d.hub.InterestView(topic)
	if !view.Active {
		d.dropPreload(topic)
		return deliveryChunk{topic: topic}
	}
	if d.preloads == nil {
		d.preloads = make(map[string]*topicPreload)
	}
	p := d.preloads[topic]
	if p == nil || p.epoch != view.Epoch {
		d.dropPreload(topic)
		p = &topicPreload{epoch: view.Epoch}
		d.preloads[topic] = p
	}
	if !p.scanNext {
		chunk := d.collectPreloaded(topic, p, view, nowMs)
		if len(chunk.items) > 0 {
			p.scanNext = true
			return chunk
		}
	}
	p.scanNext = false
	scanned := d.collectTopic(topic, nowMs)
	d.admitChunk(p, scanned, nil)
	if len(scanned.items) == 0 && d.PreloadLookahead > 0 && d.PreloadMaxMessages > 0 {
		d.preloadTopic(topic, p, view, nowMs)
	}
	return d.collectPreloaded(topic, p, view, nowMs)
}

func (d *Dispatcher) collectPreloaded(topic string, p *topicPreload, view InterestView, nowMs int64) deliveryChunk {
	started := time.Now()
	chunk := deliveryChunk{topic: topic, scanStarted: nowMs, started: started, payloads: make(map[string]cachedPayload), generation: p.valid}
	defer func() {
		observeCollection("preload_ready", started)
		d.observePreload()
	}()
	commandBytes, examined := 3, 0
	for len(p.items) > 0 && p.items[0].due <= nowMs {
		if examined >= d.Limits.Examined || len(chunk.items) >= d.Limits.Candidates || (examined > 0 && time.Since(started) >= d.Limits.WorkTime) || !d.hub.DeliveryPermitted() {
			break
		}
		candidate := p.items[0]
		item := raft.DeliveryPrepare{Key: append([]byte(nil), candidate.key[:]...), Epoch: view.Epoch, Recipients: view.Recipients}
		encoded, err := json.Marshal(item)
		if err == nil && commandBytes+len(encoded)+1 > d.Limits.Bytes && len(chunk.items) > 0 {
			break // retain this candidate for a subsequent bounded chunk
		}
		heap.Pop(&p.items)
		delete(p.keys, candidate.key)
		if candidate.ready {
			d.readyCount--
		} else {
			d.preloadCount--
		}
		examined++
		if err != nil || commandBytes+len(encoded)+1 > d.Limits.Bytes {
			continue // ordinary scanning reports an oversized recipient set
		}
		owners := view.localOwners(item.Key)
		if len(owners) == 0 || !d.hub.hasReadyOwner(owners, item.Key) {
			continue
		}
		commandBytes += len(encoded) + 1
		source := "preload"
		if candidate.ready {
			source = "scan"
		}
		metrics.DeliveryCandidatesTotal.WithLabelValues(source).Inc()
		chunk.items = append(chunk.items, item)
		chunk.payloads[string(item.Key)] = candidate.payload
		if chunk.oldestDueMs == 0 || candidate.due < chunk.oldestDueMs {
			chunk.oldestDueMs = candidate.due
		}
	}
	if len(p.items) == 0 {
		p.items, p.keys = nil, nil // release capacity when a topic drains
	} else if len(p.items) <= cap(p.items)/4 {
		items := make(candidateHeap, len(p.items))
		copy(items, p.items)
		p.items = items
		p.keys = make(map[[24]byte]bool, len(items))
		for _, candidate := range items {
			p.keys[candidate.key] = true
		}
	}
	return chunk
}

// preloadTopic reads only the moving lookahead range, with its own cyclic
// cursor and the existing key/byte/time budgets. Full buffers stop speculative
// reads, while live due scans continue independently. Coarse buckets require
// checking the exact message due time before retaining a candidate.
func (d *Dispatcher) preloadTopic(topic string, p *topicPreload, view InterestView, nowMs int64) {
	defer d.observePreload()
	if d.preloadCount >= d.PreloadMaxMessages {
		return
	}
	started := time.Now()
	loaded, scanned, filtered := 0, 0, 0
	defer func() {
		observeCollection("preload_scan", started)
		metrics.DeliveryPreloadLoadedTotal.Add(float64(loaded))
		metrics.DeliveryPreloadScannedKeysTotal.Add(float64(scanned))
		metrics.DeliveryScannedKeysTotal.Add(float64(scanned))
		metrics.DeliveryOwnershipFilteredTotal.Add(float64(filtered))
	}()
	horizon := nowMs + d.PreloadLookahead.Milliseconds()
	if horizon < nowMs { // saturate rather than wrap a very large configured duration
		horizon = int64(^uint64(0) >> 1)
	}
	hash := utils.TopicHash(topic)
	lower := utils.EventKey(utils.CalculateBucket(nowMs, d.TimeBucket), hash, 0)
	if bytes.Compare(p.after, lower) > 0 {
		lower = p.after
	}
	iter, err := d.db.NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: utils.DueUpperBound(hash, utils.CalculateBucket(horizon, d.TimeBucket))})
	if err != nil {
		d.logger.Warn("preload iterator failed", zap.Error(err))
		return
	}
	defer iter.Close() //nolint:errcheck
	examined, bytesRead := 0, 0
	valid := iter.First()
	for valid {
		if d.preloadCount >= d.PreloadMaxMessages || examined >= d.Limits.Examined || bytesRead >= d.Limits.Bytes || (examined > 0 && time.Since(started) >= d.Limits.WorkTime) || !d.hub.DeliveryPermitted() {
			break
		}
		key, value := iter.Key(), iter.Value()
		examined++
		bytesRead += len(key) + len(value)
		metrics.DeliveryDiscoveryBytesTotal.Add(float64(len(key) + len(value)))
		p.after = append(append([]byte(nil), key...), 0)
		if len(key) == 24 {
			scanned++
			if len(view.localOwners(key)) == 0 {
				filtered++
				valid = iter.Next()
				continue
			}
			candidate := preloadCandidate{key: [24]byte(key)}
			if !p.keys[candidate.key] {
				payload, err := cachePayload(iter, key, value)
				if err == nil {
					msg := payload.message
					candidate.payload = payload
					candidate.due = msg.EnqueuedAtUnixMs + msg.DelayMs
					if candidate.due > nowMs && candidate.due <= horizon && !d.isExpired(msg, nowMs) {
						if p.keys == nil {
							p.keys = make(map[[24]byte]bool)
						}
						heap.Push(&p.items, candidate)
						p.keys[candidate.key] = true
						d.preloadCount++
						loaded++
					}
				}
			}
		}
		valid = iter.Next()
	}
	if !valid || iter.Error() != nil {
		p.after = nil
	}
}

func (d *Dispatcher) dropPreload(topic string) {
	if p := d.preloads[topic]; p != nil {
		if p.valid != nil {
			p.valid.Store(false)
		}
		for _, candidate := range p.items {
			if candidate.ready {
				d.readyCount--
			} else {
				d.preloadCount--
			}
		}
		p.items, p.keys = nil, nil
		delete(d.preloads, topic)
	}
	d.observePreload()
}

func (d *Dispatcher) prunePreloads(live map[string]bool) {
	epoch, active := d.hub.DeliveryEpoch()
	for topic, p := range d.preloads {
		if !live[topic] || !active || p.epoch != epoch {
			d.dropPreload(topic)
		}
	}
}

// Each collection path calls this once, after closing its iterator. Keeping the
// aggregate and path samples together prevents preload work disappearing from
// the existing scan-cost panels. Cache residency time is deliberately excluded.
func observeCollection(path string, started time.Time) {
	elapsed := float64(time.Since(started).Microseconds()) / 1000
	metrics.DeliveryScanDurationMs.Observe(elapsed)
	metrics.DeliveryCollectionDurationMs.WithLabelValues(path).Observe(elapsed)
}

// Only the scheduler accesses preload state. Prometheus scrapes read the gauges,
// never the heap or maps, so collection cannot race with scheduler mutations.
func (d *Dispatcher) observePreload() {
	if d.scanOnly {
		return
	}
	metrics.DeliveryQueueDepth.WithLabelValues("preload").Set(float64(d.preloadCount))
	capacity := 0
	if d.PreloadLookahead > 0 && d.PreloadMaxMessages > 0 {
		capacity = d.PreloadMaxMessages
	}
	metrics.DeliveryPreloadCapacity.Set(float64(capacity))
}
