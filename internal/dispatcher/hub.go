package dispatcher

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	"go.uber.org/zap"
)

// ─── Dispatch Strategy ───────────────────────────────────────────────────────

// DispatchStrategy selects one consumer from a group to receive a message.
// Implementations must be safe for concurrent use.
type DispatchStrategy interface {
	// Select returns the consumer that should receive the message, or nil if
	// no consumer is available. The candidates slice is a snapshot — safe to
	// read without holding locks.
	Select(candidates []*ConsumerEntry, msg *pb.QueueMessage) *ConsumerEntry
}

// RoundRobinStrategy dispatches messages to consumers in rotating order.
type RoundRobinStrategy struct {
	mu   sync.Mutex
	next map[string]uint64 // groupKey → next index
}

// NewRoundRobinStrategy returns a RoundRobinStrategy.
func NewRoundRobinStrategy() *RoundRobinStrategy {
	return &RoundRobinStrategy{
		next: make(map[string]uint64),
	}
}

// Select picks the next consumer in round-robin order for the given group.
func (s *RoundRobinStrategy) Select(candidates []*ConsumerEntry, _ *pb.QueueMessage) *ConsumerEntry {
	if len(candidates) == 0 {
		return nil
	}

	// Use the first candidate's group key for the round-robin counter.
	groupKey := candidates[0].GroupKey()

	s.mu.Lock()
	idx := s.next[groupKey]
	// Keep a monotonic cursor so a changed group size immediately uses the
	// new modulus instead of restarting at a stale wrapped position.
	s.next[groupKey] = idx + 1
	s.mu.Unlock()

	return candidates[idx%uint64(len(candidates))]
}

// ─── Consumer Entry ──────────────────────────────────────────────────────────

// ConsumerEntry holds one consumer's state.
type ConsumerEntry struct {
	ID    string
	Topic string
	Group string
	Ch    chan *pb.QueueMessage
}

// GroupKey returns a canonical key for the consumer's (topic, group) pair.
// Universal consumers (empty group) each get their own unique key so they
// never compete with each other.
func (c *ConsumerEntry) GroupKey() string {
	if c.Group == "" {
		return fmt.Sprintf("%s|__universal__|%s", c.Topic, c.ID)
	}
	return fmt.Sprintf("%s|%s", c.Topic, c.Group)
}

// IsUniversal returns true if this consumer receives every message on the
// topic (no group — fan-out).
func (c *ConsumerEntry) IsUniversal() bool {
	return c.Group == ""
}

// ─── Topic Subscription ─────────────────────────────────────────────────────

// TopicSubscription tracks all consumers for a single topic, organised by
// group. Universal consumers (empty group) are stored individually.
type TopicSubscription struct {
	// groups: groupID → []*ConsumerEntry (competing consumers)
	groups map[string][]*ConsumerEntry

	// universal: consumerID → *ConsumerEntry (fan-out consumers)
	universal map[string]*ConsumerEntry
}

// newTopicSubscription returns an empty TopicSubscription.
func newTopicSubscription() *TopicSubscription {
	return &TopicSubscription{
		groups:    make(map[string][]*ConsumerEntry),
		universal: make(map[string]*ConsumerEntry),
	}
}

// add inserts a consumer into the appropriate bucket.
func (ts *TopicSubscription) add(c *ConsumerEntry) {
	if c.IsUniversal() {
		ts.universal[c.ID] = c
		return
	}
	ts.groups[c.Group] = append(ts.groups[c.Group], c)
}

// remove deletes a consumer. Returns true if the subscription is now empty.
func (ts *TopicSubscription) remove(c *ConsumerEntry) bool {
	if c.IsUniversal() {
		delete(ts.universal, c.ID)
	} else {
		group := ts.groups[c.Group]
		for i, ce := range group {
			if ce.ID == c.ID {
				ts.groups[c.Group] = append(group[:i], group[i+1:]...)
				break
			}
		}
		if len(ts.groups[c.Group]) == 0 {
			delete(ts.groups, c.Group)
		}
	}
	return ts.isEmpty()
}

// isEmpty returns true if no consumers remain on this topic.
func (ts *TopicSubscription) isEmpty() bool {
	return len(ts.groups) == 0 && len(ts.universal) == 0
}

