package metadata

import "context"

// ConsumerHeartbeats returns an immutable liveness view for the serving set.
// Counters are replicated, but expiry is measured by each observer's monotonic
// clock after a quorum read; no replicated wall clock decides ownership.
func (s *MetadataStateMachine) ConsumerHeartbeats() (uint64, bool, map[uint64]uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	beats := make(map[uint64]uint64)
	if len(s.consumers.Groups) > 0 {
		for _, node := range s.consumers.Required {
			beats[node] = s.consumers.Heartbeats[node]
		}
	}
	return s.consumers.Epoch, s.consumers.Pending, beats
}

// HeartbeatConsumers requires a fresh event-shard read as well as a committed
// metadata write. A node isolated from its event shard must not appear able to
// serve merely because it can still contact the metadata group.
func (s *Service) HeartbeatConsumers(ctx context.Context, shardID, nodeID uint64) error {
	if _, err := s.nh.SyncRead(ctx, shardID, nil); err != nil {
		return err
	}
	cmd, err := marshalConsumerAck(0, nodeID)
	if err != nil {
		return err
	}
	cmd[0] = byte(ConsumerHeartbeatCmd)
	return s.propose(ctx, cmd)
}

// ProbeConsumers only freezes an unchanged active epoch. Concurrent or delayed
// probes cannot reset a pending drain and postpone fencing indefinitely.
func (s *Service) ProbeConsumers(ctx context.Context, epoch uint64) error {
	cmd, err := marshalConsumerAck(epoch, 0)
	if err != nil {
		return err
	}
	cmd[0] = byte(ConsumerProbeCmd)
	return s.propose(ctx, cmd)
}
