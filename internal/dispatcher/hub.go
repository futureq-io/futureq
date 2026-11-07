package dispatcher

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
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

	// Rebuilt under Hub.mu only when subscriptions change. Published snapshots
	// own their maps/slices and remain immutable across later mutations.
	groupsView    map[string][]*ConsumerEntry
	universalView []*ConsumerEntry
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
	defer ts.rebuildSnapshots()
	if c.IsUniversal() {
		ts.universal[c.ID] = c
		return
	}
	ts.groups[c.Group] = append(ts.groups[c.Group], c)
}

// remove deletes a consumer. Returns true if the subscription is now empty.
func (ts *TopicSubscription) remove(c *ConsumerEntry) bool {
	defer ts.rebuildSnapshots()
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
	return ts.groupsView
}

// universalSnapshot returns a snapshot of all universal consumers.
func (ts *TopicSubscription) universalSnapshot() []*ConsumerEntry {
	return ts.universalView
}

func (ts *TopicSubscription) rebuildSnapshots() {
	groups := make(map[string][]*ConsumerEntry, len(ts.groups))
	for id, consumers := range ts.groups {
		groups[id] = append([]*ConsumerEntry(nil), consumers...)
	}
	universal := make([]*ConsumerEntry, 0, len(ts.universal))
	for _, consumer := range ts.universal {
		universal = append(universal, consumer)
	}
	ts.groupsView, ts.universalView = groups, universal
}

// ─── Hub ─────────────────────────────────────────────────────────────────────

// groupDeliveryKey reserves one logical group delivery across all its members.
// Separate fields avoid collisions between arbitrary topic and group names.
type groupDeliveryKey struct {
	topic string
	group string
	key   string
}

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
	deliveryView    func(string) metadata.TopicDeliveryView
	version         func() (uint64, bool)
	permitEpoch     uint64
	permitUntil     time.Time
	readPermit      time.Duration
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
	groupInFlight      map[groupDeliveryKey]string // reservation → consumer ID; guarded by inFlightMu

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

