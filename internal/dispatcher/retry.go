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
	"github.com/futureq-io/stormoar/v4/statemachine"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type retryIdentity [utils.EventKeySize + raft.AttemptTokenSize]byte

type retryCoordination struct {
	mu          sync.Mutex
	outcomes    map[retryIdentity]struct{}
	manualSlots map[retryIdentity]func() bool
	autoACKs    map[retryIdentity]*RetryAutoACK
	authorizing map[retryIdentity]struct{}
	refunds     map[retryIdentity]*retryRefund
	expiring    map[retryIdentity]struct{}
	transports  map[[utils.EventKeySize]byte]map[retryIdentity]retryTransport
	recovery    uint64
	draining    bool
	owner       []byte
	until       atomic.Pointer[time.Time]
}

type retryTransport struct {
	recipient          string
	deadline           time.Time
	messageExpiresAtMs int64
	timer              *time.Timer
}

type retryRefund struct {
	command  *deliverypb.RetryCommand
	retryAt  time.Time
	inFlight bool
}

// AuthorizeManualRetrySend reserves a manual outcome slot before START so a
// timely ACK or NACK can be queued while the stream is live.
func (l *DeliveryLedger) AuthorizeManualRetrySend(streamCtx context.Context, start *deliverypb.RetryCommand, ackWindow time.Duration, prepare func(uint64) (bool, error), checkTransport func() (time.Time, error)) (bool, error) {
	id := identifyRetry(start.Attempts[0])
	l.retry.mu.Lock()
	_, reserved := l.retry.manualSlots[id]
	if streamCtx.Err() != nil || l.retry.draining || reserved || len(l.retryOutcomes)+len(l.retry.manualSlots) >= cap(l.retryOutcomes) {
		l.retry.mu.Unlock()
		return false, nil
	}
	if l.retry.manualSlots == nil {
		l.retry.manualSlots = make(map[retryIdentity]func() bool)
	}
	l.retry.manualSlots[id] = context.AfterFunc(streamCtx, func() { l.releaseManualRetryOutcome(id) })
	l.retry.mu.Unlock()

	authorized, err := l.AuthorizeRetrySend(streamCtx, start, ackWindow, prepare, checkTransport)
	if !authorized {
		l.releaseManualRetryOutcome(id)
	}
	return authorized, err
}

// AbandonManualRetryOutcome releases an unused slot after a transport failure.
func (l *DeliveryLedger) AbandonManualRetryOutcome(tag []byte) {
	l.releaseManualRetryOutcome(retryIdentity(tag))
}

func (l *DeliveryLedger) releaseManualRetryOutcome(id retryIdentity) {
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	l.releaseManualRetryOutcomeLocked(id)
}

func (l *DeliveryLedger) releaseManualRetryOutcomeLocked(id retryIdentity) {
	stop, reserved := l.retry.manualSlots[id]
	if !reserved {
		return
	}
	delete(l.retry.manualSlots, id)
	stop()
	if l.retry.draining && len(l.retry.manualSlots) == 0 {
		close(l.retryOutcomes)
	}
}

