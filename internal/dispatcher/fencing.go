package dispatcher

import (
	"context"
	"sort"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/lni/dragonboat/v4"
)

// ConsumerReadPermit is the default delivery permit and legacy fencing floor.
// Production fencing uses the maximum permit registered in metadata Raft.
const ConsumerReadPermit = time.Second

// SetReadPermit is installed before serving consumers. The same duration must
// be advertised by the metadata service when registering those consumers.
func (h *Hub) SetReadPermit(duration time.Duration) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	if duration > 0 {
		h.readPermit = duration
	}
}

func (h *Hub) DeliveryReadPermit() time.Duration {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	return max(h.readPermit, time.Nanosecond)
}

type deliveryRecord struct {
	message    *pb.QueueMessage
	topic      string
	group      string
	expires    time.Time
	epoch      uint64
	sending    bool // true only after the sender authorizes an actual stream send
	ackPending bool // ACK received, but persistence has not completed
}

func (h *Hub) SetDeliveryTimeout(timeout time.Duration) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	h.inFlightTimeout = timeout
}

// NewConsumerReadBarrier checks both Raft shards before a replica scans. Every
// channel send and gRPC sender checks the resulting permit's epoch and expiry.
func NewConsumerReadBarrier(ctx context.Context, nh *dragonboat.NodeHost, sm *metadata.MetadataStateMachine, hub *Hub, shardID uint64) func() error {
	return newConsumerReadBarrier(ctx, func(ctx context.Context, shard uint64) error {
		_, err := nh.SyncRead(ctx, shard, nil)
		return err
	}, sm.ConsumerVersion, hub, shardID)
}

func newConsumerReadBarrier(ctx context.Context, read func(context.Context, uint64) error, version func() (uint64, bool), hub *Hub, shardID uint64) func() error {
	return func() error {
		started := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		// These shards have independent read indexes. Both must succeed, but
		// neither read needs the other's result. Join both, including on error.
		results := make(chan error, 2)
		for _, shard := range []uint64{metadata.MetadataShardID, shardID} {
			go func() { results <- read(callCtx, shard) }()
		}
		var readErr error
		for range 2 {
			if err := <-results; err != nil && readErr == nil {
				readErr = err
				cancel()
			}
		}
		if readErr != nil {
			return readErr
		}
		epoch, _ := version()
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
		until := readStarted.Add(h.readPermit)
		if h.permitEpoch == epoch && !until.After(h.permitUntil) {
			return // a slower, older read cannot shorten a newer same-epoch grant
		}
		h.permitEpoch = epoch
		h.permitUntil = until
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
	return exists && !record.ackPending && record.message == msg && time.Now().Before(record.expires) && (h.version == nil || record.epoch == h.permitEpoch)
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
				h.removeDeliveryRecordLocked(id, string(key))
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

func (d *RebalanceDrain) Expired(epoch uint64, pending bool, now time.Time, timeout time.Duration, permitBound ...time.Duration) bool {
	if !pending {
		d.since = time.Time{}
		return false
	}
	if d.since.IsZero() || epoch != d.epoch {
		d.epoch, d.since = epoch, now
	}
	permit := ConsumerReadPermit
	if len(permitBound) > 0 {
		permit = max(permit, permitBound[0])
	}
	return now.Sub(d.since) >= timeout+permit
}