// groupSnapshot returns a snapshot of all groups (non-universal).
func (ts *TopicSubscription) groupSnapshot() map[string][]*ConsumerEntry {
	snap := make(map[string][]*ConsumerEntry, len(ts.groups))
	for gid, consumers := range ts.groups {
		cp := make([]*ConsumerEntry, len(consumers))
		copy(cp, consumers)
		snap[gid] = cp
	}
	return snap
}

// universalSnapshot returns a snapshot of all universal consumers.
func (ts *TopicSubscription) universalSnapshot() []*ConsumerEntry {
	snap := make([]*ConsumerEntry, 0, len(ts.universal))
	for _, c := range ts.universal {
		snap = append(snap, c)
	}
	return snap
}

// ─── Hub ─────────────────────────────────────────────────────────────────────

// Hub manages consumer connections indexed by topic.
//
// Delivery semantics:
//   - Universal consumers (empty group): each receives every message (fan-out).
//   - Grouped consumers: within each group, exactly one consumer receives each
//     message (competing consumers). Different groups each get an independent
//     copy (fan-out across groups).
type Hub struct {
	mu         sync.RWMutex
	registryMu sync.Mutex
	// deliveryMu fences every delivery against a metadata Raft rebalance.
	deliveryMu      sync.RWMutex
	groupMembers    func(topic, group string) []metadata.ConsumerMember
	version         func() (uint64, bool)
	permitEpoch     uint64
	permitUntil     time.Time
	registryEpoch   uint64
	inFlightTimeout time.Duration

	// topics: topic → *TopicSubscription
	topics map[string]*TopicSubscription

	// byID: consumerID → *ConsumerEntry (fast lookup for unregister)
	byID map[string]*ConsumerEntry

	// inFlightByConsumer: consumerID → [][]byte (keys in-flight to that consumer)
	inFlightByConsumer map[string][][]byte
	inFlightMu         sync.Mutex
	deliveryRecords    map[string]map[string]deliveryRecord

	strategy DispatchStrategy
	logger   *zap.Logger
	wakeCh   chan struct{}
	// OnNack immediately clears the dispatcher's local in-flight entry.
	OnNack func([]byte)
}

// WithConsumerRegistry serializes local registration proposals and stale-ID
// reconciliation so an old snapshot cannot erase a newly added consumer.
func (h *Hub) WithConsumerRegistry(apply func() error) error {
	h.registryMu.Lock()
	defer h.registryMu.Unlock()
	return apply()
}

// SetGroupMembership switches grouped delivery to the metadata Raft view.
// Call before starting the dispatcher or accepting streams.
func (h *Hub) SetGroupMembership(source func(topic, group string) []metadata.ConsumerMember) {
	h.deliveryMu.Lock()
	h.groupMembers = source
	h.deliveryMu.Unlock()
}

// WithRebalanceBarrier waits for active local sends, then applies a Raft
// membership change while new sends are excluded.
func (h *Hub) WithRebalanceBarrier(apply func()) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	h.permitUntil = time.Time{}
	apply()
}

// LocalConsumerIDs returns the subscriptions to retain when reconciling a
// restarted node's stale group members in metadata Raft.
func (h *Hub) LocalConsumerIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.byID))
	for id := range h.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// NewHub constructs a Hub. wakeCh is signalled when a new consumer connects,
// causing the dispatcher to immediately scan for due messages.
func NewHub(strategy DispatchStrategy, logger *zap.Logger, wakeCh chan struct{}) *Hub {
	return &Hub{
		topics:             make(map[string]*TopicSubscription),
		byID:               make(map[string]*ConsumerEntry),
		inFlightByConsumer: make(map[string][][]byte),
		deliveryRecords:    make(map[string]map[string]deliveryRecord),
		inFlightTimeout:    5 * time.Second,
		strategy:           strategy,
		logger:             logger.Named("hub"),
		wakeCh:             wakeCh,
	}
}