// WithConsumerBarrier excludes sends while metadata is applied, revoking the
// permit only when ownership/activation changed (or a snapshot was restored).
func (h *Hub) WithConsumerBarrier(apply func() bool) {
	h.deliveryMu.Lock()
	defer h.deliveryMu.Unlock()
	if apply() {
		h.permitUntil = time.Time{}
	}
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
		groupInFlight:      make(map[groupDeliveryKey]string),
		inFlightTimeout:    5 * time.Second,
		readPermit:         ConsumerReadPermit,
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
	for key := range h.deliveryRecords[id] {
		h.removeDeliveryRecordLocked(id, key)
	}
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
		metrics.DeliveryRejectedTotal.WithLabelValues("permit").Inc()
		return nil
	}
	if manifest != nil && !manifest.PublishBound && h.version != nil && manifest.Epoch != h.permitEpoch {
		metrics.DeliveryRejectedTotal.WithLabelValues("epoch").Inc()
		return nil
	}
	if time.Now().UnixMilli() < msg.EnqueuedAtUnixMs+msg.DelayMs {
		metrics.DeliveryRejectedTotal.WithLabelValues("not_due").Inc()
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
	reservation := groupDeliveryKey{topic: c.Topic, group: c.Group, key: string(deliveryTag)}
	if c.Group != "" {
		if _, exists := h.groupInFlight[reservation]; exists {
			return false
		}
	}
	if h.deliveryRecords[c.ID] == nil {
		h.deliveryRecords[c.ID] = make(map[string]deliveryRecord)
	}
	// A fast sender with stalled ACKs must not turn a bounded channel into an
	// unbounded retry map. Expiry or ACK releases one of these fixed slots.
	if len(h.deliveryRecords[c.ID]) >= max(1024, cap(c.Ch)) {
		metrics.DeliveryRejectedTotal.WithLabelValues("in_flight_limit").Inc()
		return false
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
	// Capture before the channel handoff, but observe only successful enqueues.
	// This excludes time waiting in the sender queue and inside stream.Send.
	queuedAt := time.Now()
	select {
	case c.Ch <- queued:
		metrics.DeliverySenderEnqueueLatenessMs.WithLabelValues(msg.Topic).Observe(float64(queuedAt.UnixNano())/1e6 - float64(msg.EnqueuedAtUnixMs+msg.DelayMs))
		h.inFlightByConsumer[c.ID] = append(h.inFlightByConsumer[c.ID], deliveryTag)
		h.deliveryRecords[c.ID][string(deliveryTag)] = deliveryRecord{message: queued, topic: c.Topic, group: c.Group, expires: time.Now().Add(h.inFlightTimeout), epoch: h.permitEpoch}
		if c.Group != "" {
			h.groupInFlight[reservation] = c.ID
		}
		metrics.MessagesInFlight.WithLabelValues(c.Topic, c.Group).Inc()
		return true
	default:
		metrics.DeliveryRejectedTotal.WithLabelValues("full_queue").Inc()
		h.logger.Warn("consumer channel full, skipping",
			zap.String("consumer_id", c.ID),
			zap.String("topic", c.Topic),
			zap.String("group", c.Group),
		)
		return false
	}
}

// removeDeliveryRecordLocked releases metric accounting exactly once, even if
// timeout, disconnect, ACK persistence, and deletion revisit the same delivery.
// The caller must hold inFlightMu.
func (h *Hub) removeDeliveryRecordLocked(consumerID, key string) {
	if record, exists := h.deliveryRecords[consumerID][key]; exists {
		delete(h.deliveryRecords[consumerID], key)
		if record.group != "" {
			reservation := groupDeliveryKey{topic: record.topic, group: record.group, key: key}
			if h.groupInFlight[reservation] == consumerID {
				delete(h.groupInFlight, reservation)
			}
		}
		metrics.MessagesInFlight.WithLabelValues(record.topic, record.group).Dec()
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
			h.removeDeliveryRecordLocked(consumerID, string(key))
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
				h.removeDeliveryRecordLocked(id, string(key))
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

// RestoreStandaloneEpoch runs before accepting subscriptions or starting any
// workers. Existing receipts outlive the Hub, so a new process must start above
// their epochs to retire its predecessor's ephemeral universal interests.
func (h *Hub) RestoreStandaloneEpoch(db storage.DB) (err error) {
	prefix := []byte("metadata/delivery/")
	upper := append([]byte(nil), prefix...)
	upper[len(upper)-1]++
	iter, err := db.NewIter(&storage.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := iter.Close(); err == nil {
			err = closeErr
		}
	}()
	var epoch uint64
	for iter.First(); iter.Valid(); iter.Next() {
		receiptEpoch, err := raft.DeliveryStateEpoch(iter.Value())
		if err != nil {
			return fmt.Errorf("read standalone delivery epoch: %w", err)
		}
		epoch = max(epoch, receiptEpoch)
	}
	if err := iter.Error(); err != nil {
		return err
	}
	if epoch == math.MaxUint64 {
		return fmt.Errorf("standalone delivery epoch exhausted")
	}
	h.mu.Lock()
	h.registryEpoch = max(h.registryEpoch, epoch+1)
	h.mu.Unlock()
	return nil
}

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
	for group, consumers := range sub.groupSnapshot() {
		entry := localGroup{consumers: consumers}
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
		if owner.Group != "" {
			reservation := groupDeliveryKey{topic: owner.Topic, group: owner.Group, key: string(key)}
			if _, exists := h.groupInFlight[reservation]; exists {
				continue
			}
		}
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
	if !exists || record.sending || record.ackPending || record.message != msg || !time.Now().Before(record.expires) || (h.version != nil && record.epoch != h.permitEpoch) {
		return false
	}
	if time.Now().UnixMilli() < msg.EnqueuedAtUnixMs+msg.DelayMs {
		return false
	}
	record.sending = true
	// The queue has its own bounded lifetime. Once a send starts, grant a full
	// ACK window so time buffered behind another send does not shorten it.
	record.expires = time.Now().Add(h.inFlightTimeout)
	h.deliveryRecords[consumerID][string(msg.DeliveryTag)] = record
	return true
}

// RejectQueuedAttempt cannot erase a retry with the same key: only the exact
// buffered pointer can remove its record. Wake scanning to revisit the key.
func (h *Hub) RejectQueuedAttempt(consumerID string, msg *pb.QueueMessage) {
	h.inFlightMu.Lock()
	record, exists := h.deliveryRecords[consumerID][string(msg.DeliveryTag)]
	if exists && record.message == msg && !record.sending {
		h.removeDeliveryRecordLocked(consumerID, string(msg.DeliveryTag))
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

// MarkAcknowledgedSent accepts a successful ACK once, retaining the delivery
// record until persistence completes (or the normal timeout/disconnect).
func (h *Hub) MarkAcknowledgedSent(consumerID string, key []byte) bool {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	record, exists := h.deliveryRecords[consumerID][string(key)]
	if !exists || !record.sending || record.ackPending {
		return false
	}
	record.ackPending = true
	h.deliveryRecords[consumerID][string(key)] = record
	return true
}

// RemoveAcknowledgedBatch releases only the recipients whose ACKs committed.
// Other recipients may still need the same payload.
func (h *Hub) RemoveAcknowledgedBatch(acks []raft.DeliveryAck) {
	completed := make(map[string]map[string]bool)
	for _, ack := range acks {
		if completed[ack.Recipient] == nil {
			completed[ack.Recipient] = make(map[string]bool)
		}
		completed[ack.Recipient][string(ack.Key)] = true
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	for id, keys := range h.inFlightByConsumer {
		consumer := h.byID[id]
		if consumer == nil {
			continue
		}
		done := completed[metadata.DeliveryRecipient(consumer.Group, id)]
		if len(done) == 0 {
			continue
		}
		kept := keys[:0]
		for _, key := range keys {
			if done[string(key)] {
				h.removeDeliveryRecordLocked(id, string(key))
			} else {
				kept = append(kept, key)
			}
		}
		h.inFlightByConsumer[id] = kept
	}
}

// AcknowledgeSent releases a sent delivery immediately for a NACK.
func (h *Hub) AcknowledgeSent(consumerID string, key []byte) bool {
	h.inFlightMu.Lock()
	defer h.inFlightMu.Unlock()
	record, exists := h.deliveryRecords[consumerID][string(key)]
	if !exists || !record.sending || record.ackPending {
		return false
	}
	h.removeDeliveryRecordLocked(consumerID, string(key))
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

// DeliveryViewCurrent checks authorization/version without rebuilding membership
// copies per message. DispatchPrepared still checks live subscriptions and fences.
func (h *Hub) DeliveryViewCurrent(topic string, epoch uint64) bool {
	h.deliveryMu.RLock()
	defer h.deliveryMu.RUnlock()
	if !h.permitted() {
		return false
	}
	if h.version != nil {
		current, active := h.version()
		return active && current == epoch
	}
	if h.deliveryView != nil {
		view := h.deliveryView(topic)
		return view.Active && view.Epoch == epoch
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.registryEpoch == epoch
}
