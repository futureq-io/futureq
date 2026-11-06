// Package subscriptions manages durable interests independently of connections.
package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/google/uuid"
	"github.com/lni/dragonboat/v4/statemachine"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

var ErrNotFound = errors.New("subscription not found or session expired")

const SessionLease = 15 * time.Second
const heartbeatInterval = 5 * time.Second

type Service struct {
	DB      storage.DB
	Propose func(context.Context, []byte) (statemachine.Result, error)
	// ApplyLocal shares the delivery ledger's standalone mutation lock.
	ApplyLocal            func([]byte) (statemachine.Result, error)
	IsLeader              func() bool
	IdleTimeout           time.Duration
	Logger                *zap.Logger
	mu                    sync.Mutex
	sessions              map[string]*deliverypb.SubscriptionMutation
	localMu               sync.Mutex
	cursor, cleanupCursor []byte
}

func New(db storage.DB, timeout time.Duration, logger *zap.Logger) *Service {
	return &Service{DB: db, IdleTimeout: timeout, Logger: logger, sessions: make(map[string]*deliverypb.SubscriptionMutation)}
}
func Mutation(topic, group, subscriber string) *deliverypb.SubscriptionMutation {
	return &deliverypb.SubscriptionMutation{Topic: topic, GroupId: group, SubscriberId: subscriber}
}
func (s *Service) apply(ctx context.Context, command *deliverypb.SubscriptionCommand) (statemachine.Result, error) {
	data, err := raft.MarshalSubscriptionCommand(command)
	if err != nil {
		return statemachine.Result{}, err
	}
	if s.Propose != nil {
		return s.Propose(ctx, data)
	}
	if s.ApplyLocal != nil {
		return s.ApplyLocal(data)
	}
	s.localMu.Lock()
	defer s.localMu.Unlock()
	batch := s.DB.NewBatch()
	defer batch.Close() //nolint:errcheck
	overlay := raft.NewDeliveryBatch(s.DB, batch)
	result, _, err := overlay.ApplySubscription(data)
	if err != nil {
		return result, err
	}
	if err := overlay.Flush(); err != nil {
		return result, err
	}
	return result, batch.Commit(storage.Sync)
}
func (s *Service) command(ctx context.Context, operation uint32, mutations ...*deliverypb.SubscriptionMutation) (statemachine.Result, error) {
	return s.apply(ctx, &deliverypb.SubscriptionCommand{Operation: operation, NowMs: time.Now().UnixMilli(), LeaseMs: SessionLease.Milliseconds(), IdleTimeoutMs: s.IdleTimeout.Milliseconds(), Mutations: mutations})
}
func (s *Service) Create(ctx context.Context, topic, group, subscriber string) (*deliverypb.SubscriptionRecord, error) {
	mutation := Mutation(topic, group, subscriber)
	mutation.Recipient = "g:s:" + uuid.NewString()
	result, err := s.command(ctx, raft.SubscriptionCreate, mutation)
	if err != nil {
		return nil, err
	}
	record := new(deliverypb.SubscriptionRecord)
	if err := proto.Unmarshal(result.Data, record); err != nil {
		return nil, err
	}
	return record, nil
}
func (s *Service) Unsubscribe(ctx context.Context, topic, group, subscriber string) error {
	_, err := s.command(ctx, raft.SubscriptionRemove, Mutation(topic, group, subscriber))
	return err
}