// Register adds a consumer to the Hub under the given topic and group.
// An empty groupID registers a universal (fan-out) consumer.
func (h *Hub) Register(id, topic, groupID string, ch chan *pb.QueueMessage) {
	entry := &ConsumerEntry{
		ID:    id,
		Topic: topic,
		Group: groupID,
		Ch:    ch,
	}

	h.mu.Lock()
	if h.topics[topic] == nil {
		h.topics[topic] = newTopicSubscription()
	}
	h.topics[topic].add(entry)
	h.byID[id] = entry
	h.registryEpoch++
	h.mu.Unlock()

	h.logger.Info("consumer registered",
		zap.String("id", id),
		zap.String("topic", topic),
		zap.String("group", groupID),
		zap.Bool("universal", entry.IsUniversal()),
	)

	// Wake the dispatcher loop immediately.
	select {
	case h.wakeCh <- struct{}{}:
	default:
	}
}

// Unregister removes a consumer from the Hub and deletes its in-flight keys.
func (h *Hub) Unregister(id string) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	h.mu.Lock()
	entry, ok := h.byID[id]
	if !ok {
		h.mu.Unlock()
		return
	}

	if sub, exists := h.topics[entry.Topic]; exists {
		if sub.remove(entry) {
			delete(h.topics, entry.Topic)
		}
	}
	delete(h.byID, id)
	h.registryEpoch++
	h.mu.Unlock()

	h.logger.Info("consumer unregistered",
		zap.String("id", id),
		zap.String("topic", entry.Topic),
		zap.String("group", entry.Group),
	)

	// Delete in-flight keys for this consumer.
	h.inFlightMu.Lock()
	delete(h.inFlightByConsumer, id)
	delete(h.deliveryRecords, id)
	h.inFlightMu.Unlock()
}

// DispatchToTopic sends a message to all eligible consumers on a topic.
// For each group, the strategy selects one consumer. Universal consumers each
// receive a copy. Returns the group IDs the message was successfully sent to
// (universal consumers are reported with an empty group id).
func (h *Hub) DispatchToTopic(topic string, msg *pb.QueueMessage, deliveryTag []byte) []string {
	return h.DispatchPrepared(topic, msg, deliveryTag, nil)
}

// DispatchPrepared skips interests already completed in the durable manifest.
// A nil manifest retains the Hub's direct-use behavior for callers without a ledger.
func (h *Hub) DispatchPrepared(topic string, msg *pb.QueueMessage, deliveryTag []byte, manifest *raft.DeliveryState) []string {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	if !h.permitted() {
		return nil
	}
	h.mu.RLock()
	sub, ok := h.topics[topic]
	if !ok {
		h.mu.RUnlock()
		return nil
	}

	// Snapshot groups and universal consumers while holding the lock.
	groups := sub.groupSnapshot()
	universal := sub.universalSnapshot()
	h.mu.RUnlock()

	sentTo := make([]string, 0, len(groups)+len(universal))

	// Dispatch to each group — strategy picks one consumer per group.
	for gid, consumers := range groups {
		if manifest != nil && !manifest.Needs(metadata.DeliveryRecipient(gid, "")) {
			continue
		}
		var selected *ConsumerEntry
		if h.groupMembers != nil {
			members := h.groupMembers(topic, gid)
			if len(members) == 0 {
				continue // no committed assignment, or rebalance is pending
			}
			owner := members[deliveryOrdinal(deliveryTag)%uint64(len(members))].ID
			for _, consumer := range consumers {
				if consumer.ID == owner {
					selected = consumer
					break
				}
			}
		} else {
			selected = h.strategy.Select(consumers, msg)
		}
		if selected == nil {
			continue
		}
		if h.trySend(selected, msg, deliveryTag) {
			sentTo = append(sentTo, gid)
		}
	}

	// Dispatch to all universal consumers.
	for _, c := range universal {
		if manifest != nil && !manifest.Needs(metadata.DeliveryRecipient("", c.ID)) {
			continue
		}
		if h.trySend(c, msg, deliveryTag) {
			sentTo = append(sentTo, "")
		}
	}

	return sentTo
}

// deliveryOrdinal is the event ID in the topic-first storage key. Using the
// same ordinal on every replica gives a stable modulo assignment.
func deliveryOrdinal(tag []byte) uint64 {
	if len(tag) == 24 {
		return binary.BigEndian.Uint64(tag[16:24])
	}
	h := fnv.New64a()
	_, _ = h.Write(tag)
	return h.Sum64()
}

