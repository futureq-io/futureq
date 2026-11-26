package raft

import (
	"bytes"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func newDeliveryTestSM(t *testing.T) (*EventStateMachine, storage.DB) {
	t.Helper()
	db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := repository.NewEventRepository(db, zap.NewNop(), time.Second)
	require.NoError(t, err)
	sm := NewEventStateMachineFactory(db, repo, nil, zap.NewNop())(1, 1).(*EventStateMachine)
	return sm, db
}

type DeliveryRetrySuite struct {
	suite.Suite
	sm *EventStateMachine
	db storage.DB
}

func TestDeliveryRetrySuite(t *testing.T) {
	suite.Run(t, new(DeliveryRetrySuite))
}

func (s *DeliveryRetrySuite) SetupTest() {
	s.sm, s.db = newDeliveryTestSM(s.T())
}

func (s *DeliveryRetrySuite) apply(commands ...[]byte) []statemachine.Entry {
	entries := make([]statemachine.Entry, len(commands))
	for i, command := range commands {
		entries[i] = statemachine.Entry{Index: s.sm.lastApplied + uint64(i) + 1, Cmd: command}
	}
	results, err := s.sm.Update(entries)
	s.Require().NoError(err)
	return results
}

func (s *DeliveryRetrySuite) store(message *storagepb.StoredMessage, recipients ...string) []byte {
	data, err := proto.Marshal(message)
	s.Require().NoError(err)
	id := s.sm.lastPersistedID + 1
	topic := utils.TopicHash(message.Topic)
	key := utils.EventKey(1, topic, id)
	receipt, err := MarshalPublishReceipt(0, recipients)
	s.Require().NoError(err)
	store, err := MarshalStoreDeliveryBatchCmd([]StoreBatchItem{{ID: id, Bucket: 1, TopicHash: topic, Msg: data, Receipt: receipt}})
	s.Require().NoError(err)
	s.apply(store)
	return key
}

func (s *DeliveryRetrySuite) retry(op deliverypb.RetryCommand_Operation, now int64, targets ...*deliverypb.DeliveryAttemptTarget) []byte {
	cmd, err := MarshalRetryCommand(&deliverypb.RetryCommand{Operation: op, NowMs: now, Attempts: targets})
	s.Require().NoError(err)
	return cmd
}

func (s *DeliveryRetrySuite) attempt(key []byte, recipient string, token byte, deadline int64) *deliverypb.DeliveryAttemptTarget {
	return &deliverypb.DeliveryAttemptTarget{
		Key: key, Recipient: recipient, Token: bytes.Repeat([]byte{token}, AttemptTokenSize), DeadlineMs: deadline,
	}
}

func (s *DeliveryRetrySuite) TestTimeoutAndStaleOutcomesAfterSnapshotRecovery() {
	key := s.store(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: 100, MaxRetries: new(uint32(1))}, "g:worker", "u:audit")
	first := s.attempt(key, "g:worker", 1, 110)
	rival := s.attempt(key, "g:worker", 2, 110)
	results := s.apply(s.retry(deliverypb.RetryCommand_START, 100, first), s.retry(deliverypb.RetryCommand_START, 100, rival))
	s.Equal(uint64(1), results[0].Result.Value)
	s.Zero(results[1].Result.Value)
	s.apply(s.retry(deliverypb.RetryCommand_SENT, 100, s.attempt(key, "g:worker", 1, 0)))
	snapshot, err := s.sm.PrepareSnapshot()
	s.Require().NoError(err)
	var encoded bytes.Buffer
	s.Require().NoError(s.sm.SaveSnapshot(snapshot, &encoded, nil))
	s.sm, s.db = newDeliveryTestSM(s.T())
	s.Require().NoError(s.sm.RecoverFromSnapshot(&encoded, nil))
	due, err := DueDeliveryAttempts(s.db, 109)
	s.Require().NoError(err)
	s.Empty(due)
	due, err = DueDeliveryAttempts(s.db, 110)
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	expire := s.retry(deliverypb.RetryCommand_EXPIRE, 110, due...)
	s.Zero(s.apply(expire)[0].Result.Value)

	second := s.attempt(key, "g:worker", 3, 120)
	s.Equal(uint64(2), s.apply(s.retry(deliverypb.RetryCommand_START, 111, second))[0].Result.Value)
	s.apply(s.retry(deliverypb.RetryCommand_SENT, 111, s.attempt(key, "g:worker", 3, 0)))
	stale := s.attempt(key, "g:worker", 1, 0)
	s.apply(s.retry(deliverypb.RetryCommand_ACK, 112, stale), expire)
	receipt, err := ReadDeliveryState(s.db, key)
	s.Require().NoError(err)
	s.Require().NotNil(receipt)
	s.True(receipt.Needs("g:worker"))
	s.False(receipt.Ready("g:worker"))
	s.True(receipt.Ready("u:audit"))

	due, err = DueDeliveryAttempts(s.db, 120)
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	expire = s.retry(deliverypb.RetryCommand_EXPIRE, 120, due...)
	results = s.apply(expire, expire)
	s.Equal(uint64(1), results[0].Result.Value)
	s.Zero(results[1].Result.Value)
	page, err := (DeadLetterQuery{Topic: "orders", Limit: 2}).Read(s.db)
	s.Require().NoError(err)
	s.Require().Len(page.Page.Entries, 1)
	s.Equal(pb.DeadLetterReason_DEAD_LETTER_REASON_TIMEOUT, page.Page.Entries[0].Reason)
	s.Equal(uint64(2), page.Page.Entries[0].Attempts)
	receipt, err = ReadDeliveryState(s.db, key)
	s.Require().NoError(err)
	s.False(receipt.Needs("g:worker"))
	s.True(receipt.Needs("u:audit"))
}