// AuthorizeRetrySend holds capacity before reserving the attempt and protects
// an authorized attempt from expiry until BeginRetryTransport.
func (l *DeliveryLedger) AuthorizeRetrySend(streamCtx context.Context, start *deliverypb.RetryCommand, ackWindow time.Duration, prepare func(uint64) (bool, error), checkTransport func() (time.Time, error)) (authorized bool, err error) {
	target := start.Attempts[0]
	id := identifyRetry(target)
	key := [utils.EventKeySize]byte(id[:utils.EventKeySize])
	capacity := cap(l.retryOutcomes)
	if capacity == 0 {
		capacity = MaxPendingCompletions
	}
	l.retry.mu.Lock()
	if _, expiring := l.retry.expiring[id]; streamCtx.Err() != nil || l.retry.draining || expiring || len(l.retry.authorizing)+len(l.retry.refunds) >= capacity {
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
	l.retry.transports[key][id] = retryTransport{recipient: target.Recipient}
	recovery := l.retry.recovery
	l.retry.mu.Unlock()
	needsRefund := true
	defer func() {
		if authorized {
			return
		}
		if needsRefund {
			l.refundUnsentRetry(target)
			return
		}
		l.retry.mu.Lock()
		l.removeRetryTransportLocked(id)
		l.retry.mu.Unlock()
	}()

	attempt, err := l.applyRetry(streamCtx, start)
	if err == nil && attempt == 0 {
		needsRefund = false
		return false, nil
	}
	if streamCtx.Err() != nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	prepared, err := prepare(attempt)
	if !prepared || err != nil || streamCtx.Err() != nil {
		return false, err
	}
	now := time.Now()
	result, err := l.applyRetryResult(streamCtx, &deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_SENT, NowMs: now.UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: target.Key, Recipient: target.Recipient, Token: target.Token,
			DeadlineMs: max(now.UnixMilli()+1, now.Add(ackWindow).UnixMilli()),
		}},
	})
	if streamCtx.Err() != nil {
		return false, nil
	}
	if err != nil || result.Value == 0 {
		return false, err
	}
	var expiresAtMs int64
	if len(result.Data) != 0 {
		if len(result.Data) != 8 {
			return false, errors.New("invalid message expiration in retry authorization")
		}
		expiresAtMs = int64(binary.BigEndian.Uint64(result.Data))
	}
	transportDeadline, err := checkTransport()
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	transports := l.retry.transports[key]
	if _, exists := transports[id]; !exists || err != nil || streamCtx.Err() != nil ||
		!time.Now().Before(transportDeadline) || l.retry.draining || !l.retryOwnerActive() {
		return false, err
	}
	if recovery != l.retry.recovery {
		state, err := raft.ReadDeliveryState(l.db, target.Key)
		if err != nil || !state.HasActiveAttempt(target.Recipient, target.Token) {
			return false, err
		}
	}
	if expiresAtMs != 0 && time.Now().UnixMilli() >= expiresAtMs {
		return false, nil
	}
	// The durable deadline is set before SENT. The local owner gives the
	// consumer its full monotonic ACK window starting at transport.
	transports[id] = retryTransport{
		recipient: target.Recipient, deadline: transportDeadline, messageExpiresAtMs: expiresAtMs,
		timer: time.AfterFunc(time.Until(transportDeadline), func() { l.onRetryTransportDeadline(id) }),
	}
	return true, nil
}

// BeginRetryTransport checks the delivery permit and deadlines immediately
// before the first frame, using the same lock that arbitrates expiry.
func (l *DeliveryLedger) BeginRetryTransport(tag []byte, canSend func() bool) bool {
	id := retryIdentity(tag)
	key := [utils.EventKeySize]byte(id[:utils.EventKeySize])
	l.retry.mu.Lock()
	transport, exists := l.retry.transports[key][id]
	_, held := l.retry.authorizing[id]
	_, expiring := l.retry.expiring[id]
	if !held || !exists || expiring {
		delete(l.retry.authorizing, id)
		l.retry.mu.Unlock()
		return false
	}
	permitted := canSend() && l.retryOwnerActive()
	now := time.Now()
	if permitted && now.Before(transport.deadline) &&
		(transport.messageExpiresAtMs == 0 || now.UnixMilli() < transport.messageExpiresAtMs) {
		delete(l.retry.authorizing, id)
		l.retry.mu.Unlock()
		return true
	}
	// Keep expiry out until the refund commits or is queued.
	l.retry.mu.Unlock()
	l.refundUnsentRetry(&deliverypb.DeliveryAttemptTarget{
		Key: tag[:utils.EventKeySize], Recipient: transport.recipient, Token: tag[utils.EventKeySize:],
	})
	return false
}

