package metadata

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

func (s *Service) requestTopology(shardID uint64) {
	s.mu.Lock()
	s.shards[shardID] = struct{}{}
	s.topologyPending[shardID] = struct{}{}
	s.mu.Unlock()
	select {
	case s.topologyWake <- struct{}{}:
	default:
	}
}

// Run publishes topology refreshes and consumer removals in independent workers.
// Call after SetNodeHost and startup registration; it stops on ctx cancellation.
func (s *Service) Run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		s.runConsumerRemovalPool(ctx, func(ctx context.Context, removal consumerRemoval) error {
			return s.ChangeConsumer(ctx, removal.shardID, removal.topic, removal.group, removal.member, false)
		})
	}()
	defer workers.Wait()
	s.runTopology(ctx, s.updateTopology)
}

func (s *Service) runTopology(ctx context.Context, update func(context.Context, uint64) error) {
	retry := time.NewTicker(time.Second)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.topologyWake:
		case <-retry.C:
		}
		s.mu.Lock()
		pending := s.topologyPending
		s.topologyPending = make(map[uint64]struct{})
		s.mu.Unlock()
		for shardID := range pending {
			if ctx.Err() != nil {
				return
			}
			if err := update(ctx, shardID); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.logger.Warn("topology refresh failed; will retry", zap.Uint64("shard_id", shardID), zap.Error(err))
				s.mu.Lock()
				s.topologyPending[shardID] = struct{}{}
				s.mu.Unlock()
				// Wait for the retry tick or a new event instead of busy-looping.
			}
		}
	}
}