func (s *DeliveryRetrySuite) TestFanoutArchivesSharePayloadAndDeleteIndependently() {
	key := s.store(&storagepb.StoredMessage{
		Topic: "orders", Payload: []byte("original"), EnqueuedAtUnixMs: 100, DelayMs: 5, TtlMs: 50,
		MaxRetries: new(uint32(0)), Indexes: []*pb.Index{{Value: &pb.Index_Int64Value{Int64Value: 42}}},
	}, "g:worker", "u:audit")
	worker := s.attempt(key, "g:worker", 1, 120)
	audit := s.attempt(key, "u:audit", 2, 120)
	s.apply(s.retry(deliverypb.RetryCommand_START, 105, worker), s.retry(deliverypb.RetryCommand_START, 105, audit))
	worker.DeadlineMs, audit.DeadlineMs = 0, 0
	s.apply(s.retry(deliverypb.RetryCommand_SENT, 105, worker), s.retry(deliverypb.RetryCommand_SENT, 105, audit))
	results := s.apply(s.retry(deliverypb.RetryCommand_NACK, 110, worker), s.retry(deliverypb.RetryCommand_NACK, 111, audit))
	s.Equal(uint64(1), results[0].Result.Value)
	s.Equal(uint64(1), results[1].Result.Value)
	_, _, err := s.db.Get(key)
	s.ErrorIs(err, storage.ErrNotFound)

	query := DeadLetterQuery{Topic: "orders", Limit: 1}
	first, err := query.Read(s.db)
	s.Require().NoError(err)
	s.Require().Len(first.Page.Entries, 1)
	s.Require().NotEmpty(first.Page.NextPageToken)
	query.After = first.Page.NextPageToken
	next, err := query.Read(s.db)
	s.Require().NoError(err)
	s.Require().Len(next.Page.Entries, 1)
	s.Empty(next.Page.NextPageToken)
	for _, item := range []*pb.DeadLetterSummary{first.Page.Entries[0], next.Page.Entries[0]} {
		record, err := (DeadLetterQuery{Topic: "orders", ID: item.Id}).Read(s.db)
		s.Require().NoError(err)
		s.Require().NotNil(record.Entry)
		s.Equal([]byte("original"), record.Entry.Message.Payload)
		s.Equal(int64(42), record.Entry.Message.Indexes[0].GetInt64Value())
		s.Require().NotNil(record.Entry.Message.MaxRetries)
		s.Zero(*record.Entry.Message.MaxRetries)
		cmd, err := MarshalRetryCommand(&deliverypb.RetryCommand{
			Operation: deliverypb.RetryCommand_DELETE_DEAD_LETTER, Topic: "orders", DeadLetterId: item.Id,
		})
		s.Require().NoError(err)
		s.apply(cmd, cmd)
	}
	_, _, err = s.db.Get(append([]byte(deadLetterPayload), key...))
	s.ErrorIs(err, storage.ErrNotFound)
}