func (l *DeliveryLedger) refundUnsentRetry(target *deliverypb.DeliveryAttemptTarget) {
	id := identifyRetry(target)
	key := [utils.EventKeySize]byte(id[:utils.EventKeySize])
	cancel := &deliverypb.RetryCommand{
		Operation: deliverypb.RetryCommand_CANCEL_RESERVATION,
		NowMs:     time.Now().UnixMilli(),
		Attempts: []*deliverypb.DeliveryAttemptTarget{{
			Key: target.Key, Recipient: target.Recipient, Token: target.Token,
		}},
	}
	// The broker-scoped proposal survives stream cancellation. Keep expiry
	// blocked until the proposal commits or the refund worker takes over.
	_, err := l.ApplyRetry(cancel)
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	delete(l.retry.authorizing, id)
	if _, exists := l.retry.transports[key][id]; exists && err != nil {
		if l.retry.refunds == nil {
			l.retry.refunds = make(map[retryIdentity]*retryRefund)
		}
		l.retry.refunds[id] = &retryRefund{command: cancel}
		return
	}
	l.removeRetryTransportLocked(id)
}

func (l *DeliveryLedger) onRetryTransportDeadline(id retryIdentity) {
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if _, authorizing := l.retry.authorizing[id]; authorizing {
		return
	}
	if l.retry.draining {
		l.releaseManualRetryOutcomeLocked(id)
	}
	// Keep the identity available for committed deletion or expiry
	// while it still owns an outcome slot.
	if _, manual := l.retry.manualSlots[id]; !manual && l.retry.autoACKs[id] == nil {
		l.removeRetryTransportLocked(id)
	}
}

func (l *DeliveryLedger) removeRetryTransportLocked(id retryIdentity) {
	delete(l.retry.authorizing, id)
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
			l.releaseManualRetryOutcomeLocked(id)
			delete(l.retry.authorizing, id)
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
		l.releaseManualRetryOutcomeLocked(id)
		delete(l.retry.refunds, id)
	}
}

// ReconcileRetryAttempts removes local reservations for attempts absent from a
// restored snapshot. Raft calls it after installing the snapshot and before
// applying any subsequent entries.
func (l *DeliveryLedger) ReconcileRetryAttempts() error {
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	l.retry.recovery++
	for key, transports := range l.retry.transports {
		state, err := raft.ReadDeliveryState(l.db, key[:])
		if err != nil {
			return err
		}
		for id, transport := range transports {
			if _, authorizing := l.retry.authorizing[id]; authorizing && transport.deadline.IsZero() {
				// An in-progress proposal can commit after recovery. The
				// authorizer rechecks the receipt before entering transport.
				continue
			}
			token := id[utils.EventKeySize:]
			if state.HasActiveAttempt(transport.recipient, token) ||
				(l.retry.refunds[id] != nil && state.HasRetryAttempt(transport.recipient, token)) {
				continue
			}
			l.removeRetryTransportLocked(id)
			l.releaseManualRetryOutcomeLocked(id)
			l.releaseRetryAutoACKLocked(id)
			delete(l.retry.refunds, id)
		}
	}
	return nil
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
	transport := l.retry.transports[[utils.EventKeySize]byte(key)][id]
	if transport.messageExpiresAtMs != 0 && time.Now().UnixMilli() >= transport.messageExpiresAtMs {
		return false, nil
	}
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
	_, accepted := l.TryManualRetryOutcome(command, nil, onCommitted)
	return accepted
}

// TryManualRetryOutcome validates the stream's attempt and queues its outcome
// under the same lock used to claim attempts for expiry.
func (l *DeliveryLedger) TryManualRetryOutcome(command *deliverypb.RetryCommand, validate func() bool, onCommitted func()) (valid, accepted bool) {
	id := identifyRetry(command.Attempts[0])
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if validate != nil && !validate() {
		return false, false
	}
	_, reserved := l.retry.manualSlots[id]
	if _, expiring := l.retry.expiring[id]; (l.retry.draining && !reserved) || expiring || !l.retryOwnerActive() {
		return true, false
	}
	if _, pending := l.retry.outcomes[id]; pending {
		return true, true
	}
	if l.retry.autoACKs[id] != nil {
		return true, false
	}
	if !reserved && len(l.retry.manualSlots) != 0 &&
		len(l.retryOutcomes)+len(l.retry.manualSlots) >= cap(l.retryOutcomes) {
		return true, false
	}
	select {
	case l.retryOutcomes <- retryOutcome{command: command, onCommitted: onCommitted}:
		l.releaseManualRetryOutcomeLocked(id)
		l.trackRetryOutcomeLocked(id)
		return true, true
	default:
		return true, false
	}
}

