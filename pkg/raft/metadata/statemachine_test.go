package metadata

import (
	"bytes"
	"testing"

	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

type StateMachineSuite struct {
	suite.Suite
}

func TestStateMachineSuite(t *testing.T) {
	suite.Run(t, new(StateMachineSuite))
}

func newSM() *MetadataStateMachine {
	factory := NewMetadataStateMachineFactory(zap.NewNop())
	sm, _ := factory(1, 1).(*MetadataStateMachine)
	return sm
}

// ─── Update: UpdateTopologyCmd ───────────────────────────────────────────────

func (s *StateMachineSuite) TestUpdate_UpdateTopology_StoresShard() {
	require := s.Require()

	sm := newSM()

	topo := &ShardTopology{
		ShardID:  42,
		LeaderID: 3,
		Epoch:    10,
		Nodes:    map[uint64]string{1: "addr1"},
	}
	cmd, err := MarshalUpdateTopologyCmd(topo)
	require.NoError(err)

	result, err := sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(err)
	require.Equal(uint64(1), result.Value)
}

func (s *StateMachineSuite) TestUpdate_UpdateTopology_EmptyCmd_Skipped() {
	require := s.Require()

	sm := newSM()

	result, err := sm.Update(statemachine.Entry{Cmd: []byte{}})
	require.NoError(err)
	require.Equal(uint64(0), result.Value)
}

// ─── Update: RegisterNodeAddrCmd ─────────────────────────────────────────────

func (s *StateMachineSuite) TestUpdate_RegisterNodeAddr_StoresAddr() {
	require := s.Require()

	sm := newSM()

	cmd, err := MarshalRegisterNodeAddrCmd(5, "node5:9000")
	require.NoError(err)

	result, err := sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(err)
	require.Equal(uint64(1), result.Value)

	addrs := sm.GetGrpcAddrs()
	require.Equal("node5:9000", addrs[5])
}

// ─── Update: unknown command ──────────────────────────────────────────────────

func (s *StateMachineSuite) TestUpdate_UnknownCommand_ReturnsZero() {
	require := s.Require()

	sm := newSM()

	result, err := sm.Update(statemachine.Entry{Cmd: []byte{0xFF, 0x01, 0x02}})
	require.NoError(err)
	require.Equal(uint64(0), result.Value)
}

// ─── Lookup ──────────────────────────────────────────────────────────────────

func (s *StateMachineSuite) TestLookup_Nil_ReturnsTopologySnapshot() {
	require := s.Require()

	sm := newSM()

	// Seeds a shard.
	topo := &ShardTopology{ShardID: 99, Nodes: map[uint64]string{1: "a"}}
	cmd, _ := MarshalUpdateTopologyCmd(topo)
	_, err := sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(err)

	result, err := sm.Lookup(nil)
	require.NoError(err)

	snap, ok := result.(*TopologySnapshot)
	require.True(ok)
	require.Contains(snap.Shards, uint64(99))
}

func (s *StateMachineSuite) TestLookup_ByShardID_ReturnsShard() {
	require := s.Require()

	sm := newSM()

	topo := &ShardTopology{ShardID: 42, LeaderID: 7, Nodes: map[uint64]string{}}
	cmd, _ := MarshalUpdateTopologyCmd(topo)
	_, err := sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(err)

	result, err := sm.Lookup(uint64(42))
	require.NoError(err)

	shard, ok := result.(*ShardTopology)
	require.True(ok)
	require.Equal(uint64(42), shard.ShardID)
	require.Equal(uint64(7), shard.LeaderID)
}

func (s *StateMachineSuite) TestLookup_ByUnknownShard_ReturnsNil() {
	require := s.Require()

	sm := newSM()

	result, err := sm.Lookup(uint64(999))
	require.NoError(err)
	require.Nil(result)
}

func (s *StateMachineSuite) TestLookup_StringTopology_ReturnsSnapshot() {
	require := s.Require()

	sm := newSM()

	result, err := sm.Lookup("topology")
	require.NoError(err)

	snap, ok := result.(*TopologySnapshot)
	require.True(ok)
	require.NotNil(snap.Shards)
}

func (s *StateMachineSuite) TestLookup_UnknownString_ReturnsNil() {
	require := s.Require()

	sm := newSM()

	result, err := sm.Lookup("unknown-query")
	require.NoError(err)
	require.Nil(result)
}

// ─── GetTopology / GetShardTopology / GetGrpcAddrs ─────────────────────────────

func (s *StateMachineSuite) TestGetTopology_ReturnsCopy() {
	require := s.Require()

	sm := newSM()

	topo := &ShardTopology{ShardID: 7, Epoch: 5, Nodes: map[uint64]string{1: "a"}}
	cmd, _ := MarshalUpdateTopologyCmd(topo)
	_, err := sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(err)

	snap := sm.GetTopology()
	require.NotNil(snap)
	require.Contains(snap.Shards, uint64(7))

	// Mutating the returned snapshot should NOT affect the SM.
	delete(snap.Shards, 7)
	snap2 := sm.GetTopology()
	require.Contains(snap2.Shards, uint64(7))
}

func (s *StateMachineSuite) TestGetShardTopology_ReturnsCopy() {
	require := s.Require()

	sm := newSM()

	topo := &ShardTopology{ShardID: 7, LeaderID: 2, Nodes: map[uint64]string{1: "m"}}
	cmd, _ := MarshalUpdateTopologyCmd(topo)
	_, err := sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(err)

	shard := sm.GetShardTopology(7)
	require.NotNil(shard)
	require.Equal(uint64(2), shard.LeaderID)

	// Mutating the copy should not affect the SM.
	shard.LeaderID = 99
	shard2 := sm.GetShardTopology(7)
	require.Equal(uint64(2), shard2.LeaderID)
}

func (s *StateMachineSuite) TestGetGrpcAddrs_ReturnsCopy() {
	require := s.Require()

	sm := newSM()

	cmd, _ := MarshalRegisterNodeAddrCmd(1, "addr-1")
	_, err := sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(err)

	addrs := sm.GetGrpcAddrs()
	require.Equal("addr-1", addrs[1])

	// Mutating the map should not affect the SM.
	delete(addrs, 1)
	addrs2 := sm.GetGrpcAddrs()
	require.Contains(addrs2, uint64(1))
}

// ─── Snapshot round-trip ──────────────────────────────────────────────────────

func (s *StateMachineSuite) TestSnapshot_RoundTrip() {
	require := s.Require()

	sm1 := newSM()

	// Seed state.
	topo := &ShardTopology{
		ShardID:    42,
		LeaderID:   1,
		LeaderAddr: "host:9090",
		Epoch:      33,
		Nodes:      map[uint64]string{1: "raft-1", 2: "raft-2"},
		GrpcAddrs:  map[uint64]string{1: "grpc-1"},
	}
	topoCmd, _ := MarshalUpdateTopologyCmd(topo)
	_, err := sm1.Update(statemachine.Entry{Cmd: topoCmd})
	require.NoError(err)

	addrCmd, _ := MarshalRegisterNodeAddrCmd(1, "grpc-1")
	_, err = sm1.Update(statemachine.Entry{Cmd: addrCmd})
	require.NoError(err)

	// Save snapshot.
	var buf bytes.Buffer
	err = sm1.SaveSnapshot(&buf, nil, nil)
	require.NoError(err)

	// Recover into fresh SM.
	sm2 := newSM()
	err = sm2.RecoverFromSnapshot(&buf, nil, nil)
	require.NoError(err)

	snap := sm2.GetTopology()
	require.Contains(snap.Shards, uint64(42))
	shard := snap.Shards[42]
	require.Equal(uint64(1), shard.LeaderID)
	require.Equal("host:9090", shard.LeaderAddr)
	require.Equal(map[uint64]string{1: "raft-1", 2: "raft-2"}, shard.Nodes)

	addrs := sm2.GetGrpcAddrs()
	require.Equal("grpc-1", addrs[1])
}

// ─── Close ────────────────────────────────────────────────────────────────────

func (s *StateMachineSuite) TestClose_IsNoOp() {
	require := s.Require()

	sm := newSM()
	require.NoError(sm.Close())
}
