package metadata

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

const consumerRemovalQueueSize = 1024

type consumerRemoval struct {
	shardID      uint64
	topic, group string
	member       ConsumerMember
}

// RemoveConsumerAsync queues removal after the caller has unregistered the
// local stream under its registry lock. Stream IDs must be unique and never
// reused: delayed removal then cannot remove a newly connected consumer.
// A full queue or shutdown leaves recovery to periodic node reconciliation.
func (s *Service) RemoveConsumerAsync(shardID uint64, topic, group string, member ConsumerMember) {
	select {
	case s.consumerRemovals <- consumerRemoval{shardID: shardID, topic: topic, group: group, member: member}:
	default:
		s.logger.Warn("consumer removal queue full; reconciliation will retry", zap.String("consumer_id", member.ID))
	}
}

// Independent stream IDs allow parallel removals. Raft serializes application;
// retries never add members or change the durable subscription generation.
func (s *Service) runConsumerRemovalPool(ctx context.Context, remove func(context.Context, consumerRemoval) error) {
	var workers sync.WaitGroup
	for range max(1, s.ConsumerRemovalWorkers) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			s.runConsumerRemovals(ctx, remove)
		}()
	}
	workers.Wait()
}

// Each worker retains one retrying command and
// checks every result. It never holds the local consumer registry lock while
// waiting for membership reads or proposals. Rebalance fencing is unchanged.
func (s *Service) runConsumerRemovals(ctx context.Context, remove func(context.Context, consumerRemoval) error) {
	for {
		var removal consumerRemoval
		select {
		case <-ctx.Done():
			return
		case removal = <-s.consumerRemovals:
		}
		for ctx.Err() == nil {
			callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := remove(callCtx, removal)
			cancel()
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("consumer removal failed; retrying", zap.String("consumer_id", removal.member.ID), zap.Error(err))
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
