package metadata

import (
	"io"
	"sync"

	"github.com/lni/dragonboat/v4/statemachine"
	"go.uber.org/zap"
)

// MetadataStateMachine implements statemachine.IStateMachine (in-memory).
// It stores the cluster topology — per-shard leader info, membership, and roles.
// State is fully transient: rebuilt from the Raft log on restart.
type MetadataStateMachine struct {
	mu       sync.RWMutex
	topology *TopologySnapshot
	logger   *zap.Logger
}

// NewMetadataStateMachineFactory returns the factory function that Dragonboat
// passes (clusterID, nodeID) to when it instantiates a new replica.
func NewMetadataStateMachineFactory(logger *zap.Logger) func(uint64, uint64) statemachine.IStateMachine {
	return func(clusterID, nodeID uint64) statemachine.IStateMachine {
		return &MetadataStateMachine{
			topology: &TopologySnapshot{
				Shards: make(map[uint64]*ShardTopology),
			},
			logger: logger.Named("metadata_sm"),
		}
	}
}

// Update applies a single Raft log entry to the in-memory state.
func (s *MetadataStateMachine) Update(entry statemachine.Entry) (statemachine.Result, error) {
	if len(entry.Cmd) == 0 {
		return statemachine.Result{Value: 0}, nil
	}

	switch CommandType(entry.Cmd[0]) {
	case UpdateTopologyCmd:
		topo, err := UnmarshalUpdateTopologyCmd(entry.Cmd)
		if err != nil {
			s.logger.Error("failed to unmarshal UpdateTopologyCmd", zap.Error(err))
			return statemachine.Result{Value: 0}, nil
		}
		s.mu.Lock()
		s.topology.Shards[topo.ShardID] = topo
		if topo.Epoch > s.topology.Epoch {
			s.topology.Epoch = topo.Epoch
		}
		s.mu.Unlock()

		s.logger.Debug("topology updated",
			zap.Uint64("shard_id", topo.ShardID),
			zap.Uint64("leader_id", topo.LeaderID),
			zap.Uint64("epoch", topo.Epoch),
		)
		return statemachine.Result{Value: 1}, nil

	default:
		s.logger.Warn("unknown metadata command type", zap.Uint8("type", entry.Cmd[0]))
		return statemachine.Result{Value: 0}, nil
	}
}

// Lookup handles read-only queries against the in-memory state.
// Supported query types:
//   - nil or "topology": returns a copy of the full TopologySnapshot
//   - uint64: returns the ShardTopology for that shard ID
func (s *MetadataStateMachine) Lookup(query interface{}) (interface{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	switch q := query.(type) {
	case nil:
		return s.copyTopology(), nil
	case string:
		if q == "topology" {
			return s.copyTopology(), nil
		}
	case uint64:
		if shard, ok := s.topology.Shards[q]; ok {
			return copyShardTopology(shard), nil
		}
		return nil, nil
	}

	return nil, nil
}

// GetTopology returns a copy of the current topology snapshot.
// Safe for concurrent use.
func (s *MetadataStateMachine) GetTopology() *TopologySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copyTopology()
}

// GetShardTopology returns a copy of the topology for a specific shard.
// Returns nil if the shard is not tracked.
func (s *MetadataStateMachine) GetShardTopology(shardID uint64) *ShardTopology {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if shard, ok := s.topology.Shards[shardID]; ok {
		return copyShardTopology(shard)
	}
	return nil
}

// SaveSnapshot serialises the in-memory state to the writer.
// For an in-memory state machine, this is used by Dragonboat to
// transfer state to new members joining the metadata group.
func (s *MetadataStateMachine) SaveSnapshot(w io.Writer, _ statemachine.ISnapshotFileCollection, _ <-chan struct{}) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Write number of shards.
	count := uint32(len(s.topology.Shards))
	if err := writeUint32(w, count); err != nil {
		return err
	}

	for _, shard := range s.topology.Shards {
		cmd, err := MarshalUpdateTopologyCmd(shard)
		if err != nil {
			return err
		}
		// Write command length + command bytes.
		if err := writeUint32(w, uint32(len(cmd))); err != nil {
			return err
		}
		if _, err := w.Write(cmd); err != nil {
			return err
		}
	}

	return nil
}

// RecoverFromSnapshot rebuilds the in-memory state from a snapshot reader.
func (s *MetadataStateMachine) RecoverFromSnapshot(r io.Reader, _ []statemachine.SnapshotFile, _ <-chan struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.topology = &TopologySnapshot{
		Shards: make(map[uint64]*ShardTopology),
	}

	count, err := readUint32(r)
	if err != nil {
		return err
	}

	for i := uint32(0); i < count; i++ {
		cmdLen, err := readUint32(r)
		if err != nil {
			return err
		}
		cmd := make([]byte, cmdLen)
		if _, err := io.ReadFull(r, cmd); err != nil {
			return err
		}
		topo, err := UnmarshalUpdateTopologyCmd(cmd)
		if err != nil {
			return err
		}
		s.topology.Shards[topo.ShardID] = topo
		if topo.Epoch > s.topology.Epoch {
			s.topology.Epoch = topo.Epoch
		}
	}

	return nil
}

// Close is a no-op for the in-memory state machine.
func (s *MetadataStateMachine) Close() error {
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (s *MetadataStateMachine) copyTopology() *TopologySnapshot {
	cp := &TopologySnapshot{
		Shards: make(map[uint64]*ShardTopology, len(s.topology.Shards)),
		Epoch:  s.topology.Epoch,
	}
	for id, shard := range s.topology.Shards {
		cp.Shards[id] = copyShardTopology(shard)
	}
	return cp
}

func copyShardTopology(t *ShardTopology) *ShardTopology {
	cp := &ShardTopology{
		ShardID:        t.ShardID,
		LeaderID:       t.LeaderID,
		LeaderAddr:     t.LeaderAddr,
		Term:           t.Term,
		Epoch:          t.Epoch,
		ConfigChangeID: t.ConfigChangeID,
		Nodes:          make(map[uint64]string, len(t.Nodes)),
		NonVotings:     make(map[uint64]string, len(t.NonVotings)),
		Witnesses:      make(map[uint64]string, len(t.Witnesses)),
	}
	for k, v := range t.Nodes {
		cp.Nodes[k] = v
	}
	for k, v := range t.NonVotings {
		cp.NonVotings[k] = v
	}
	for k, v := range t.Witnesses {
		cp.Witnesses[k] = v
	}
	return cp
}

func writeUint32(w io.Writer, v uint32) error {
	b := []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	_, err := w.Write(b)
	return err
}

func readUint32(r io.Reader) (uint32, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, err
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}
