package dispatcher

import (
	"context"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"go.uber.org/zap"
)

// ApplyRetry uses the same mutation lock and receipt cache as standalone ACKs.
// Attempts and outcomes are synced before the caller uses the result.
func (l *DeliveryLedger) ApplyRetry(command *deliverypb.RetryCommand) (uint64, error) {
	cmd, err := raft.MarshalRetryCommand(command)
	if err != nil {
		return 0, err
	}
	var value uint64
	if l.propose != nil {
		result, err := l.propose(cmd)
		if err != nil {
			return 0, err
		}
		value = result.Value
	} else {
		err := l.applyLocal(storage.Sync, func(batch *raft.DeliveryBatch) ([][]byte, error) {
			result, deleted, err := batch.ApplyRetry(cmd)
			value = result.Value
			return deleted, err
		})
		if err != nil {
			return 0, err
		}
	}
	if value > 0 {
		switch command.Operation {
		case deliverypb.RetryCommand_NACK:
			metrics.DeadLetteredTotal.WithLabelValues("nack").Add(float64(value))
		case deliverypb.RetryCommand_EXPIRE:
			metrics.DeadLetteredTotal.WithLabelValues("timeout").Add(float64(value))
		}
	}
	return value, nil
}

// RunRetryExpiry resolves unacknowledged attempts even if the worker or broker
// that originally delivered them is gone. In a cluster only the leader scans.
func (l *DeliveryLedger) RunRetryExpiry(ctx context.Context, interval time.Duration, isLeader func() bool, logger *zap.Logger) {
	ticker := time.NewTicker(min(max(interval, 25*time.Millisecond), 250*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if isLeader != nil && !isLeader() {
				continue
			}
			nowMs := now.UnixMilli()
			attempts, err := raft.DueDeliveryAttempts(l.db, nowMs)
			if err == nil && len(attempts) > 0 {
				_, err = l.ApplyRetry(&deliverypb.RetryCommand{
					Operation: deliverypb.RetryCommand_EXPIRE, NowMs: nowMs, Attempts: attempts,
				})
			}
			if err != nil && ctx.Err() == nil {
				logger.Warn("failed to expire delivery attempts", zap.Error(err))
			}
		}
	}
}
