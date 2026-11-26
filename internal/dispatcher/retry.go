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

type retryIdentity [utils.EventKeySize + raft.AttemptTokenSize]byte

type retryCoordination struct {
	mu          sync.Mutex
	outcomes    map[retryIdentity]struct{}
	authorizing map[retryIdentity]struct{}
	refunds     map[retryIdentity]*deliverypb.RetryCommand
	expiring    map[retryIdentity]struct{}
	transports  map[retryIdentity]retryTransport
	draining    bool
	owner       []byte
	until       atomic.Pointer[time.Time]
}

type retryTransport struct {
	deadline time.Time
	timer    *time.Timer
}

// AuthorizeRetrySend holds capacity before reserving the attempt and protects
// it from expiry until preparation and the final transport check have passed.
func (l *DeliveryLedger) AuthorizeRetrySend(start *deliverypb.RetryCommand, ackWindow time.Duration, prepare func(uint64) (bool, error), checkTransport func() (time.Time, error)) (authorized bool, err error) {
	target := start.Attempts[0]
	id := identifyRetry(target)
	capacity := cap(l.retryOutcomes)
	if capacity == 0 {
		capacity = MaxPendingCompletions
	}
	l.retry.mu.Lock()
	if _, expiring := l.retry.expiring[id]; l.retry.draining || expiring || len(l.retry.authorizing)+len(l.retry.refunds) >= capacity {
		l.retry.mu.Unlock()
		return false, nil
	}
	if l.retry.authorizing == nil {
		l.retry.authorizing = make(map[retryIdentity]struct{})
	}
	l.retry.authorizing[id] = struct{}{}
	l.retry.mu.Unlock()
	needsRefund := true
	var transportDeadline time.Time
	defer func() {
		var refund *deliverypb.RetryCommand
		if needsRefund && !authorized {
			cancel := &deliverypb.RetryCommand{
				Operation: deliverypb.RetryCommand_CANCEL_RESERVATION,
				NowMs:     time.Now().UnixMilli(),
				Attempts: []*deliverypb.DeliveryAttemptTarget{{
					Key: target.Key, Recipient: target.Recipient, Token: target.Token,
				}},
			}
			// Hold the protection until this commits or is transferred to
			// the refund worker. Refunds bypass the consumer outcome queue.
			if _, err := l.ApplyRetry(cancel); err != nil {
				refund = cancel
			}
		}

		l.retry.mu.Lock()
		delete(l.retry.authorizing, id)
		if authorized {
			// The durable deadline is a recovery index set before the SENT
			// proposal. Keep the live owner's full, monotonic ACK window.
			if l.retry.transports == nil {
				l.retry.transports = make(map[retryIdentity]retryTransport)
			}
			l.retry.transports[id] = retryTransport{deadline: transportDeadline, timer: time.AfterFunc(time.Until(transportDeadline), func() {
				l.retry.mu.Lock()
				delete(l.retry.transports, id)
				l.retry.mu.Unlock()
			})}
		}
		if refund != nil {
			if l.retry.refunds == nil {
				l.retry.refunds = make(map[retryIdentity]*deliverypb.RetryCommand)
			}
			l.retry.refunds[id] = refund
		}
		l.retry.mu.Unlock()
	}()

	attempt, err := l.ApplyRetry(start)
	if err != nil {
		return false, err
	}
	if attempt == 0 {
		needsRefund = false
		return false, nil
	}
	prepared, err := prepare(attempt)
	if !prepared || err != nil {
		return false, err
	}
	now := time.Now()
	result, err := l.ApplyRetry(&deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_SENT, NowMs: now.UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: target.Key, Recipient: target.Recipient, Token: target.Token,
			DeadlineMs: max(now.UnixMilli()+1, now.Add(ackWindow).UnixMilli()),
		}},
	})
	if err != nil || result == 0 {
		return false, err
	}
	transportDeadline, err = checkTransport()
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	return err == nil && !transportDeadline.IsZero() && !l.retry.draining && l.retryOwnerActive(), err
}

func (l *DeliveryLedger) flushRetryRefunds(ctx context.Context) error {
	var refunds [32]*deliverypb.RetryCommand
	l.retry.mu.Lock()
	n := 0
	for _, command := range l.retry.refunds {
		refunds[n] = command
		n++
		if n == len(refunds) {
			break
		}
	}
	l.retry.mu.Unlock()
	for _, command := range refunds[:n] {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := l.ApplyRetry(command); err != nil {
			return err
		}
		l.retry.mu.Lock()
		delete(l.retry.refunds, identifyRetry(command.Attempts[0]))
		l.retry.mu.Unlock()
	}
	return nil
}