// StopRetryOutcomeAdmission stops new reservations. Existing reservations can
// still enqueue outcomes while their attempts are active during the drain.
func (l *DeliveryLedger) StopRetryOutcomeAdmission() {
	l.retry.mu.Lock()
	defer l.retry.mu.Unlock()
	if !l.retry.draining {
		l.retry.draining = true
		if l.retryOutcomes != nil && len(l.retry.manualSlots) == 0 {
			close(l.retryOutcomes)
		}
		now := time.Now()
		for id := range l.retry.manualSlots {
			if _, authorizing := l.retry.authorizing[id]; authorizing {
				continue
			}
			key := [utils.EventKeySize]byte(id[:utils.EventKeySize])
			deadline := l.retry.transports[key][id].deadline
			// These timers fired before shutdown and left the slot for the
			// expiry worker, which is also stopping.
			if !deadline.IsZero() && !now.Before(deadline) {
				l.releaseManualRetryOutcomeLocked(id)
				l.removeRetryTransportLocked(id)
			}
		}
		if l.retryAutoACKOutcomes != nil && len(l.retry.autoACKs) == 0 {
			close(l.retryAutoACKOutcomes)
		}
	}
}

// RunRetryOutcomes gives manual and automatic outcomes dedicated persistence
// workers while keeping Raft and storage waits off the stream receiver.
func (l *DeliveryLedger) RunRetryOutcomes(ctx context.Context, workers int, logger *zap.Logger) {
	var group sync.WaitGroup
	for _, queue := range []chan retryOutcome{l.retryOutcomes, l.retryAutoACKOutcomes} {
		for range workers {
			group.Go(func() {
				for {
					select {
					case <-ctx.Done():
						return
					case outcome, ok := <-queue:
						if !ok {
							return
						}
						l.commitRetryOutcome(ctx, outcome, logger)
					}
				}
			})
		}
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
	result, err := l.applyRetryResult(ctx, command)
	return result.Value, err
}

func (l *DeliveryLedger) applyRetryResult(ctx context.Context, command *deliverypb.RetryCommand) (statemachine.Result, error) {
	var result statemachine.Result
	if len(l.retry.owner) != 0 && command.Operation != deliverypb.RetryCommand_RENEW_OWNER &&
		command.Operation != deliverypb.RetryCommand_DELETE_DEAD_LETTER {
		command.Owner = l.retry.owner
		switch command.Operation {
		case deliverypb.RetryCommand_START, deliverypb.RetryCommand_SENT:
			if !l.retryOwnerActive() {
				return result, errors.New("delivery retry owner lease unavailable")
			}
		}
	}
	cmd, err := raft.MarshalRetryCommand(command)
	if err != nil {
		return result, err
	}
	switch {
	case ctx != nil && l.ProposeContext != nil:
		result, err = l.ProposeContext(ctx, cmd)
	case l.propose != nil:
		result, err = l.propose(cmd)
	default:
		err = l.applyLocal(storage.Sync, func(batch *raft.DeliveryBatch) ([][]byte, error) {
			var deleted [][]byte
			var err error
			result, deleted, err = batch.ApplyRetry(cmd)
			return deleted, err
		})
	}
	if err != nil {
		return statemachine.Result{}, err
	}
	if result.Value > 0 {
		switch command.Operation {
		case deliverypb.RetryCommand_NACK:
			metrics.DeadLetteredTotal.WithLabelValues("nack").Add(float64(result.Value))
		case deliverypb.RetryCommand_EXPIRE:
			metrics.DeadLetteredTotal.WithLabelValues("timeout").Add(float64(result.Value))
		}
	}
	return result, nil
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
			l.releaseManualRetryOutcomeLocked(id)
			l.removeRetryTransportLocked(id)
		}
		delete(l.retry.expiring, id)
	}
	l.retry.mu.Unlock()
	return err
}
