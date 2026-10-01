package metadata

import (
	"context"
	"sort"
)

// TopicDeliveryView is an immutable copy of one committed assignment. Member
// order is the same order used by ConsumerGroup and deliveryOrdinal ownership.
type TopicDeliveryView struct {
	Epoch uint64
	Active bool
	Groups map[string][]ConsumerMember
	Recipients []string
}

func (s *MetadataStateMachine) TopicDeliverySnapshot(topic string) TopicDeliveryView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	view := TopicDeliveryView{Epoch: s.consumers.Epoch, Active: !s.consumers.Pending, Groups: make(map[string][]ConsumerMember)}
	if !view.Active { return view }
	for group, members := range s.consumers.Groups[topic] {
		view.Groups[group] = append([]ConsumerMember(nil), members...)
		if group == "" {
			for _, member := range members { view.Recipients = append(view.Recipients, DeliveryRecipient("", member.ID)) }
		} else if len(members) > 0 { view.Recipients = append(view.Recipients, DeliveryRecipient(group, "")) }
	}
	sort.Strings(view.Recipients)
	return view
}

// ConsumerVersion is the local applied assignment version. It is safe to use
// for delivery only after a linearizable metadata read, within a bounded permit.
func (s *MetadataStateMachine) ConsumerVersion() (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.consumers.Epoch, !s.consumers.Pending
}

// ConsumerPresent includes members in a pending assignment. Returning replicas
// use this after a quorum read to restore live streams removed by a fence.
func (s *MetadataStateMachine) ConsumerPresent(topic, group, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, member := range s.consumers.Groups[topic][group] {
		if member.ID == id {
			return true
		}
	}
	return false
}

// DeliveryRecipients snapshots the independent interests at the first delivery
// of a message: one per named group, and one per universal subscription.
func (s *MetadataStateMachine) DeliveryRecipients(topic string) (uint64, []string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.consumers.Pending {
		return s.consumers.Epoch, nil, false
	}
	var recipients []string
	for group, members := range s.consumers.Groups[topic] {
		if group == "" {
			for _, member := range members {
				recipients = append(recipients, DeliveryRecipient(group, member.ID))
			}
		} else if len(members) > 0 {
			recipients = append(recipients, DeliveryRecipient(group, ""))
		}
	}
	sort.Strings(recipients)
	return s.consumers.Epoch, recipients, true
}

// DeliveryRecipient keeps group and universal interests in separate namespaces.
func DeliveryRecipient(group, id string) string {
	if group == "" {
		return "u:" + id
	}
	return "g:" + group
}

// FenceConsumers must be called only after the permit plus drain timeout has
// elapsed for this observed pending epoch. Returning nodes cannot serve without
// a fresh quorum read and registration in the activated assignment.
func (s *Service) FenceConsumers(ctx context.Context, epoch uint64) error {
	cmd, err := marshalConsumerAck(epoch, 0)
	if err != nil {
		return err
	}
	cmd[0] = byte(ConsumerFenceCmd)
	return s.propose(ctx, cmd)
}

// SyncConsumers catches the local metadata replica up before granting delivery
// permits or restoring subscriptions. A minority partition cannot pass it.
func (s *Service) SyncConsumers(ctx context.Context) error {
	_, err := s.nh.SyncRead(ctx, MetadataShardID, nil)
	return err
}
