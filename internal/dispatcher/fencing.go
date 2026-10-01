package dispatcher

import (
	"context"
	"sort"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/lni/dragonboat/v4"
)

// ConsumerReadPermit bounds delivery after the last successful quorum read.
// Fence proposals wait for this permit plus the configured delivery timeout.
const ConsumerReadPermit = time.Second

type deliveryRecord struct {
	message *pb.QueueMessage
	expires time.Time
	epoch   uint64
	sending bool // true only after the sender authorizes an actual stream send
}

func (h *Hub) SetDeliveryTimeout(timeout time.Duration) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	h.inFlightTimeout = timeout
}

// NewConsumerReadBarrier checks both Raft shards before a replica scans. Every
// channel send and gRPC sender checks the resulting permit's epoch and expiry.
func NewConsumerReadBarrier(ctx context.Context, nh *dragonboat.NodeHost, sm *metadata.MetadataStateMachine, hub *Hub, shardID uint64) func() error {
	return func() error {
		started := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := nh.SyncRead(callCtx, metadata.MetadataShardID, nil); err != nil {
			return err
		}
		if _, err := nh.SyncRead(callCtx, shardID, nil); err != nil {
			return err
		}
		epoch, _ := sm.ConsumerVersion()
		hub.GrantDeliveryPermit(epoch, started)
		return nil
	}
}

func (h *Hub) SetDeliveryFence(version func() (uint64, bool), timeout time.Duration) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	h.version = version
	h.inFlightTimeout = timeout
}

// GrantDeliveryPermit uses the start of the quorum read, never its completion:
// a delayed read response cannot extend an old assignment's authorization.
func (h *Hub) GrantDeliveryPermit(epoch uint64, readStarted time.Time) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	current, active := h.version()
	if current == epoch && active {
		h.permitEpoch = epoch
		h.permitUntil = readStarted.Add(ConsumerReadPermit)
	}
}

// permitted requires deliveryMu to be held. Minority partitions, delayed
// reads, and newly installed snapshots cannot keep serving stale assignments.
func (h *Hub) permitted() bool {
	if h.version == nil {
		return true
	}
	epoch, active := h.version()
	return active && epoch == h.permitEpoch && time.Now().Before(h.permitUntil)
}

func (h *Hub) CanSend(consumerID string, msg *pb.QueueMessage) bool {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	if !h.permitted() {
		return false
	}
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	record, exists := h.deliveryRecords[consumerID][string(msg.DeliveryTag)]
	return exists && record.message == msg && time.Now().Before(record.expires) && (h.version == nil || record.epoch == h.permitEpoch)
}

// ExpireInFlight runs even while dispatch is frozen. Stalled consumers cannot
// hold a rebalance forever, and old buffered sends are rejected by CanSend.
func (h *Hub) ExpireInFlight(now time.Time) {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	for id, keys := range h.inFlightByConsumer {
		kept := keys[:0]
		for _, key := range keys {
			if record, exists := h.deliveryRecords[id][string(key)]; exists && now.Before(record.expires) {
				kept = append(kept, key)
			} else {
				delete(h.deliveryRecords[id], string(key))
			}
		}
		h.inFlightByConsumer[id] = kept
	}
}

func (h *Hub) LocalConsumers() []ConsumerEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	entries := make([]ConsumerEntry, 0, len(h.byID))
	for _, entry := range h.byID {
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

func (h *Hub) LocalDeliveryRecipients(topic string) (uint64, []string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var recipients []string
	if subscription := h.topics[topic]; subscription != nil {
		for group := range subscription.groups {
			recipients = append(recipients, metadata.DeliveryRecipient(group, ""))
		}
		for id := range subscription.universal {
			recipients = append(recipients, metadata.DeliveryRecipient("", id))
		}
	}
	sort.Strings(recipients)
	return h.registryEpoch, recipients, true
}

// RebalanceDrain bounds the wait for an observed pending epoch. All replicas
// use their own monotonic observation time; delayed observers wait longer.
type RebalanceDrain struct {
	epoch uint64
	since time.Time
}

func (d *RebalanceDrain) Expired(epoch uint64, pending bool, now time.Time, timeout time.Duration) bool {
	if !pending {
		d.since = time.Time{}
		return false
	}
	if d.since.IsZero() || epoch != d.epoch {
		d.epoch, d.since = epoch, now
	}
	return now.Sub(d.since) >= timeout+ConsumerReadPermit
}
