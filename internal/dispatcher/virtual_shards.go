package dispatcher

import (
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
)

// replicaMembership describes the local replica's contiguous block in the
// metadata group's member list, which is sorted by replica ID.
type replicaMembership struct {
	replicas, ordinal int
	workers, before   int
	totalWorkers      int
	distribution      metadata.ShardDistribution
}

func describeReplica(members []metadata.ConsumerMember, nodeID uint64, distribution metadata.ShardDistribution) replicaMembership {
	r := replicaMembership{totalWorkers: len(members), distribution: distribution}
	for i, member := range members {
		if i == 0 || member.NodeID != members[i-1].NodeID {
			if member.NodeID == nodeID {
				r.ordinal, r.before = r.replicas, i
			}
			r.replicas++
		}
		if member.NodeID == nodeID {
			r.workers++
		}
	}
	return r
}

func (r replicaMembership) shardRange(count int) (first, end int) {
	if r.workers == 0 {
		return 0, 0
	}
	if r.replicas > count {
		// Use per-event ownership when some replicas would have no shard.
		return 0, count
	}
	if r.distribution == metadata.ShardDistributionReplicaCount {
		return r.ordinal * count / r.replicas, (r.ordinal + 1) * count / r.replicas
	}
	// Give every replica one shard and split the rest by worker count. Rounded
	// cumulative boundaries meet exactly, with no overlaps or uncovered shards.
	extra := count - r.replicas
	boundary := func(workers int) int { return (extra*workers + r.totalWorkers/2) / r.totalWorkers }
	return r.ordinal + boundary(r.before), r.ordinal + 1 + boundary(r.before+r.workers)
}

func (r replicaMembership) usesReplicaWorkers(count int) bool {
	return r.distribution == metadata.ShardDistributionReplicaCount || (count > 0 && r.replicas <= count)
}

func (r replicaMembership) owns(slot, count int, key []byte) bool {
	if r.workers == 0 {
		return false
	}
	if count < r.replicas {
		return deliveryOrdinal(key)%uint64(r.replicas) == uint64(r.ordinal)
	}
	first, end := r.shardRange(count)
	return count+first <= slot && slot < count+end
}

func (r replicaMembership) localWorkers(members []metadata.ConsumerMember, consumers []*ConsumerEntry) []*ConsumerEntry {
	var local []*ConsumerEntry
	for _, consumer := range consumers {
		for _, member := range members[r.before : r.before+r.workers] {
			if consumer.ID == member.ID {
				local = append(local, consumer)
				break
			}
		}
	}
	return local
}

// ownedShardSlots is the union of the ranges for the local groups. A universal
// subscriber needs every range. Each layout remains discoverable after resize.
func (v InterestView) ownedShardSlots() [2 * utils.MaxVirtualShards]bool {
	var owned [2 * utils.MaxVirtualShards]bool
	if len(v.universal) != 0 {
		for slot := 2; slot < len(owned); slot++ {
			owned[slot] = true
		}
		return owned
	}
	for _, group := range v.groups {
		if len(group.local) == 0 {
			continue
		}
		for count := 2; count <= utils.MaxVirtualShards; count *= 2 {
			first, end := group.replica.shardRange(count)
			for slot := count + first; slot < count+end; slot++ {
				owned[slot] = true
			}
		}
	}
	return owned
}
