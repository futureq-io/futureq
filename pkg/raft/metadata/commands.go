package metadata

import (
	"encoding/binary"
	"fmt"
)

// MetadataShardID is the reserved shard ID for the cluster metadata Raft group.
// This group is separate from any event data shard and is used exclusively for
// replicating cluster topology (leader info, membership, roles).
const MetadataShardID uint64 = 0

// CommandType identifies the metadata state machine operation.
type CommandType uint8

const (
	// UpdateTopologyCmd replaces the full topology snapshot for a shard.
	// Sent when a leader changes or membership changes in any shard.
	UpdateTopologyCmd CommandType = iota

	// RegisterNodeAddrCmd registers a node's client-facing gRPC address.
	// Each node proposes this once at startup with its own address.
	// The address registry is global (not per-shard) and survives
	// topology updates — publishTopology merges it into each shard's
	// GrpcAddrs map before proposing.
	RegisterNodeAddrCmd
)

// ShardTopology describes the current state of a single Raft shard.
//
// Nodes/NonVotings/Witnesses hold Raft addresses (from Dragonboat membership).
// GrpcAddrs holds each node's client-facing gRPC address — this is what SDKs
// use to connect to the leader.
type ShardTopology struct {
	ShardID  uint64
	LeaderID uint64
	// LeaderAddr is the leader's gRPC address (client-facing).
	LeaderAddr     string
	Term           uint64
	Epoch          uint64 // incremented on every topology change
	Nodes          map[uint64]string
	NonVotings     map[uint64]string
	Witnesses      map[uint64]string
	GrpcAddrs      map[uint64]string // nodeID → gRPC address
	ConfigChangeID uint64
}

// TopologySnapshot is the full cluster topology across all shards.
type TopologySnapshot struct {
	Shards map[uint64]*ShardTopology
	Epoch  uint64 // global epoch, incremented on any change
}

// MarshalUpdateTopologyCmd serialises a ShardTopology into a command payload.
//
// Wire format:
//
//	[0]      CommandType   (1 byte = 0)
//	[1..8]   ShardID       (uint64 big-endian)
//	[9..16]  LeaderID      (uint64 big-endian)
//	[17..24] Term          (uint64 big-endian)
//	[25..32] Epoch         (uint64 big-endian)
//	[33..40] ConfigChangeID (uint64 big-endian)
//	[41..42] LeaderAddrLen (uint16 big-endian)
//	[43..]   LeaderAddr    (variable — gRPC address)
//	[..+2]   NumNodes      (uint16 big-endian)
//	for each node:
//	  [n..n+7]   NodeID    (uint64 big-endian)
//	  [n+8..n+9] AddrLen   (uint16 big-endian)
//	  [n+10..]   Addr      (variable — Raft address)
//	[..+2]   NumNonVotings (uint16 big-endian)
//	for each non-voting: same as node
//	[..+2]   NumWitnesses  (uint16 big-endian)
//	for each witness: same as node
//	[..+2]   NumGrpcAddrs  (uint16 big-endian)
//	for each grpc addr: same as node (gRPC address)
func MarshalUpdateTopologyCmd(t *ShardTopology) ([]byte, error) {
	size := 1 + 8*5 + 2 + len(t.LeaderAddr)
	for _, m := range []map[uint64]string{t.Nodes, t.NonVotings, t.Witnesses, t.GrpcAddrs} {
		size += 2
		for _, addr := range m {
			size += 8 + 2 + len(addr)
		}
	}

	out := make([]byte, size)
	out[0] = byte(UpdateTopologyCmd)
	pos := 1

	binary.BigEndian.PutUint64(out[pos:], t.ShardID)
	pos += 8
	binary.BigEndian.PutUint64(out[pos:], t.LeaderID)
	pos += 8
	binary.BigEndian.PutUint64(out[pos:], t.Term)
	pos += 8
	binary.BigEndian.PutUint64(out[pos:], t.Epoch)
	pos += 8
	binary.BigEndian.PutUint64(out[pos:], t.ConfigChangeID)
	pos += 8

	binary.BigEndian.PutUint16(out[pos:], uint16(len(t.LeaderAddr)))
	pos += 2
	copy(out[pos:], t.LeaderAddr)
	pos += len(t.LeaderAddr)

	pos = marshalNodeMap(out, pos, t.Nodes)
	pos = marshalNodeMap(out, pos, t.NonVotings)
	pos = marshalNodeMap(out, pos, t.Witnesses)
	_ = marshalNodeMap(out, pos, t.GrpcAddrs)

	return out, nil
}

