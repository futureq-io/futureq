package metadata

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/suite"
)

type CommandsSuite struct {
	suite.Suite
}

func TestCommandsSuite(t *testing.T) {
	suite.Run(t, new(CommandsSuite))
}

// ─── UpdateTopologyCmd round-trips ──────────────────────────────────────────

func (s *CommandsSuite) TestUpdateTopologyCmd_BasicRoundTrip() {
	require := s.Require()

	orig := &ShardTopology{
		ShardID:        7,
		LeaderID:       2,
		LeaderAddr:     "10.0.0.1:9000",
		Term:           42,
		Epoch:          100,
		ConfigChangeID: 555,
		Nodes:          map[uint64]string{1: "raft://1", 2: "raft://2"},
		NonVotings:     map[uint64]string{3: "raft://3"},
		Witnesses:      map[uint64]string{},
		GrpcAddrs:      map[uint64]string{1: "grpc://1", 2: "grpc://2"},
	}

	cmd, err := MarshalUpdateTopologyCmd(orig)
	require.NoError(err)
	require.Equal(byte(UpdateTopologyCmd), cmd[0])

	got, err := UnmarshalUpdateTopologyCmd(cmd)
	require.NoError(err)

	require.Equal(orig.ShardID, got.ShardID)
	require.Equal(orig.LeaderID, got.LeaderID)
	require.Equal(orig.LeaderAddr, got.LeaderAddr)
	require.Equal(orig.Term, got.Term)
	require.Equal(orig.Epoch, got.Epoch)
	require.Equal(orig.ConfigChangeID, got.ConfigChangeID)
	require.Equal(orig.Nodes, got.Nodes)
	require.Equal(orig.NonVotings, got.NonVotings)
	require.Equal(orig.Witnesses, got.Witnesses)
	require.Equal(orig.GrpcAddrs, got.GrpcAddrs)
}

func (s *CommandsSuite) TestUpdateTopologyCmd_EmptyMaps() {
	require := s.Require()

	orig := &ShardTopology{
		ShardID:    1,
		Nodes:      map[uint64]string{},
		NonVotings: map[uint64]string{},
		Witnesses:  map[uint64]string{},
		GrpcAddrs:  map[uint64]string{},
	}

	cmd, err := MarshalUpdateTopologyCmd(orig)
	require.NoError(err)

	got, err := UnmarshalUpdateTopologyCmd(cmd)
	require.NoError(err)
	require.Equal(orig.ShardID, got.ShardID)
	require.Empty(got.Nodes)
	require.Empty(got.NonVotings)
}

func (s *CommandsSuite) TestUpdateTopologyCmd_WrongType_Fails() {
	require := s.Require()

	// Build a RegisterNodeAddrCmd but try to parse as UpdateTopologyCmd.
	regCmd, _ := MarshalRegisterNodeAddrCmd(1, "addr")
	regCmd[0] = byte(RegisterNodeAddrCmd)

	_, err := UnmarshalUpdateTopologyCmd(regCmd)
	require.Error(err)
}

func (s *CommandsSuite) TestUpdateTopologyCmd_TooShort_Fails() {
	require := s.Require()

	_, err := UnmarshalUpdateTopologyCmd([]byte{0x00, 0x01})
	require.Error(err)
}

func (s *CommandsSuite) TestUpdateTopologyCmd_TruncatedLeaderAddr_Fails() {
	require := s.Require()

	orig := &ShardTopology{
		ShardID:    1,
		LeaderAddr: "a-very-long-hostname.example.com:9999",
		Nodes:      map[uint64]string{},
	}
	cmd, _ := MarshalUpdateTopologyCmd(orig)

	// Cut the buffer before the leaderAddr data ends.
	_, err := UnmarshalUpdateTopologyCmd(cmd[:40])
	require.Error(err)
}

// ─── RegisterNodeAddrCmd round-trips ───────────────────────────────────────

func (s *CommandsSuite) TestRegisterNodeAddrCmd_BasicRoundTrip() {
	require := s.Require()

	nodeID := uint64(9)
	addr := "192.168.1.10:8443"

	cmd, err := MarshalRegisterNodeAddrCmd(nodeID, addr)
	require.NoError(err)
	require.Equal(byte(RegisterNodeAddrCmd), cmd[0])

	gotID, gotAddr, err := UnmarshalRegisterNodeAddrCmd(cmd)
	require.NoError(err)
	require.Equal(nodeID, gotID)
	require.Equal(addr, gotAddr)
}

func (s *CommandsSuite) TestRegisterNodeAddrCmd_EmptyAddr() {
	require := s.Require()

	cmd, err := MarshalRegisterNodeAddrCmd(1, "")
	require.NoError(err)

	_, gotAddr, err := UnmarshalRegisterNodeAddrCmd(cmd)
	require.NoError(err)
	require.Equal("", gotAddr)
}

func (s *CommandsSuite) TestRegisterNodeAddrCmd_WrongType_Fails() {
	require := s.Require()

	// Topology command but try as RegisterNodeAddrCmd.
	topo := &ShardTopology{ShardID: 1, Nodes: map[uint64]string{}}
	topoCmd, _ := MarshalUpdateTopologyCmd(topo)

	_, _, err := UnmarshalRegisterNodeAddrCmd(topoCmd)
	require.Error(err)
}

func (s *CommandsSuite) TestRegisterNodeAddrCmd_TooShort_Fails() {
	require := s.Require()

	_, _, err := UnmarshalRegisterNodeAddrCmd([]byte{0x01, 0x00, 0x00})
	require.Error(err)
}

// ─── Metadata snapshot round-trips (via exported helpers) ──────────────────

func (s *CommandsSuite) TestWriteReadUint32_RoundTrip() {
	require := s.Require()

	values := []uint32{0, 1, 127, 256, 65535, 1 << 20, 0xFFFFFFFF}
	var buf bytes.Buffer
	for _, v := range values {
		require.NoError(writeUint32(&buf, v))
	}
	for _, expected := range values {
		got, err := readUint32(&buf)
		require.NoError(err)
		require.Equal(expected, got, fmt.Sprintf("expected %d, got %d", expected, got))
	}
}
