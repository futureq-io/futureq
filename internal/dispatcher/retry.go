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
	autoACKs    map[retryIdentity]*RetryAutoACK
	authorizing map[retryIdentity]struct{}
	refunds     map[retryIdentity]*retryRefund
	expiring    map[retryIdentity]struct{}
	transports  map[[utils.EventKeySize]byte]map[retryIdentity]retryTransport
	draining    bool
	owner       []byte
	until       atomic.Pointer[time.Time]
}

type retryTransport struct {
	deadline time.Time
	timer    *time.Timer
}

type retryRefund struct {
	command  *deliverypb.RetryCommand
	retryAt  time.Time
	inFlight bool
}

// AuthorizeRetrySend holds capacity before reserving the attempt and protects
// it from expiry until preparation and the final transport check have passed.
func (l *DeliveryLedger) AuthorizeRetrySend(start *deliverypb.RetryCommand, ackWindow time.Duration, prepare func(uint64) (bool, error), checkTransport func() (time.Time, error)) (authorized bool, err error) {
	target := start.Attempts[0]
	id := identifyRetry(target)
	key := [utils.EventKeySize]byte(id[:utils.EventKeySize])
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
	if l.retry.transports == nil {
		l.retry.transports = make(map[[utils.EventKeySize]byte]map[retryIdentity]retryTransport)
	}
	if l.retry.transports[key] == nil {
		l.retry.transports[key] = make(map[retryIdentity]retryTransport)
	}
	// A committed deletion can invalidate this authorization before it
	// installs a timer.
	l.retry.transports[key][id] = retryTransport{}
	l.retry.mu.Unlock()
	needsRefund := true
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
		_, stillExists := l.retry.transports[key][id]
		// Receipt cleanup may have completed while the refund was in flight.
		if refund != nil && stillExists {
			if l.retry.refunds == nil {
				l.retry.refunds = make(map[retryIdentity]*retryRefund)
			}
			l.retry.refunds[id] = &retryRefund{command: refund}
		} else if !authorized {
			l.removeRetryTransportLocked(id)
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
	transportDeadline, err := checkTransport()
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	transports := l.retry.transports[key]
	if _, exists := transports[id]; !exists || err != nil || !time.Now().Before(transportDeadline) || l.retry.draining || !l.retryOwnerActive() {
		return false, err
	}
	// The durable deadline is set before SENT. The local owner gives the
	// consumer its full monotonic ACK window starting at transport.
	transports[id] = retryTransport{deadline: transportDeadline, timer: time.AfterFunc(time.Until(transportDeadline), func() {
		l.retry.mu.Lock()
		l.removeRetryTransportLocked(id)
		l.retry.mu.Unlock()
	})}
	return true, nil
}

func (l *DeliveryLedger) removeRetryTransportLocked(id retryIdentity) {
	key := [utils.EventKeySize]byte(id[:utils.EventKeySize])
	transports := l.retry.transports[key]
	if timer := transports[id].timer; timer != nil {
		timer.Stop()
	}
	delete(transports, id)
	if len(transports) == 0 {
		delete(l.retry.transports, key)
	}
}

// RemoveRetryTransports releases local ACK windows after a committed deletion.
func (l *DeliveryLedger) RemoveRetryTransports(keys [][]byte) {
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	for _, key := range keys {
		if len(key) != utils.EventKeySize {
			continue
		}
		eventKey := [utils.EventKeySize]byte(key)
		for id, transport := range l.retry.transports[eventKey] {
			if transport.timer != nil {
				transport.timer.Stop()
			}
			l.releaseRetryAutoACKLocked(id)
			delete(l.retry.refunds, id)
		}
		delete(l.retry.transports, eventKey)
	}
}

// CompleteRetryAttempts releases only the attempts removed by receipt cleanup.
func (l *DeliveryLedger) CompleteRetryAttempts(attempts []raft.CompletedAttempt) {
	if len(attempts) == 0 {
		return
	}
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	for _, attempt := range attempts {
		var id retryIdentity
		copy(id[:utils.EventKeySize], attempt.Key[:])
		copy(id[utils.EventKeySize:], attempt.Token[:])
		l.removeRetryTransportLocked(id)
		l.releaseRetryAutoACKLocked(id)
		delete(l.retry.refunds, id)
	}
}

func (l *DeliveryLedger) claimRetryRefund() (refund *retryRefund, drained bool) {
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	now := time.Now()
	for _, pending := range l.retry.refunds {
		if !pending.inFlight && !now.Before(pending.retryAt) {
			pending.inFlight = true
			return pending, false
		}
	}
	return nil, l.retry.draining && len(l.retry.authorizing) == 0 && len(l.retry.refunds) == 0
}

func (l *DeliveryLedger) applyRetryRefund(ctx context.Context, refund *retryRefund, retryDelay time.Duration) error {
	_, err := l.applyRetry(ctx, refund.command)
	id := identifyRetry(refund.command.Attempts[0])
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if l.retry.refunds[id] != refund {
		return nil
	}
	if err != nil {
		refund.inFlight = false
		refund.retryAt = time.Now().Add(retryDelay)
		return err
	}
	delete(l.retry.refunds, id)
	l.removeRetryTransportLocked(id)
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

// RetryAutoACK reserves an automatic outcome slot until transport reports its result.
type RetryAutoACK struct {
	ledger  *DeliveryLedger
	id      retryIdentity
	outcome retryOutcome
}

func (l *DeliveryLedger) TryReserveRetryAutoACK(command *deliverypb.RetryCommand, onCommitted func()) *RetryAutoACK {
	id := identifyRetry(command.Attempts[0])
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if l.retry.draining || !l.retryOwnerActive() || l.retry.autoACKs[id] != nil ||
		len(l.retryAutoACKOutcomes)+len(l.retry.autoACKs) >= cap(l.retryAutoACKOutcomes) {
		return nil
	}
	if l.retry.autoACKs == nil {
		l.retry.autoACKs = make(map[retryIdentity]*RetryAutoACK)
	}
	reservation := &RetryAutoACK{ledger: l, id: id, outcome: retryOutcome{command: command, onCommitted: onCommitted}}
	l.retry.autoACKs[id] = reservation
	return reservation
}

// Finish releases the slot or queues the ACK after a successful send. The
// reservation remains usable when the broker stops admitting new outcomes.
func (a *RetryAutoACK) Finish(sent bool) bool {
	l := a.ledger
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if l.retry.autoACKs[a.id] != a {
		return false
	}
	defer l.releaseRetryAutoACKLocked(a.id)
	if _, expiring := l.retry.expiring[a.id]; !sent || expiring || !l.retryOwnerActive() {
		return false
	}
	a.outcome.command.NowMs = time.Now().UnixMilli()
	// The automatic queue stays open during shutdown until all reservations
	// are released, and this send already owns one of its slots.
	l.retryAutoACKOutcomes <- a.outcome
	l.trackRetryOutcomeLocked(a.id)
	return true
}

func (l *DeliveryLedger) releaseRetryAutoACKLocked(id retryIdentity) {
	if _, exists := l.retry.autoACKs[id]; !exists {
		return
	}
	delete(l.retry.autoACKs, id)
	if l.retry.draining && len(l.retry.autoACKs) == 0 {
		close(l.retryAutoACKOutcomes)
	}
}

func (l *DeliveryLedger) trackRetryOutcomeLocked(id retryIdentity) {
	if l.retry.outcomes == nil {
		l.retry.outcomes = make(map[retryIdentity]struct{})
	}
	l.retry.outcomes[id] = struct{}{}
}

// ConfigureRetryOutcomes initializes the independently bounded outcome queues.
func (l *DeliveryLedger) ConfigureRetryOutcomes(capacity int) {
	l.retryOutcomes = make(chan retryOutcome, capacity)
	l.retryAutoACKOutcomes = make(chan retryOutcome, capacity)
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
	if l.retry.autoACKs[id] != nil {
		return false
	}
	select {
	case l.retryOutcomes <- retryOutcome{command: command, onCommitted: onCommitted}:
		l.trackRetryOutcomeLocked(id)
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
		if l.retryAutoACKOutcomes != nil && len(l.retry.autoACKs) == 0 {
			close(l.retryAutoACKOutcomes)
		}
	}
}

// RunRetryOutcomes keeps Raft and storage waits off the stream receiver. More
// than one worker lets independent deliveries progress around a slow proposal.
func (l *DeliveryLedger) RunRetryOutcomes(ctx context.Context, workers int, logger *zap.Logger) {
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			manual, automatic := l.retryOutcomes, l.retryAutoACKOutcomes
			for manual != nil || automatic != nil {
				var outcome retryOutcome
				var ok bool
				select {
				case <-ctx.Done():
					return
				case outcome, ok = <-manual:
					if !ok {
						manual = nil
						continue
					}
				case outcome, ok = <-automatic:
					if !ok {
						automatic = nil
						continue
					}
				}
				l.commitRetryOutcome(ctx, outcome, logger)
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
			l.removeRetryTransportLocked(id)
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

// RunRetryRefunds drains refunds independently of expiry. During shutdown it
// also waits for authorizations that may still hand off an unsent attempt.
func (l *DeliveryLedger) RunRetryRefunds(ctx context.Context, interval time.Duration, logger *zap.Logger) {
	interval = min(max(interval, 25*time.Millisecond), 250*time.Millisecond)
	const workers = 4
	var group sync.WaitGroup
	for range workers {
		group.Go(func() { l.runRetryRefundWorker(ctx, interval, logger) })
	}
	group.Wait()
}

func (l *DeliveryLedger) runRetryRefundWorker(ctx context.Context, interval time.Duration, logger *zap.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		refund, drained := l.claimRetryRefund()
		if drained {
			return
		}
		if refund != nil {
			if err := l.applyRetryRefund(ctx, refund, interval); err != nil && ctx.Err() == nil {
				logger.Warn("failed to refund unsent delivery attempt", zap.Error(err))
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
		transport := l.retry.transports[[utils.EventKeySize]byte(id[:utils.EventKeySize])][id]
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
		id := identifyRetry(target)
		if err == nil {
			l.releaseRetryAutoACKLocked(id)
		}
		delete(l.retry.expiring, id)
	}
	l.retry.mu.Unlock()
	return err
}