// Connect resolves identity in the Raft apply result, never a potentially stale
// follower read. A missing interest is not automatically recreated by reconnect.
func (s *Service) Connect(ctx context.Context, topic, group, subscriber, session string) (*deliverypb.SubscriptionRecord, error) {
	mutation := Mutation(topic, group, subscriber)
	mutation.Session = session
	result, err := s.command(ctx, raft.SubscriptionConnect, mutation)
	if err != nil {
		return nil, err
	}
	if result.Value == 0 {
		return nil, ErrNotFound
	}
	record := new(deliverypb.SubscriptionRecord)
	if err := proto.Unmarshal(result.Data, record); err != nil {
		return nil, err
	}
	mutation.Recipient = record.Recipient
	s.mu.Lock()
	s.sessions[session] = mutation
	s.mu.Unlock()
	return record, nil
}
func (s *Service) Disconnect(ctx context.Context, session string) error {
	s.mu.Lock()
	mutation := s.sessions[session]
	delete(s.sessions, session)
	s.mu.Unlock()
	if mutation == nil {
		return nil
	}
	_, err := s.command(ctx, raft.SubscriptionDisconnect, mutation)
	return err
}
func (s *Service) Alive(recipient, session string) bool {
	record, err := raft.ReadSubscriptionRecipient(s.DB, recipient)
	return err == nil && record != nil && record.Active && record.Sessions[session] > time.Now().UnixMilli()
}
func (s *Service) renew(ctx context.Context) error {
	s.mu.Lock()
	mutations := make([]*deliverypb.SubscriptionMutation, 0, len(s.sessions))
	for _, mutation := range s.sessions {
		mutations = append(mutations, proto.Clone(mutation).(*deliverypb.SubscriptionMutation))
	}
	s.mu.Unlock()
	for len(mutations) > 0 {
		count := min(len(mutations), 256)
		if _, err := s.command(ctx, raft.SubscriptionRenew, mutations[:count]...); err != nil {
			return err
		}
		mutations = mutations[count:]
	}
	return nil
}
func nextPage(db storage.DB, prefix []byte, cursor *[]byte, visit func([]byte, []byte) error) error {
	lower := prefix
	if len(*cursor) > 0 {
		lower = *cursor
	}
	upper := append([]byte(nil), prefix...)
	upper[len(upper)-1]++
	iter, err := db.NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return err
	}
	defer iter.Close() //nolint:errcheck
	valid := iter.First()
	for count := 0; valid && count < 256; count++ {
		*cursor = append(append([]byte(nil), iter.Key()...), 0)
		if err := visit(iter.Key(), iter.Value()); err != nil {
			return err
		}
		valid = iter.Next()
	}
	if !valid {
		*cursor = nil
	}
	return iter.Error()
}

// Maintain only proposes conditional expirations and bounded receipt cleanup.
// Every proposal rechecks current state, so renew/unsubscribe/recreate races
// cannot expire a new generation or erase another subscription's delivery.
func (s *Service) Maintain(ctx context.Context, now time.Time) error {
	if s.IsLeader != nil && !s.IsLeader() {
		return nil
	}
	var mutations []*deliverypb.SubscriptionMutation
	err := nextPage(s.DB, raft.SubscriptionPrefix, &s.cursor, func(_, data []byte) error {
		record := new(deliverypb.SubscriptionRecord)
		if err := proto.Unmarshal(data, record); err != nil {
			return err
		}
		if !record.Active {
			return nil
		}
		needs := false
		for _, deadline := range record.Sessions {
			if deadline <= now.UnixMilli() {
				needs = true
			}
		}
		if len(record.Sessions) == 0 && s.IdleTimeout > 0 && now.UnixMilli()-record.DisconnectedSinceMs >= s.IdleTimeout.Milliseconds() {
			needs = true
		}
		if needs {
			mutation := Mutation(record.Topic, record.GroupId, record.SubscriberId)
			mutation.Recipient = record.Recipient
			mutations = append(mutations, mutation)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(mutations) > 0 {
		_, err = s.apply(ctx, &deliverypb.SubscriptionCommand{Operation: raft.SubscriptionExpire, NowMs: now.UnixMilli(), IdleTimeoutMs: s.IdleTimeout.Milliseconds(), Mutations: mutations})
		if err != nil {
			return err
		}
	}
	retired, err := raft.HasRetiredSubscriptions(s.DB)
	if err != nil || !retired {
		return err
	}
	var keys [][]byte
	prefix := []byte("metadata/delivery/")
	err = nextPage(s.DB, prefix, &s.cleanupCursor, func(key, data []byte) error {
		if len(key) != len(prefix)+24 {
			return nil
		}
		state, err := raft.UnmarshalDeliveryState(data)
		if err != nil {
			return err
		}
		for _, recipient := range state.Recipients {
			if !state.Needs(recipient) {
				continue
			}
			record, err := raft.ReadSubscriptionRecipient(s.DB, recipient)
			if err != nil {
				return err
			}
			if record != nil && !record.Active {
				keys = append(keys, append([]byte(nil), key[len(prefix):]...))
				break
			}
		}
		return nil
	})
	if err != nil || len(keys) == 0 {
		return err
	}
	_, err = s.apply(ctx, &deliverypb.SubscriptionCommand{Operation: raft.SubscriptionCleanup, NowMs: now.UnixMilli(), Keys: keys})
	return err
}
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastRenew := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if now.Sub(lastRenew) >= heartbeatInterval {
				if err := s.renew(callCtx); err != nil {
					s.Logger.Warn("subscription renewal failed", zap.Error(err))
				} else {
					lastRenew = now
				}
			}
			if err := s.Maintain(callCtx, now); err != nil {
				s.Logger.Warn("subscription maintenance failed", zap.Error(fmt.Errorf("maintain: %w", err)))
			}
			cancel()
		}
	}
}
