package dispatcher

import (
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
)

type localGroup struct {
	members   []metadata.ConsumerMember
	consumers []*ConsumerEntry
}

// InterestView freezes both the local subscriptions and the global assignment
// used to filter a chunk. Queueing still validates the live epoch under the
// rebalance lock; this view is never itself a delivery permit.
type InterestView struct {
	Epoch      uint64
	Active     bool
	Recipients []string
	groups     []localGroup
	universal  []*ConsumerEntry
}

func (h *Hub) SetDeliveryView(source func(string) metadata.TopicDeliveryView) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	h.deliveryView = source
}

func (h *Hub) InterestView(topic string) InterestView {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	if !h.permitted() {
		return InterestView{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	view := InterestView{Active: true, Epoch: h.registryEpoch}
	var global metadata.TopicDeliveryView
	if h.deliveryView != nil {
		global = h.deliveryView(topic)
		if !global.Active || global.Epoch != h.permitEpoch {
			return InterestView{}
		}
		view.Epoch, view.Recipients = global.Epoch, global.Recipients
	} else if h.version != nil {
		view.Epoch = h.permitEpoch
	}
	sub := h.topics[topic]
	if sub == nil {
		return view
	}
	for group, consumers := range sub.groups {
		entry := localGroup{consumers: append([]*ConsumerEntry(nil), consumers...)}
		if h.deliveryView != nil {
			entry.members = global.Groups[group]
		} else if h.groupMembers != nil {
			entry.members = h.groupMembers(topic, group)
		}
		// A non-nil membership source must not fall back to local ownership.
		if (h.deliveryView != nil || h.groupMembers != nil) && len(entry.members) == 0 {
			continue
		}
		view.groups = append(view.groups, entry)
		if h.deliveryView == nil {
			view.Recipients = append(view.Recipients, metadata.DeliveryRecipient(group, ""))
		}
	}
	for id, consumer := range sub.universal {
		eligible := h.deliveryView == nil
		for _, member := range global.Groups[""] {
			if member.ID == id {
				eligible = true
				break
			}
		}
		if eligible {
			view.universal = append(view.universal, consumer)
		}
		if h.deliveryView == nil {
			view.Recipients = append(view.Recipients, metadata.DeliveryRecipient("", id))
		}
	}
	return view
}

// localOwners uses the committed member order and deliveryOrdinal for every
// represented group independently. In standalone mode any group member can
// serve; selection remains with the configured dispatch strategy.
func (v InterestView) localOwners(key []byte) []*ConsumerEntry {
	owners := append([]*ConsumerEntry(nil), v.universal...)
	for _, group := range v.groups {
		if len(group.members) == 0 {
			owners = append(owners, group.consumers...)
			continue
		}
		id := group.members[deliveryOrdinal(key)%uint64(len(group.members))].ID
		for _, consumer := range group.consumers {
			if consumer.ID == id {
				owners = append(owners, consumer)
				break
			}
		}
	}
	return owners
}

func (h *Hub) hasReadyOwner(owners []*ConsumerEntry, key []byte) bool {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	for _, owner := range owners {
		if _, exists := h.deliveryRecords[owner.ID][string(key)]; !exists {
			return true
		}
	}
	return false
}

func (h *Hub) DeliveryPermitted() bool {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	return h.permitted()
}

func (h *Hub) DeliveryEpoch() (uint64, bool) {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	if h.version != nil {
		return h.version()
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.registryEpoch, true
}

func (h *Hub) PermitNeedsRenewal(budget time.Duration) bool {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	return h.version != nil && (!h.permitted() || time.Until(h.permitUntil) <= budget)
}

// BeginSend validates the exact buffered pointer and authorizes the actual
// attempt. ACKs are accepted only after this point. The in-flight drain keeps
// rebalances safe while the network send runs outside these locks.
func (h *Hub) BeginSend(consumerID string, msg *pb.QueueMessage) bool {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	if !h.permitted() {
		metrics.DeliveryRejectedTotal.WithLabelValues("permit").Inc()
		return false
	}
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	record, exists := h.deliveryRecords[consumerID][string(msg.DeliveryTag)]
	if !exists || record.message != msg || !time.Now().Before(record.expires) || (h.version != nil && record.epoch != h.permitEpoch) {
		return false
	}
	if time.Now().UnixMilli() < msg.EnqueuedAtUnixMs+msg.DelayMs {
		return false
	}
	record.sending = true
	h.deliveryRecords[consumerID][string(msg.DeliveryTag)] = record
	return true
}

// RejectQueuedAttempt cannot erase a retry with the same key: only the exact
// buffered pointer can remove its record. Wake scanning to revisit the key.
func (h *Hub) RejectQueuedAttempt(consumerID string, msg *pb.QueueMessage) {
	h.inFlightMu.Lock()
	record, exists := h.deliveryRecords[consumerID][string(msg.DeliveryTag)]
	if exists && record.message == msg && !record.sending {
		delete(h.deliveryRecords[consumerID], string(msg.DeliveryTag))
		keys := h.inFlightByConsumer[consumerID]
		for i, key := range keys {
			if string(key) == string(msg.DeliveryTag) {
				h.inFlightByConsumer[consumerID] = append(keys[:i], keys[i+1:]...)
				break
			}
		}
	}
	h.inFlightMu.Unlock()
	select {
	case h.wakeCh <- struct{}{}:
	default:
	}
}

func (h *Hub) AcknowledgeSent(consumerID string, key []byte) bool {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	record, exists := h.deliveryRecords[consumerID][string(key)]
	if !exists || !record.sending {
		return false
	}
	delete(h.deliveryRecords[consumerID], string(key))
	keys := h.inFlightByConsumer[consumerID]
	for i, pending := range keys {
		if string(pending) == string(key) {
			h.inFlightByConsumer[consumerID] = append(keys[:i], keys[i+1:]...)
			break
		}
	}
	return true
}

func (h *Hub) ObserveQueues() {
	h.mu.RLock()
	depth := 0
	for _, consumer := range h.byID {
		depth += len(consumer.Ch)
	}
	h.mu.RUnlock()
	h.inFlightMu.Lock()
	oldest := int64(0)
	now := time.Now().UnixMilli()
	for _, records := range h.deliveryRecords {
		for _, record := range records {
			if !record.sending {
				late := now - record.message.EnqueuedAtUnixMs - record.message.DelayMs
				if late > oldest {
					oldest = late
				}
			}
		}
	}
	h.inFlightMu.Unlock()
	metrics.DeliveryQueueDepth.WithLabelValues("consumer").Set(float64(depth))
	metrics.DeliveryOldestQueuedLatenessMs.Set(float64(oldest))
}