// RetrySendActive checks the committed receipt and the owner's local expiry
// arbitration before the sender exposes another fragment of the same attempt.
func (l *DeliveryLedger) RetrySendActive(key []byte, recipient string, token []byte) (bool, error) {
	state, err := raft.ReadDeliveryState(l.db, key)
	if err != nil || !state.HasActiveAttempt(recipient, token) {
		return false, err
	}
	var id retryIdentity
	copy(id[:utils.EventKeySize], key)
	copy(id[utils.EventKeySize:], token)
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	_, expiring := l.retry.expiring[id]
	return !expiring && l.retryOwnerActive(), nil
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
func (l *DeliveryLedger) RunRetryOwner(ctx context.Context, lease, maxClockSkew time.Duration, logger *zap.Logger) {
	ticker := time.NewTicker(lease / 5)
	defer ticker.Stop()
	for ctx.Err() == nil {
		now := time.Now()
		localUntil := now.Add(lease)
		until, err := l.applyRetry(ctx, &deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_RENEW_OWNER, Owner: l.retry.owner,
			NowMs: now.UnixMilli(), LeaseUntilMs: localUntil.Add(maxClockSkew).UnixMilli(),
		})
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("failed to renew delivery retry owner lease", zap.Error(err))
			}
		} else if until == 0 {
			l.retry.until.Store(nil)
		} else {
			l.retry.until.Store(&localUntil)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (l *DeliveryLedger) retryOwnerActive() bool {
	if len(l.retry.owner) == 0 {
		return true
	}
	until := l.retry.until.Load()
	return until != nil && time.Now().Before(*until)
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
	if _, expiring := l.retry.expiring[id]; l.retry.draining || expiring || !l.retryOwnerActive() {
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

// StopRetryOutcomeAdmission lets the workers drain the accepted outcomes.
func (l *DeliveryLedger) StopRetryOutcomeAdmission() {
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if !l.retry.draining {
		l.retry.draining = true
		if l.retryOutcomes != nil {
			close(l.retryOutcomes)
		}
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
				case outcome, ok := <-l.retryOutcomes:
					if !ok {
						return
					}
					l.commitRetryOutcome(ctx, outcome, logger)
				}
			}
		})
	}
	group.Wait()
}

func (l *DeliveryLedger) commitRetryOutcome(ctx context.Context, outcome retryOutcome, logger *zap.Logger) {
	for ctx.Err() == nil {
		if _, err := l.applyRetry(ctx, outcome.command); err != nil {
			logger.Warn("failed to persist delivery outcome", zap.Error(err))
		} else {
			outcome.onCommitted()
			l.retry.mu.Lock()
			id := identifyRetry(outcome.command.Attempts[0])
			delete(l.retry.outcomes, id)
			if transport, ok := l.retry.transports[id]; ok {
				transport.timer.Stop()
				delete(l.retry.transports, id)
			}
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
	return l.applyRetry(nil, command)
}

func (l *DeliveryLedger) applyRetry(ctx context.Context, command *deliverypb.RetryCommand) (uint64, error) {
	if len(l.retry.owner) != 0 && command.Operation != deliverypb.RetryCommand_RENEW_OWNER &&
		command.Operation != deliverypb.RetryCommand_DELETE_DEAD_LETTER {
		command.Owner = l.retry.owner
		switch command.Operation {
		case deliverypb.RetryCommand_START, deliverypb.RetryCommand_SENT:
			if !l.retryOwnerActive() {
				return 0, errors.New("delivery retry owner lease unavailable")
			}
		}
	}
	cmd, err := raft.MarshalRetryCommand(command)
	if err != nil {
		return 0, err
	}
	var value uint64
	switch {
	case ctx != nil && l.ProposeContext != nil:
		result, err := l.ProposeContext(ctx, cmd)
		if err != nil {
			return 0, err
		}
		value = result.Value
	case l.propose != nil:
		result, err := l.propose(cmd)
		if err != nil {
			return 0, err
		}
		value = result.Value
	default:
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
	interval = min(max(interval, 25*time.Millisecond), 250*time.Millisecond)
	var refunds sync.WaitGroup
	refunds.Go(func() { l.runRetryRefunds(ctx, interval, logger) })
	defer refunds.Wait()
	ticker := time.NewTicker(interval)
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

func (l *DeliveryLedger) runRetryRefunds(ctx context.Context, interval time.Duration, logger *zap.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.flushRetryRefunds(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("failed to refund unsent delivery attempts", zap.Error(err))
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
		_, authorizing := l.retry.authorizing[id]
		_, refunding := l.retry.refunds[id]
		_, expiringElsewhere := l.retry.expiring[id]
		transport := l.retry.transports[id]
		if !pending && !authorizing && !refunding && !expiringElsewhere && !time.Now().Before(transport.deadline) {
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
