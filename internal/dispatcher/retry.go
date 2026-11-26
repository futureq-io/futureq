package dispatcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const retryOwnerLeaseDuration = 15 * time.Second

type retryIdentity [utils.EventKeySize + raft.AttemptTokenSize]byte

type retryCoordination struct {
	mu       sync.Mutex
	outcomes map[retryIdentity]struct{}
	expiring map[retryIdentity]struct{}
	owner    []byte
	until    atomic.Int64
}

func identifyRetry(target *deliverypb.DeliveryAttemptTarget) retryIdentity {
	var id retryIdentity
	copy(id[:utils.EventKeySize], target.Key)
	copy(id[utils.EventKeySize:], target.Token)
	return id
}

// SetRetryOwner identifies this broker process before starting the retry workers.
func (l *DeliveryLedger) SetRetryOwner(nodeID uint64) {
	instance := uuid.New()
	l.retry.owner = append(binary.BigEndian.AppendUint64(nil, nodeID), instance[:]...)
}

// RunRetryOwner renews independently of both the outcome and expiry workers.
func (l *DeliveryLedger) RunRetryOwner(ctx context.Context, logger *zap.Logger) {
	ticker := time.NewTicker(retryOwnerLeaseDuration / 5)
	defer ticker.Stop()
	for ctx.Err() == nil {
		now := time.Now()
		until, err := l.ApplyRetry(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_RENEW_OWNER, Owner: l.retry.owner,
			NowMs: now.UnixMilli(), LeaseUntilMs: now.Add(retryOwnerLeaseDuration).UnixMilli(),
		})
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("failed to renew delivery retry owner lease", zap.Error(err))
			}
		} else {
			l.retry.until.Store(int64(until))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type retryOutcome struct {
	command     *deliverypb.RetryCommand
	onCommitted func()
}

// ConfigureRetryOutcomes initializes the bounded queue before serving consumers.
func (l *DeliveryLedger) ConfigureRetryOutcomes(capacity int) {
	l.retryOutcomes = make(chan retryOutcome, capacity)
}

func (l *DeliveryLedger) TryRetryOutcome(command *deliverypb.RetryCommand, onCommitted func()) bool {
	id := identifyRetry(command.Attempts[0])
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if _, expiring := l.retry.expiring[id]; expiring ||
		(len(l.retry.owner) != 0 && time.Now().UnixMilli() >= l.retry.until.Load()) {
		return false
	}
	if _, pending := l.retry.outcomes[id]; pending {
		return true
	}
	select {
	case l.retryOutcomes <- retryOutcome{command: command, onCommitted: onCommitted}:
		if l.retry.outcomes == nil {
			l.retry.outcomes = make(map[retryIdentity]struct{})
		}
		l.retry.outcomes[id] = struct{}{}
		return true
	default:
		return false
	}
}

// RunRetryOutcomes keeps Raft and storage waits off the stream receiver. More
// than one worker lets independent deliveries progress around a slow proposal.
func (l *DeliveryLedger) RunRetryOutcomes(ctx context.Context, workers int, logger *zap.Logger) {
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case outcome := <-l.retryOutcomes:
					l.commitRetryOutcome(ctx, outcome, logger)
				}
			}
		})
	}
	group.Wait()
}

func (l *DeliveryLedger) commitRetryOutcome(ctx context.Context, outcome retryOutcome, logger *zap.Logger) {
	for ctx.Err() == nil {
		if _, err := l.ApplyRetry(outcome.command); err != nil {
			logger.Warn("failed to persist delivery outcome", zap.Error(err))
		} else {
			outcome.onCommitted()
			l.retry.mu.Lock()
			delete(l.retry.outcomes, identifyRetry(outcome.command.Attempts[0]))
			l.retry.mu.Unlock()
			return
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// ApplyRetry uses the same mutation lock and receipt cache as standalone ACKs.
// Attempts and outcomes are synced before the caller uses the result.
func (l *DeliveryLedger) ApplyRetry(command *deliverypb.RetryCommand) (uint64, error) {
	if len(l.retry.owner) != 0 && command.Operation != deliverypb.RetryCommand_RENEW_OWNER &&
		command.Operation != deliverypb.RetryCommand_DELETE_DEAD_LETTER {
		command.Owner = l.retry.owner
		switch command.Operation {
		case deliverypb.RetryCommand_START, deliverypb.RetryCommand_SENT:
			if command.NowMs >= l.retry.until.Load() {
				return 0, errors.New("delivery retry owner lease unavailable")
			}
		}
	}
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
// that originally delivered them is gone. An active broker expires its own
// attempts. The leader also recovers attempts whose owner lease has lapsed.
func (l *DeliveryLedger) RunRetryExpiry(ctx context.Context, interval time.Duration, isLeader func() bool, logger *zap.Logger) {
	ticker := time.NewTicker(min(max(interval, 25*time.Millisecond), 250*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := l.expireDueRetryAttempts(ctx, now.UnixMilli(), isLeader); err != nil && ctx.Err() == nil {
				logger.Warn("failed to expire delivery attempts", zap.Error(err))
			}
		}
	}
}

func (l *DeliveryLedger) expireDueRetryAttempts(ctx context.Context, nowMs int64, isLeader func() bool) error {
	var cursor []byte
	ownerLive := make(map[string]bool)
	for ctx.Err() == nil {
		leader := isLeader == nil || isLeader()
		if !leader && len(l.retry.owner) == 0 {
			return nil
		}
		attempts, next, err := raft.ScanDueDeliveryAttempts(l.db, nowMs, cursor)
		if err != nil || len(attempts) == 0 {
			return err
		}
		if err := l.expireRetryPage(attempts, nowMs, leader, ownerLive); err != nil || len(next) == 0 {
			return err
		}
		cursor = next
	}
	return ctx.Err()
}

func (l *DeliveryLedger) expireRetryPage(attempts []*deliverypb.DeliveryAttemptTarget, nowMs int64, leader bool, ownerLive map[string]bool) error {
	eligible := attempts[:0]
	for _, target := range attempts {
		if bytes.Equal(l.retry.owner, target.Owner) {
			eligible = append(eligible, target)
		} else if leader {
			live, known := ownerLive[string(target.Owner)]
			if !known {
				var err error
				live, err = raft.RetryOwnerLive(l.db, target.Owner, nowMs)
				if err != nil {
					return err
				}
				ownerLive[string(target.Owner)] = live
			}
			if !live {
				eligible = append(eligible, target)
			}
		}
	}

	l.retry.mu.Lock()
	expiring := eligible[:0]
	for _, target := range eligible {
		id := identifyRetry(target)
		_, pending := l.retry.outcomes[id]
		_, expiringElsewhere := l.retry.expiring[id]
		if !pending && !expiringElsewhere {
			if l.retry.expiring == nil {
				l.retry.expiring = make(map[retryIdentity]struct{})
			}
			l.retry.expiring[id] = struct{}{}
			expiring = append(expiring, target)
		}
	}
	l.retry.mu.Unlock()
	if len(expiring) == 0 {
		return nil
	}
	_, err := l.ApplyRetry(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_EXPIRE, NowMs: nowMs, Attempts: expiring,
	})
	l.retry.mu.Lock()
	for _, target := range expiring {
		delete(l.retry.expiring, identifyRetry(target))
	}
	l.retry.mu.Unlock()
	return err
}
