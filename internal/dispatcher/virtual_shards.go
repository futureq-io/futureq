package dispatcher

import (
	"slices"
	"sort"

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
		ordinal, total, replicas := int64(r.ordinal), int64(count), int64(r.replicas)
		return int(ordinal * total / replicas), int((ordinal + 1) * total / replicas)
	}
	// Give every replica one shard and split the rest by worker count. Rounded
	// cumulative boundaries meet exactly, with no overlaps or uncovered shards.
	extra, totalWorkers := int64(count-r.replicas), int64(r.totalWorkers)
	boundary := func(workers int) int { return int((extra*int64(workers) + totalWorkers/2) / totalWorkers) }
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
	if r.workers == 0 {
		return nil
	}
	committed := make(map[string]struct{}, r.workers)
	for _, member := range members[r.before : r.before+r.workers] {
		committed[member.ID] = struct{}{}
	}
	local := make([]*ConsumerEntry, 0, min(r.workers, len(consumers)))
	for _, consumer := range consumers {
		if _, ok := committed[consumer.ID]; ok {
			local = append(local, consumer)
		}
	}
	return local
}

type shardSlotRange struct {
	first, end int // half-open
}

type shardSlotRanges []shardSlotRange

// next returns the first owned slot at or after slot, or -1.
func (r shardSlotRanges) next(slot int) int {
	i := sort.Search(len(r), func(i int) bool { return r[i].end > slot })
	if i == len(r) {
		return -1
	}
	return max(slot, r[i].first)
}

// ownedShardSlots returns the union of local group ranges for the requested layouts.
func (v InterestView) ownedShardSlots(layouts int) shardSlotRanges {
	var owned shardSlotRanges
	allShards := len(v.universal) != 0 || (v.standaloneShardScans && len(v.groups) != 0)
	for count := 1; count <= utils.MaxVirtualShards; count *= 2 {
		if layouts&count == 0 {
			continue
		}
		if allShards {
			owned = append(owned, shardSlotRange{count, 2 * count})
			continue
		}
		for _, group := range v.groups {
			if len(group.local) == 0 {
				continue
			}
			first, end := group.replica.shardRange(count)
			if first < end {
				owned = append(owned, shardSlotRange{count + first, count + end})
			}
		}
	}
	slices.SortFunc(owned, func(a, b shardSlotRange) int { return a.first - b.first })
	merged := owned[:0]
	for _, r := range owned {
		last := len(merged) - 1
		if last >= 0 && r.first <= merged[last].end {
			merged[last].end = max(merged[last].end, r.end)
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}
