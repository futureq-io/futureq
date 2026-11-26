package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"google.golang.org/protobuf/proto"
)

const (
	AttemptTokenSize    = 16
	retryExpiryPrefix   = "metadata/delivery-retry-expiry/"
	maxRetryExpiryBatch = 32
)

// Ready excludes recipients with a committed attempt awaiting an outcome.
func (s *DeliveryState) Ready(recipient string) bool {
	if !s.Needs(recipient) {
		return false
	}
	if len(s.attempts) == 0 {
		return true
	}
	i := uint32(sort.SearchStrings(s.Recipients, recipient))
	return len(s.attempts[i].GetToken()) == 0
}

func MarshalRetryCommand(cmd *deliverypb.RetryCommand) ([]byte, error) {
	if err := validateRetryCommand(cmd); err != nil {
		return nil, err
	}
	return proto.MarshalOptions{}.MarshalAppend([]byte{byte(RetryDeliveryCmd)}, cmd)
}

func validateRetryCommand(cmd *deliverypb.RetryCommand) error {
	op := cmd.GetOperation()
	if op == deliverypb.RetryCommand_DELETE_DEAD_LETTER {
		if len(cmd.GetAttempts()) != 0 {
			return fmt.Errorf("delete dead letter cannot contain delivery attempts")
		}
		return ValidateDeadLetterID(cmd.GetTopic(), cmd.GetDeadLetterId())
	}
	if op < deliverypb.RetryCommand_START || op > deliverypb.RetryCommand_EXPIRE || cmd.GetNowMs() <= 0 ||
		len(cmd.GetAttempts()) == 0 || len(cmd.GetAttempts()) > maxRetryExpiryBatch ||
		(op != deliverypb.RetryCommand_EXPIRE && len(cmd.GetAttempts()) != 1) ||
		cmd.GetTopic() != "" || len(cmd.GetDeadLetterId()) != 0 {
		return fmt.Errorf("invalid retry command")
	}
	for _, attempt := range cmd.GetAttempts() {
		if len(attempt.GetKey()) != utils.EventKeySize || !validRetryRecipient(attempt.GetRecipient()) ||
			len(attempt.GetToken()) != AttemptTokenSize {
			return fmt.Errorf("invalid retry target")
		}
		switch op {
		case deliverypb.RetryCommand_START:
			if attempt.GetDeadlineMs() <= cmd.GetNowMs() {
				return fmt.Errorf("attempt deadline must follow its start time")
			}
		case deliverypb.RetryCommand_EXPIRE:
			if attempt.GetDeadlineMs() <= 0 || attempt.GetDeadlineMs() > cmd.GetNowMs() {
				return fmt.Errorf("attempt has not expired")
			}
		default:
			if attempt.GetDeadlineMs() != 0 {
				return fmt.Errorf("unexpected attempt deadline")
			}
		}
	}
	return nil
}

func validRetryRecipient(recipient string) bool {
	return len(recipient) > 2 && len(recipient) <= 4096 && utf8.ValidString(recipient) &&
		(strings.HasPrefix(recipient, "g:") || strings.HasPrefix(recipient, "u:"))
}

func retryExpiryKey(target *deliverypb.DeliveryAttemptTarget) []byte {
	key := binary.BigEndian.AppendUint64([]byte(retryExpiryPrefix), uint64(target.DeadlineMs))
	key = append(key, target.Key...)
	key = append(key, target.Token...)
	return append(key, target.Recipient...)
}