func (s *DeliveryRetrySuite) TestTTLAndUnlimitedMessagesDoNotArchive() {
	unlimited := s.store(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: 100}, "g:worker")
	expiring := s.store(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: 100, TtlMs: 5, MaxRetries: new(uint32(0))}, "g:worker")
	results := s.apply(
		s.retry(deliverypb.RetryCommand_START, 101, s.attempt(unlimited, "g:worker", 1, 110)),
		s.retry(deliverypb.RetryCommand_START, 101, s.attempt(expiring, "g:worker", 2, 110)),
	)
	s.Zero(results[0].Result.Value)
	s.Equal(uint64(1), results[1].Result.Value)
	s.apply(s.retry(deliverypb.RetryCommand_SENT, 102, s.attempt(expiring, "g:worker", 2, 0)))
	due, err := DueDeliveryAttempts(s.db, 110)
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	s.Zero(s.apply(s.retry(deliverypb.RetryCommand_EXPIRE, 110, due...))[0].Result.Value)
	page, err := (DeadLetterQuery{Topic: "orders", Limit: 2}).Read(s.db)
	s.Require().NoError(err)
	s.Empty(page.Page.Entries)
}

func (s *DeliveryRetrySuite) TestExpiredReservationPreservesTheFirstAttempt() {
	key := s.store(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: 100, MaxRetries: new(uint32(0))}, "g:worker")
	first := s.attempt(key, "g:worker", 1, 110)
	s.Require().Equal(uint64(1), s.apply(s.retry(deliverypb.RetryCommand_START, 100, first))[0].Result.Value)
	s.Zero(s.apply(s.retry(deliverypb.RetryCommand_EXPIRE, 110, first))[0].Result.Value)
	receipt, err := ReadDeliveryState(s.db, key)
	s.Require().NoError(err)
	s.True(receipt.Ready("g:worker"))
	second := s.attempt(key, "g:worker", 2, 120)
	s.Equal(uint64(1), s.apply(s.retry(deliverypb.RetryCommand_START, 111, second))[0].Result.Value)
	commands := s.apply(
		s.retry(deliverypb.RetryCommand_SENT, 101, s.attempt(key, "g:worker", 1, 0)),
		s.retry(deliverypb.RetryCommand_SENT, 111, s.attempt(key, "g:worker", 2, 0)),
	)
	s.Zero(commands[0].Result.Value, "a stalled authorization cannot expose an expired reservation")
	s.Equal(uint64(1), commands[1].Result.Value)
	s.Equal(uint64(1), s.apply(s.retry(deliverypb.RetryCommand_EXPIRE, 120, second))[0].Result.Value)
}

func (s *DeliveryRetrySuite) TestForeignExpiryWaitsForOwnerLease() {
	key := s.store(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: 100, MaxRetries: new(uint32(0))}, "g:worker")
	owner := bytes.Repeat([]byte{2}, RetryOwnerSize)
	foreign := bytes.Repeat([]byte{3}, RetryOwnerSize)
	marshal := func(cmd *deliverypb.RetryCommand) []byte {
		data, err := MarshalRetryCommand(cmd)
		s.Require().NoError(err)
		return data
	}
	target := s.attempt(key, "g:worker", 1, 110)
	authorize := s.attempt(key, "g:worker", 1, 0)
	results := s.apply(
		marshal(&deliverypb.RetryCommand{Operation: deliverypb.RetryCommand_RENEW_OWNER, Owner: owner, NowMs: 100, LeaseUntilMs: 150}),
		marshal(&deliverypb.RetryCommand{Operation: deliverypb.RetryCommand_START, Owner: owner, NowMs: 100, Attempts: []*deliverypb.DeliveryAttemptTarget{target}}),
		marshal(&deliverypb.RetryCommand{Operation: deliverypb.RetryCommand_SENT, Owner: owner, NowMs: 101, Attempts: []*deliverypb.DeliveryAttemptTarget{authorize}}),
	)
	s.Require().Equal(uint64(1), results[2].Result.Value)
	expire := &deliverypb.RetryCommand{Operation: deliverypb.RetryCommand_EXPIRE, Owner: foreign, NowMs: 110, Attempts: []*deliverypb.DeliveryAttemptTarget{target}}
	s.Zero(s.apply(marshal(expire))[0].Result.Value)
	due, err := DueDeliveryAttempts(s.db, 110)
	s.Require().NoError(err)
	s.Require().Len(due, 1)
	s.Equal(owner, due[0].GetOwner())
	expire.NowMs = 150
	s.Equal(uint64(1), s.apply(marshal(expire))[0].Result.Value)
}