// trySend attempts to deliver a message to a single consumer. Returns true on
// success. Tracks the delivery tag as in-flight for the consumer.
func (h *Hub) trySend(c *ConsumerEntry, msg *pb.QueueMessage, deliveryTag []byte) bool {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	if h.deliveryRecords[c.ID] == nil {
		h.deliveryRecords[c.ID] = make(map[string]deliveryRecord)
	}
	if _, exists := h.deliveryRecords[c.ID][string(deliveryTag)]; exists {
		return false
	}
	// Separate pointers let the sender reject an old queued attempt after a
	// timeout, even if the same key has since been delivered to this stream again.
	queued := &pb.QueueMessage{
		Topic: msg.Topic, Payload: msg.Payload, DeliveryTag: deliveryTag,
		EnqueuedAtUnixMs: msg.EnqueuedAtUnixMs, DelayMs: msg.DelayMs,
	}
	select {
	case c.Ch <- queued:
		h.inFlightByConsumer[c.ID] = append(h.inFlightByConsumer[c.ID], deliveryTag)
		h.deliveryRecords[c.ID][string(deliveryTag)] = deliveryRecord{message: queued, expires: time.Now().Add(h.inFlightTimeout), epoch: h.permitEpoch}
		return true
	default:
		h.logger.Warn("consumer channel full, skipping",
			zap.String("consumer_id", c.ID),
			zap.String("topic", c.Topic),
			zap.String("group", c.Group),
		)
		return false
	}
}

// RemoveInFlightForConsumer removes a specific key from a consumer's in-flight
// tracking. Called when the consumer ACKs or NACKs a message.
func (h *Hub) RemoveInFlightForConsumer(consumerID string, key []byte) bool {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	keys := h.inFlightByConsumer[consumerID]
	for i, k := range keys {
		if bytes.Equal(k, key) {
			h.inFlightByConsumer[consumerID] = append(keys[:i], keys[i+1:]...)
			delete(h.deliveryRecords[consumerID], string(key))
			return true
		}
	}
	return false
}

// HasInFlightForConsumer validates an ACK against a message actually sent on
// this stream, so an arbitrary delivery tag cannot delete another message.
func (h *Hub) HasInFlightForConsumer(consumerID string, key []byte) bool {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	for _, k := range h.inFlightByConsumer[consumerID] {
		if bytes.Equal(k, key) {
			return true
		}
	}
	return false
}

// RemoveDeletedBatch clears tracking when a delete applies or an attempt times out.
func (h *Hub) RemoveDeletedBatch(keys [][]byte) {
	deleted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		deleted[string(key)] = struct{}{}
	}
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	for id, pending := range h.inFlightByConsumer {
		kept := pending[:0]
		for _, key := range pending {
			if _, ok := deleted[string(key)]; !ok {
				kept = append(kept, key)
			} else {
				delete(h.deliveryRecords[id], string(key))
			}
		}
		h.inFlightByConsumer[id] = kept
	}
}

// GroupInFlightCount is the number of deliveries that must finish before this
// replica acknowledges a rebalance freeze.
func (h *Hub) GroupInFlightCount() int {
	h.ExpireInFlight(time.Now())
	h.mu.RLock()
	defer h.mu.RUnlock()
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	count := 0
	for id, entry := range h.byID {
		if entry.Group != "" {
			count += len(h.inFlightByConsumer[id])
		}
	}
	return count
}

// ActiveTopics returns a snapshot of all topics that currently have at least
// one connected consumer.
func (h *Hub) ActiveTopics() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]string, 0, len(h.topics))
	for topic := range h.topics {
		result = append(result, topic)
	}
	return result
}

// HasConsumers returns true if at least one consumer is currently connected.
func (h *Hub) HasConsumers() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byID) > 0
}

// GroupsForTopic returns a snapshot of all group IDs that have active consumers
// for the given topic. Does not include universal consumers.
func (h *Hub) GroupsForTopic(topic string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	sub, ok := h.topics[topic]
	if !ok {
		return nil
	}
	result := make([]string, 0, len(sub.groups))
	for gid, consumers := range sub.groups {
		if len(consumers) > 0 {
			result = append(result, gid)
		}
	}
	return result
}
