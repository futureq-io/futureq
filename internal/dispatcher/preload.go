package dispatcher

import (
	"bytes"
	"container/heap"
	"encoding/json"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// Preloading retains only fixed-size candidate metadata. Payloads and recipient
// manifests remain authoritative in storage and are read by the unchanged
// preparation/delivery path. No iterator or ownership decision survives a scan.
type preloadCandidate struct {
	key [24]byte
	due int64
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
	*h = (*h)[:last]
	return v
}

type topicPreload struct {
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
	if d.PreloadLookahead <= 0 || d.PreloadMaxMessages <= 0 {
		return d.collectTopic(topic, nowMs)
	}
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
	chunk := d.collectTopic(topic, nowMs)
	// Prioritize due work; speculative I/O never delays an already found chunk.
	if len(chunk.items) == 0 {
		d.preloadTopic(topic, p, view, nowMs)
	}
	return chunk
}

func (d *Dispatcher) collectPreloaded(topic string, p *topicPreload, view InterestView, nowMs int64) deliveryChunk {
	started := time.Now()
	chunk := deliveryChunk{topic: topic, scanStarted: nowMs, started: started}
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
		d.preloadCount--
		examined++
		if err != nil || commandBytes+len(encoded)+1 > d.Limits.Bytes {
			continue // ordinary scanning reports an oversized recipient set
		}
		owners := view.localOwners(item.Key)
		if len(owners) == 0 || !d.hub.hasReadyOwner(owners, item.Key) {
			continue
		}
		commandBytes += len(encoded) + 1
		chunk.items = append(chunk.items, item)
		if chunk.oldestDueMs == 0 || candidate.due < chunk.oldestDueMs {
			chunk.oldestDueMs = candidate.due
		}
	}
	if len(p.items) == 0 {
		p.items, p.keys = nil, nil // release capacity when a topic drains
	}
	return chunk
}

// preloadTopic reads only the moving lookahead range, with its own cyclic
// cursor and the existing key/byte/time budgets. Full buffers stop speculative
// reads, while live due scans continue independently. Coarse buckets require
// checking the exact message due time before retaining a candidate.
func (d *Dispatcher) preloadTopic(topic string, p *topicPreload, view InterestView, nowMs int64) {
	if d.preloadCount >= d.PreloadMaxMessages {
		return
	}
	started := time.Now()
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
		p.after = append(append([]byte(nil), key...), 0)
		if len(key) == 24 && len(view.localOwners(key)) > 0 {
			candidate := preloadCandidate{key: [24]byte(key)}
			if !p.keys[candidate.key] {
				var msg storagepb.StoredMessage
				if proto.Unmarshal(value, &msg) == nil {
					candidate.due = msg.EnqueuedAtUnixMs + msg.DelayMs
					if candidate.due > nowMs && candidate.due <= horizon && !d.isExpired(&msg, nowMs) {
						if p.keys == nil {
							p.keys = make(map[[24]byte]bool)
						}
						heap.Push(&p.items, candidate)
						p.keys[candidate.key] = true
						d.preloadCount++
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
		d.preloadCount -= len(p.items)
		delete(d.preloads, topic)
	}
}

func (d *Dispatcher) prunePreloads(live map[string]bool) {
	epoch, active := d.hub.DeliveryEpoch()
	for topic, p := range d.preloads {
		if !live[topic] || !active || p.epoch != epoch {
			d.dropPreload(topic)
		}
	}
}

func (d *Dispatcher) nextPreloadDue(busy map[string]bool) int64 {
	var due int64
	for topic, p := range d.preloads {
		if !busy[topic] && len(p.items) > 0 && (due == 0 || p.items[0].due < due) {
			due = p.items[0].due
		}
	}
	return due
}
