package dispatcher

import pb "github.com/futureq-io/protocol/proto/go"

// topicAssignment is immutable after publication. Local registry changes
// discard it, and a metadata epoch or barrier change forces a rebuild.
type topicAssignment struct {
	barrier uint64
	view    InterestView
	groups  map[string]localGroup
}

// Both Hub read locks must be held. The topic mutex serializes cold builds
// without making the steady-state lookup contend with other sends.
func (h *Hub) topicAssignmentLocked(topic string, sub *TopicSubscription) *topicAssignment {
	if a := sub.assignment.Load(); a != nil && a.barrier == h.assignmentEpoch && a.view.Epoch == h.permitEpoch {
		return a
	}
	sub.assignmentMu.Lock()
	defer sub.assignmentMu.Unlock()
	if a := sub.assignment.Load(); a != nil && a.barrier == h.assignmentEpoch && a.view.Epoch == h.permitEpoch {
		return a
	}
	global := h.deliveryView(topic)
	if !global.Active || global.Epoch != h.permitEpoch {
		return nil
	}
	groups := sub.groupSnapshot()
	a := &topicAssignment{
		barrier: h.assignmentEpoch,
		view:    InterestView{Active: true, Epoch: global.Epoch, Recipients: global.Recipients, sharded: h.localReplica != 0, assignmentEpoch: h.assignmentEpoch},
		groups:  make(map[string]localGroup, len(groups)),
	}
	distribution := h.shardDistribution()
	for id, consumers := range groups {
		members := global.Groups[id]
		if len(members) == 0 {
			continue
		}
		group := localGroup{members: members, consumers: consumers}
		if a.view.sharded {
			group.replica = describeReplica(members, h.localReplica, distribution)
			group.local = group.replica.localWorkers(members, consumers)
		}
		a.groups[id] = group
		a.view.groups = append(a.view.groups, group)
	}
	for _, member := range global.Groups[""] {
		if consumer := sub.universal[member.ID]; consumer != nil {
			a.view.universal = append(a.view.universal, consumer)
		}
	}
	sub.assignment.Store(a)
	return a
}

func (h *Hub) assignedWorker(group localGroup, msg *pb.QueueMessage, key []byte, slot, count int) *ConsumerEntry {
	if group.replica.usesReplicaWorkers(count) {
		if !group.replica.owns(slot, count, key) {
			return nil
		}
		return h.strategy.Select(group.local, msg)
	}
	return moduloWorker(group.members, group.consumers, key)
}