// UnmarshalUpdateTopologyCmd deserialises an UpdateTopologyCmd payload.
func UnmarshalUpdateTopologyCmd(data []byte) (*ShardTopology, error) {
	if len(data) < 1+8*5+2 {
		return nil, fmt.Errorf("metadata: UpdateTopologyCmd too short: %d bytes", len(data))
	}
	if CommandType(data[0]) != UpdateTopologyCmd {
		return nil, fmt.Errorf("metadata: expected UpdateTopologyCmd (0), got %d", data[0])
	}

	t := &ShardTopology{}
	pos := 1

	t.ShardID = binary.BigEndian.Uint64(data[pos:])
	pos += 8
	t.LeaderID = binary.BigEndian.Uint64(data[pos:])
	pos += 8
	t.Term = binary.BigEndian.Uint64(data[pos:])
	pos += 8
	t.Epoch = binary.BigEndian.Uint64(data[pos:])
	pos += 8
	t.ConfigChangeID = binary.BigEndian.Uint64(data[pos:])
	pos += 8

	addrLen := int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	if pos+addrLen > len(data) {
		return nil, fmt.Errorf("metadata: UpdateTopologyCmd truncated at LeaderAddr")
	}
	t.LeaderAddr = string(data[pos : pos+addrLen])
	pos += addrLen

	var err error
	if t.Nodes, pos, err = unmarshalNodeMap(data, pos); err != nil {
		return nil, fmt.Errorf("metadata: nodes: %w", err)
	}
	if t.NonVotings, pos, err = unmarshalNodeMap(data, pos); err != nil {
		return nil, fmt.Errorf("metadata: nonVotings: %w", err)
	}
	if t.Witnesses, pos, err = unmarshalNodeMap(data, pos); err != nil {
		return nil, fmt.Errorf("metadata: witnesses: %w", err)
	}
	if t.GrpcAddrs, _, err = unmarshalNodeMap(data, pos); err != nil {
		return nil, fmt.Errorf("metadata: grpcAddrs: %w", err)
	}

	return t, nil
}

func marshalNodeMap(out []byte, pos int, m map[uint64]string) int {
	binary.BigEndian.PutUint16(out[pos:], uint16(len(m)))
	pos += 2
	for id, addr := range m {
		binary.BigEndian.PutUint64(out[pos:], id)
		pos += 8
		binary.BigEndian.PutUint16(out[pos:], uint16(len(addr)))
		pos += 2
		copy(out[pos:], addr)
		pos += len(addr)
	}
	return pos
}

func unmarshalNodeMap(data []byte, pos int) (map[uint64]string, int, error) {
	if pos+2 > len(data) {
		return nil, 0, fmt.Errorf("truncated at count")
	}
	count := int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2

	m := make(map[uint64]string, count)
	for i := 0; i < count; i++ {
		if pos+8+2 > len(data) {
			return nil, 0, fmt.Errorf("truncated at entry %d", i)
		}
		id := binary.BigEndian.Uint64(data[pos:])
		pos += 8
		addrLen := int(binary.BigEndian.Uint16(data[pos:]))
		pos += 2
		if pos+addrLen > len(data) {
			return nil, 0, fmt.Errorf("truncated at entry %d addr", i)
		}
		m[id] = string(data[pos : pos+addrLen])
		pos += addrLen
	}
	return m, pos, nil
}

// MarshalRegisterNodeAddrCmd serialises a (nodeID, grpcAddr) registration.
//
// Wire format:
//
//	[0]      CommandType (1 byte = 1)
//	[1..8]   NodeID      (uint64 big-endian)
//	[9..10]  AddrLen     (uint16 big-endian)
//	[11..]   Addr        (variable — gRPC address)
func MarshalRegisterNodeAddrCmd(nodeID uint64, grpcAddr string) ([]byte, error) {
	out := make([]byte, 1+8+2+len(grpcAddr))
	out[0] = byte(RegisterNodeAddrCmd)
	binary.BigEndian.PutUint64(out[1:], nodeID)
	binary.BigEndian.PutUint16(out[9:], uint16(len(grpcAddr)))
	copy(out[11:], grpcAddr)
	return out, nil
}

// UnmarshalRegisterNodeAddrCmd deserialises a RegisterNodeAddrCmd payload.
func UnmarshalRegisterNodeAddrCmd(data []byte) (nodeID uint64, grpcAddr string, err error) {
	if len(data) < 1+8+2 {
		return 0, "", fmt.Errorf("metadata: RegisterNodeAddrCmd too short: %d bytes", len(data))
	}
	if CommandType(data[0]) != RegisterNodeAddrCmd {
		return 0, "", fmt.Errorf("metadata: expected RegisterNodeAddrCmd (1), got %d", data[0])
	}
	nodeID = binary.BigEndian.Uint64(data[1:])
	addrLen := int(binary.BigEndian.Uint16(data[9:]))
	if 11+addrLen > len(data) {
		return 0, "", fmt.Errorf("metadata: RegisterNodeAddrCmd truncated at addr")
	}
	return nodeID, string(data[11 : 11+addrLen]), nil
}
