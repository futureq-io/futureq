package subscriptions

import (
	"context"
	"sync"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"go.uber.org/zap"
)

func (s *Service) runDisconnectPool(ctx context.Context) {
	var workers sync.WaitGroup
	for range max(1, s.DisconnectWorkers) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			s.runDisconnects(ctx)
		}()
	}
	workers.Wait()
}

// One worker retains at most one 256-session batch in addition to the bounded
// queue. It checks completion and retries without holding renewal's mutex.
// Shutdown can leave sessions for their existing lease to expire.
func (s *Service) runDisconnects(ctx context.Context) {
	for {
		var batch []*deliverypb.SubscriptionMutation
		select {
		case <-ctx.Done():
			return
		case mutation := <-s.disconnects:
			batch = append(batch, mutation)
		}
	collect:
		for len(batch) < 256 {
			select {
			case mutation := <-s.disconnects:
				batch = append(batch, mutation)
			default:
				break collect
			}
		}
		for ctx.Err() == nil {
			callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err := s.command(callCtx, raft.SubscriptionDisconnect, batch...)
			cancel()
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			s.Logger.Warn("subscription disconnect failed; retrying", zap.Error(err))
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
