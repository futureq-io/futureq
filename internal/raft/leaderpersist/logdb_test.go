package leaderpersist

import (
	"errors"
	"testing"

	"github.com/lni/dragonboat/v4/raftio"
	pb "github.com/lni/dragonboat/v4/raftpb"
	"github.com/stretchr/testify/suite"
)

type LogDBSuite struct {
	suite.Suite
}

func TestLogDBSuite(t *testing.T) {
	suite.Run(t, new(LogDBSuite))
}

// stubLogDB satisfies raftio.ILogDB by embedding the interface; only the
// methods the decorator actually calls are overridden. Any unexpected call
// panics on a nil interface — which is what we want in tests.
type stubLogDB struct {
	raftio.ILogDB
	saveErr    error
	saveCalled bool
	savedCmds  [][]byte
}

func (s *stubLogDB) SaveRaftState(updates []pb.Update, shardID uint64) error {
	s.saveCalled = true
	for _, ud := range updates {
		for _, e := range ud.EntriesToSave {
			if len(e.Cmd) > 0 {
				s.savedCmds = append(s.savedCmds, e.Cmd)
			}
		}
	}
	return s.saveErr
}

func (s *LogDBSuite) TestSaveRaftStateNotifiesOnSuccess() {
	require := s.Require()
	tracker := NewTracker()
	stub := &stubLogDB{}
	db := &logDB{ILogDB: stub, tracker: tracker}

	payload := []byte("batch-payload-1")
	persistedCh, cancel := tracker.Register(tracker.Hash(payload))
	defer cancel()

	updates := []pb.Update{{
		EntriesToSave: []pb.Entry{{Cmd: payload}},
	}}

	require.NoError(db.SaveRaftState(updates, 1))
	require.True(stub.saveCalled)

	select {
	case <-persistedCh:
	default:
		require.Fail("expected tracker to be notified after successful save")
	}
}

func (s *LogDBSuite) TestSaveRaftStateDoesNotNotifyOnError() {
	require := s.Require()
	tracker := NewTracker()
	stub := &stubLogDB{saveErr: errors.New("disk full")}
	db := &logDB{ILogDB: stub, tracker: tracker}

	payload := []byte("batch-payload-2")
	persistedCh, cancel := tracker.Register(tracker.Hash(payload))
	defer cancel()

	updates := []pb.Update{{
		EntriesToSave: []pb.Entry{{Cmd: payload}},
	}}

	err := db.SaveRaftState(updates, 1)
	require.Error(err)

	select {
	case <-persistedCh:
		require.Fail("tracker must not be notified when the underlying save fails")
	default:
	}
}

func (s *LogDBSuite) TestSaveRaftStateSkipsEmptyCmds() {
	require := s.Require()
	tracker := NewTracker()
	stub := &stubLogDB{}
	db := &logDB{ILogDB: stub, tracker: tracker}

	// Register a waiter for some other payload; empty-Cmd entries must not
	// accidentally notify it.
	otherPayload := []byte("other")
	persistedCh, cancel := tracker.Register(tracker.Hash(otherPayload))
	defer cancel()

	updates := []pb.Update{{
		EntriesToSave: []pb.Entry{{Cmd: nil}, {Cmd: []byte{}}},
	}}

	require.NoError(db.SaveRaftState(updates, 1))

	select {
	case <-persistedCh:
		require.Fail("empty Cmd entries must not trigger notifications")
	default:
	}
}

func (s *LogDBSuite) TestSaveRaftStateNotifiesAllPayloadsInBatch() {
	require := s.Require()
	tracker := NewTracker()
	stub := &stubLogDB{}
	db := &logDB{ILogDB: stub, tracker: tracker}

	payloads := [][]byte{[]byte("p1"), []byte("p2"), []byte("p3")}
	chans := make([]<-chan struct{}, len(payloads))
	cancels := make([]func(), len(payloads))
	for i, p := range payloads {
		ch, cancel := tracker.Register(tracker.Hash(p))
		chans[i] = ch
		cancels[i] = cancel
		defer cancel()
	}

	updates := []pb.Update{{
		EntriesToSave: []pb.Entry{
			{Cmd: payloads[0]},
			{Cmd: payloads[1]},
			{Cmd: payloads[2]},
		},
	}}

	require.NoError(db.SaveRaftState(updates, 1))

	for i, ch := range chans {
		select {
		case <-ch:
		default:
			require.Failf("payload not notified", "index=%d", i)
		}
	}
}

func (s *LogDBSuite) TestSaveRaftStateMultipleShards() {
	require := s.Require()
	tracker := NewTracker()
	stub := &stubLogDB{}
	db := &logDB{ILogDB: stub, tracker: tracker}

	payloadA := []byte("shard-a-payload")
	payloadB := []byte("shard-b-payload")
	chA, cancelA := tracker.Register(tracker.Hash(payloadA))
	defer cancelA()
	chB, cancelB := tracker.Register(tracker.Hash(payloadB))
	defer cancelB()

	updates := []pb.Update{
		{ShardID: 1, EntriesToSave: []pb.Entry{{Cmd: payloadA}}},
		{ShardID: 2, EntriesToSave: []pb.Entry{{Cmd: payloadB}}},
	}

	require.NoError(db.SaveRaftState(updates, 0))

	select {
	case <-chA:
	default:
		require.Fail("shard A payload not notified")
	}
	select {
	case <-chB:
	default:
		require.Fail("shard B payload not notified")
	}
}