// DueDeliveryAttempts scans only the due portion of the durable deadline index.
// The proposal rechecks each token against the receipt, including after failover.
func DueDeliveryAttempts(db storage.DB, nowMs int64) ([]*deliverypb.DeliveryAttemptTarget, error) {
	iter, err := db.NewIter(&storage.IterOptions{
		LowerBound: []byte(retryExpiryPrefix),
		UpperBound: binary.BigEndian.AppendUint64([]byte(retryExpiryPrefix), uint64(nowMs)+1),
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close() //nolint:errcheck
	var due []*deliverypb.DeliveryAttemptTarget
	for ok := iter.First(); ok; ok = iter.Next() {
		data := iter.Key()[len(retryExpiryPrefix):]
		const tokenStart = 8 + utils.EventKeySize
		const recipientStart = tokenStart + AttemptTokenSize
		if len(data) <= recipientStart || !validRetryRecipient(string(data[recipientStart:])) {
			return nil, fmt.Errorf("invalid retry deadline index")
		}
		due = append(due, &deliverypb.DeliveryAttemptTarget{
			DeadlineMs: int64(binary.BigEndian.Uint64(data)),
			Key:        bytes.Clone(data[8:tokenStart]),
			Token:      bytes.Clone(data[tokenStart:recipientStart]),
			Recipient:  string(data[recipientStart:]),
		})
		if len(due) == maxRetryExpiryBatch {
			break
		}
	}
	return due, iter.Error()
}

func (b *DeliveryBatch) ApplyRetry(data []byte) (statemachine.Result, [][]byte, error) {
	var result statemachine.Result
	var cmd deliverypb.RetryCommand
	if len(data) == 0 || CommandType(data[0]) != RetryDeliveryCmd {
		return result, nil, fmt.Errorf("invalid retry command tag")
	}
	if err := deliveryUnmarshal.Unmarshal(data[1:], &cmd); err != nil {
		return result, nil, err
	}
	if err := validateRetryCommand(&cmd); err != nil {
		return result, nil, err
	}
	if cmd.Operation == deliverypb.RetryCommand_DELETE_DEAD_LETTER {
		return result, nil, b.deleteDeadLetter(cmd.Topic, cmd.DeadLetterId)
	}
	var deleted [][]byte
	for _, target := range cmd.Attempts {
		value, removed, err := b.applyAttempt(cmd.Operation, cmd.NowMs, target)
		if err != nil {
			return result, nil, err
		}
		result.Value += value
		if removed {
			deleted = append(deleted, target.Key)
		}
	}
	return result, deleted, nil
}

func (b *DeliveryBatch) applyAttempt(op deliverypb.RetryCommand_Operation, nowMs int64, target *deliverypb.DeliveryAttemptTarget) (uint64, bool, error) {
	if op == deliverypb.RetryCommand_EXPIRE {
		// A TTL delete or subscription cleanup can leave an obsolete deadline.
		// Delete that exact index entry even when the receipt or token is gone.
		if err := b.batch.Delete(retryExpiryKey(target)); err != nil {
			return 0, false, err
		}
	}
	state, err := b.load(target.Key)
	if err != nil || !state.Needs(target.Recipient) {
		return 0, false, err
	}
	i := uint32(sort.SearchStrings(state.Recipients, target.Recipient))
	previous := state.attempts[i]
	if op == deliverypb.RetryCommand_START {
		return b.startAttempt(state, i, previous, nowMs, target)
	}
	if !bytes.Equal(previous.GetToken(), target.Token) ||
		(op == deliverypb.RetryCommand_EXPIRE && previous.GetDeadlineMs() != target.DeadlineMs) {
		return 0, false, nil
	}
	if op != deliverypb.RetryCommand_EXPIRE {
		deadline := &deliverypb.DeliveryAttemptTarget{
			Key: target.Key, Token: target.Token, Recipient: target.Recipient, DeadlineMs: previous.DeadlineMs,
		}
		if err := b.batch.Delete(retryExpiryKey(deadline)); err != nil {
			return 0, false, err
		}
	}
	if op == deliverypb.RetryCommand_ACK {
		return b.completeAttempt(state, target, 0)
	}
	message, err := b.retryMessage(target.Key, nowMs)
	if err != nil {
		return 0, false, err
	}
	if message == nil {
		return b.completeAttempt(state, target, 0)
	}
	if previous.Number <= uint64(message.GetMaxRetries()) {
		state.attempts[i] = &deliverypb.DeliveryAttempt{Number: previous.Number}
		b.save(target.Key, state)
		return 0, false, nil
	}
	reason := pb.DeadLetterReason_DEAD_LETTER_REASON_TIMEOUT
	if op == deliverypb.RetryCommand_NACK {
		reason = pb.DeadLetterReason_DEAD_LETTER_REASON_NACK
	}
	archived, err := b.archiveDeadLetter(message, target, previous.Number, nowMs, reason)
	if err != nil {
		return 0, false, err
	}
	return b.completeAttempt(state, target, archived)
}

func (b *DeliveryBatch) startAttempt(state *DeliveryState, i uint32, previous *deliverypb.DeliveryAttempt, nowMs int64, target *deliverypb.DeliveryAttemptTarget) (uint64, bool, error) {
	if len(previous.GetToken()) != 0 {
		return 0, false, nil
	}
	message, err := b.retryMessage(target.Key, nowMs)
	if err != nil || message == nil || message.MaxRetries == nil || previous.GetNumber() > uint64(message.GetMaxRetries()) {
		return 0, false, err
	}
	if strings.HasPrefix(target.Recipient, subscriptionRecipientPrefix) {
		subscription, err := b.subscriptionRead([]byte(subscriptionGenerationPrefix + target.Recipient))
		if err != nil || !subscription.GetActive() {
			return 0, false, err
		}
	}
	if err := b.batch.Set(retryExpiryKey(target), nil); err != nil {
		return 0, false, err
	}
	number := previous.GetNumber() + 1
	if state.attempts == nil {
		state.attempts = make(map[uint32]*deliverypb.DeliveryAttempt)
	}
	state.attempts[i] = &deliverypb.DeliveryAttempt{Number: number, Token: target.Token, DeadlineMs: target.DeadlineMs}
	b.save(target.Key, state)
	return number, false, nil
}

func (b *DeliveryBatch) retryMessage(key []byte, nowMs int64) (*deliverypb.StoredMessageMetadata, error) {
	data, closer, err := b.db.Get(key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close() //nolint:errcheck
	message := new(deliverypb.StoredMessageMetadata)
	if err := deliveryUnmarshal.Unmarshal(data, message); err != nil {
		return nil, err
	}
	if message.TtlMs > 0 && nowMs >= message.EnqueuedAtUnixMs && nowMs-message.EnqueuedAtUnixMs >= message.TtlMs {
		return nil, nil
	}
	return message, nil
}

func (b *DeliveryBatch) completeAttempt(state *DeliveryState, target *deliverypb.DeliveryAttemptTarget, value uint64) (uint64, bool, error) {
	state.acknowledge(target.Recipient)
	if state.complete() {
		return value, true, b.Delete(target.Key)
	}
	b.save(target.Key, state)
	return value, false, nil
}