func TestDeliveryReceiptsSurviveSnapshotAndAckBatch(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 3)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	prepare, err := MarshalPrepareDeliveryCmd(key, 1, []string{"g:a", "g:b", "u:first", "u:second"})
	require.NoError(t, err)
	ack, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:a"}, {Key: key, Recipient: "unknown"}})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}, {Index: 3, Cmd: ack}})
	require.NoError(t, err, "prepare sees a store from the same Update")
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.False(t, state.Needs("g:a"))
	require.True(t, state.Needs("g:b"))
	ctx, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(ctx, &snapshot, nil))
	recovered, recoveredDB := newDeliveryTestSM(t)
	require.NoError(t, recovered.RecoverFromSnapshot(&snapshot, nil))
	state, err = ReadDeliveryState(recoveredDB, key)
	require.NoError(t, err)
	require.False(t, state.Needs("g:a"))
	require.True(t, state.Needs("g:b"))
	ackB, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "g:b"}})
	require.NoError(t, err)
	ackU, err := MarshalAckDeliveryBatchCmd([]DeliveryAck{{Key: key, Recipient: "u:first"}, {Key: key, Recipient: "u:second"}, {Key: key, Recipient: "u:second"}})
	require.NoError(t, err)
	var deleted [][]byte
	recovered.OnDeleteKeys = func(keys [][]byte) { deleted = append(deleted, keys...) }
	_, err = recovered.Update([]statemachine.Entry{{Index: 4, Cmd: ackB}, {Index: 5, Cmd: ackU}})
	require.NoError(t, err, "ACK entries in one Update must share the pending state")
	require.Equal(t, [][]byte{key}, deleted)
	_, _, err = recoveredDB.Get(key)
	require.ErrorIs(t, err, storage.ErrNotFound)
	state, err = ReadDeliveryState(recoveredDB, key)
	require.NoError(t, err)
	require.Nil(t, state)
	_, err = recovered.Update([]statemachine.Entry{{Index: 6, Cmd: prepare}})
	require.NoError(t, err)
	state, err = ReadDeliveryState(recoveredDB, key)
	require.NoError(t, err)
	require.Nil(t, state, "a late snapshot scan cannot recreate receipts for a deleted payload")
}

func TestDeliveryManifestFreezesGroupsAndReleasesDisconnectedUniversal(t *testing.T) {
	sm, db := newDeliveryTestSM(t)
	key := utils.EventKey(1, 2, 3)
	store, err := MarshalStoreBatchCmd([]StoreBatchItem{{ID: 3, Bucket: 1, TopicHash: 2, Msg: []byte("payload")}})
	require.NoError(t, err)
	prepare, err := MarshalPrepareDeliveryCmd(key, 10, []string{"g:offline", "u:disconnected"})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 1, Cmd: store}, {Index: 2, Cmd: prepare}})
	require.NoError(t, err)
	newEpoch, err := MarshalPrepareDeliveryCmd(key, 11, []string{"g:new", "u:new"})
	require.NoError(t, err)
	staleEpoch, err := MarshalPrepareDeliveryCmd(key, 9, nil)
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 3, Cmd: newEpoch}, {Index: 4, Cmd: staleEpoch}})
	require.NoError(t, err)
	state, err := ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.True(t, state.Needs("g:offline"), "a disconnected named group retains its incomplete delivery")
	require.False(t, state.Needs("u:disconnected"))
	require.False(t, state.Needs("g:new"), "new interests do not join an already prepared delivery")
	require.Equal(t, uint64(11), state.Epoch)
	forceDelete, err := MarshalDeleteBatchCmd([][]byte{key})
	require.NoError(t, err)
	_, err = sm.Update([]statemachine.Entry{{Index: 5, Cmd: forceDelete}, {Index: 6, Cmd: prepare}})
	require.NoError(t, err)
	state, err = ReadDeliveryState(db, key)
	require.NoError(t, err)
	require.Nil(t, state, "explicit deletes/TTL override receipt retention atomically")
}
