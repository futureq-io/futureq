package metadata

import "context"

// ShardDistribution controls how a named group divides delivery among replicas.
type ShardDistribution string

const (
	ShardDistributionConsumerCount ShardDistribution = "consumerCount"
	ShardDistributionReplicaCount  ShardDistribution = "replicaCount"
	setShardDistribution                             = "shard_distribution"
)

func (d ShardDistribution) Valid() bool {
	return d == ShardDistributionConsumerCount || d == ShardDistributionReplicaCount
}

func (c *ConsumerState) shardDistribution() ShardDistribution {
	if c.ShardDistribution == ShardDistributionReplicaCount {
		return ShardDistributionReplicaCount
	}
	return ShardDistributionConsumerCount
}

// ShardDistribution returns the replicated policy. The delivery permit fences
// use of the policy while its consumer assignment is pending.
func (s *MetadataStateMachine) ShardDistribution() ShardDistribution {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.consumers.shardDistribution()
}

// ReconcileShardDistribution lets the metadata leader propose its configured
// policy. The epoch check in apply rejects a proposal that races a rebalance.
func (s *Service) ReconcileShardDistribution(ctx context.Context, eventShard, nodeID, epoch uint64, policy ShardDistribution) error {
	leader, term, valid, err := s.nh.GetLeaderID(MetadataShardID)
	if err != nil || !valid || leader != nodeID {
		return err
	}
	membership, err := s.nh.SyncGetShardMembership(ctx, eventShard)
	if err != nil {
		return err
	}
	required := make([]uint64, 0, len(membership.Nodes)+len(membership.NonVotings))
	for id := range membership.Nodes {
		required = append(required, id)
	}
	for id := range membership.NonVotings {
		required = append(required, id)
	}
	leader, currentTerm, valid, err := s.nh.GetLeaderID(MetadataShardID)
	if err != nil || !valid || leader != nodeID || currentTerm != term {
		return err
	}
	cmd, err := marshalConsumerChange(consumerChange{Operation: setShardDistribution, ShardDistribution: policy, ExpectedEpoch: epoch, Required: required})
	if err != nil {
		return err
	}
	return s.propose(ctx, cmd)
}
