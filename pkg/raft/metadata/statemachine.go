package metadata

import (
	"encoding/json"
	"io"
	"sort"
	"sync"

	"github.com/lni/dragonboat/v4/statemachine"
	"go.uber.org/zap"
)

// MetadataStateMachine implements statemachine.IStateMachine (in-memory).
// It stores the cluster topology — per-shard leader info, membership, and roles.
// It also keeps a global nodeID → gRPC address registry populated by
// RegisterNodeAddrCmd; publishTopology merges this into each shard's GrpcAddrs.
// State is fully transient: rebuilt from the Raft log on restart.
type MetadataStateMachine struct {
	mu        sync.RWMutex
	topology  *TopologySnapshot
	grpcAddrs map[uint64]string // nodeID → client-facing gRPC address
	consumers ConsumerState
	barrierMu sync.RWMutex
	// consumerBarrier excludes a local delivery while membership changes.
	// Set once during startup, before subscriptions are accepted.
	consumerBarrier func(func())
	logger          *zap.Logger
}

// NewMetadataStateMachineFactory returns the factory function that Dragonboat
// passes (clusterID, nodeID) to when it instantiates a new replica.
func NewMetadataStateMachineFactory(logger *zap.Logger) func(uint64, uint64) statemachine.IStateMachine {
	return func(clusterID, nodeID uint64) statemachine.IStateMachine {
		return &MetadataStateMachine{
			topology: &TopologySnapshot{
				Shards: make(map[uint64]*ShardTopology),
			},
			grpcAddrs: make(map[uint64]string),
			consumers: ConsumerState{Groups: make(map[string]map[string][]ConsumerMember), Acked: make(map[uint64]bool)},
			logger:    logger.Named("metadata_sm"),
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
		s.withConsumerBarrier(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.topology.Shards[topo.ShardID] = topo
			if topo.Epoch > s.topology.Epoch {
				s.topology.Epoch = topo.Epoch
			}
			if len(s.consumers.Groups) > 0 {
				s.consumers.applyTopology(consumerNodes(topo))
			}
		})

		s.logger.Debug("topology updated",
			zap.Uint64("shard_id", topo.ShardID),
			zap.Uint64("leader_id", topo.LeaderID),
			zap.Uint64("epoch", topo.Epoch),
		)
		return statemachine.Result{Value: 1}, nil

	case ConsumerChangeCmd:
		var change consumerChange
		if err := json.Unmarshal(entry.Cmd[1:], &change); err != nil {
			s.logger.Error("failed to unmarshal ConsumerChangeCmd", zap.Error(err))
			return statemachine.Result{Value: 0}, nil
		}
		var changed bool
		s.withConsumerBarrier(func() {
			s.mu.Lock()
			changed = s.consumers.applyChange(change)
			s.mu.Unlock()
		})
		if changed {
			return statemachine.Result{Value: 1}, nil
		}
		return statemachine.Result{Value: 0}, nil

	case ConsumerAckCmd:
		var ack consumerAck
		if err := json.Unmarshal(entry.Cmd[1:], &ack); err != nil {
			s.logger.Error("failed to unmarshal ConsumerAckCmd", zap.Error(err))
			return statemachine.Result{Value: 0}, nil
		}
		s.mu.Lock()
		s.consumers.acknowledge(ack)
		s.mu.Unlock()
		return statemachine.Result{Value: 1}, nil

	case ConsumerFenceCmd:
		var ack consumerAck
		if err := json.Unmarshal(entry.Cmd[1:], &ack); err != nil {
			return statemachine.Result{}, err
		}
		s.withConsumerBarrier(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.consumers.fence(ack.Epoch)
		})
		return statemachine.Result{Value: 1}, nil

	case RegisterNodeAddrCmd:
		nodeID, grpcAddr, err := UnmarshalRegisterNodeAddrCmd(entry.Cmd)
		if err != nil {
			s.logger.Error("failed to unmarshal RegisterNodeAddrCmd", zap.Error(err))
			return statemachine.Result{Value: 0}, nil
		}
		s.mu.Lock()
		s.grpcAddrs[nodeID] = grpcAddr
		s.mu.Unlock()

		s.logger.Debug("registered node gRPC address",
			zap.Uint64("node_id", nodeID),
			zap.String("grpc_addr", grpcAddr),
		)
		return statemachine.Result{Value: 1}, nil

	default:
		s.logger.Warn("unknown metadata command type", zap.Uint8("type", entry.Cmd[0]))
		return statemachine.Result{Value: 0}, nil
	}
}

func consumerNodes(topo *ShardTopology) []uint64 {
	ids := make([]uint64, 0, len(topo.Nodes)+len(topo.NonVotings))
	for id := range topo.Nodes {
		ids = append(ids, id)
	}
	for id := range topo.NonVotings {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return uniqueNodeIDs(ids)
}

// SetConsumerBarrier installs the local delivery fence used on membership
// changes. It must be installed before accepting consumer streams.
func (s *MetadataStateMachine) SetConsumerBarrier(barrier func(func())) {
	s.barrierMu.Lock()
	s.consumerBarrier = barrier
	s.barrierMu.Unlock()
}

func (s *MetadataStateMachine) withConsumerBarrier(apply func()) {
	s.barrierMu.RLock()
	barrier := s.consumerBarrier
	s.barrierMu.RUnlock()
	if barrier != nil {
		barrier(apply)
	} else {
		apply()
	}
}

// ConsumerGroup returns the active, cluster-wide ordered group assignment.
// A pending rebalance returns no members so replicas pause delivery.
func (s *MetadataStateMachine) ConsumerGroup(topic, group string) []ConsumerMember {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.consumers.Pending {
		return nil
	}
	members := s.consumers.Groups[topic][group]
	return append([]ConsumerMember(nil), members...)
}

// ConsumerStatus returns the current epoch, its barrier state, and whether
// this node has already acknowledged that epoch.
func (s *MetadataStateMachine) ConsumerStatus(nodeID uint64) (uint64, bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	needed := false
	for _, id := range s.consumers.Required {
		if id == nodeID {
			needed = true
			break
		}
	}
	return s.consumers.Epoch, s.consumers.Pending && needed, s.consumers.Acked[nodeID]
}

// ConsumerActive reports whether the member is in an activated assignment.
func (s *MetadataStateMachine) ConsumerActive(topic, group, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.consumers.Pending {
		return false
	}
	for _, member := range s.consumers.Groups[topic][group] {
		if member.ID == id {
			return true
		}
	}
	return false
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

// GetGrpcAddrs returns a copy of the global nodeID → gRPC address registry.
// Safe for concurrent use.
func (s *MetadataStateMachine) GetGrpcAddrs() map[uint64]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[uint64]string, len(s.grpcAddrs))
	for k, v := range s.grpcAddrs {
		cp[k] = v
	}
	return cp
}

// SaveSnapshot serialises the in-memory state to the writer.
// For an in-memory state machine, this is used by Dragonboat to
// transfer state to new members joining the metadata group.
func (s *MetadataStateMachine) SaveSnapshot(w io.Writer, _ statemachine.ISnapshotFileCollection, _ <-chan struct{}) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Write the gRPC address registry first.
	if err := writeUint32(w, uint32(len(s.grpcAddrs))); err != nil {
		return err
	}
	for nodeID, addr := range s.grpcAddrs {
		cmd, err := MarshalRegisterNodeAddrCmd(nodeID, addr)
		if err != nil {
			return err
		}
		if err := writeUint32(w, uint32(len(cmd))); err != nil {
			return err
		}
		if _, err := w.Write(cmd); err != nil {
			return err
		}
	}

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
	consumerData, err := json.Marshal(s.consumers)
	if err != nil {
		return err
	}
	if err := writeUint32(w, uint32(len(consumerData))); err != nil {
		return err
	}
	if _, err := w.Write(consumerData); err != nil {
		return err
	}

	return nil
}

// RecoverFromSnapshot rebuilds the in-memory state from a snapshot reader.
func (s *MetadataStateMachine) RecoverFromSnapshot(r io.Reader, _ []statemachine.SnapshotFile, _ <-chan struct{}) error {
	var result error
	s.withConsumerBarrier(func() { result = s.recoverSnapshot(r) })
	return result
}

func (s *MetadataStateMachine) recoverSnapshot(r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.topology = &TopologySnapshot{
		Shards: make(map[uint64]*ShardTopology),
	}
	s.grpcAddrs = make(map[uint64]string)
	s.consumers = ConsumerState{Groups: make(map[string]map[string][]ConsumerMember), Acked: make(map[uint64]bool)}

	// Read the gRPC address registry.
	grpcCount, err := readUint32(r)
	if err != nil {
		return err
	}
	for i := uint32(0); i < grpcCount; i++ {
		cmdLen, err := readUint32(r)
		if err != nil {
			return err
		}
		cmd := make([]byte, cmdLen)
		if _, err := io.ReadFull(r, cmd); err != nil {
			return err
		}
		nodeID, addr, err := UnmarshalRegisterNodeAddrCmd(cmd)
		if err != nil {
			return err
		}
		s.grpcAddrs[nodeID] = addr
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
	consumerLen, err := readUint32(r)
	if err == io.EOF {
		return nil // snapshots from before consumer groups were introduced
	}
	if err != nil {
		return err
	}
	consumerData := make([]byte, consumerLen)
	if _, err := io.ReadFull(r, consumerData); err != nil {
		return err
	}
	if err := json.Unmarshal(consumerData, &s.consumers); err != nil {
		return err
	}
	s.consumers.normalize()

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
		GrpcAddrs:      make(map[uint64]string, len(t.GrpcAddrs)),
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
	for k, v := range t.GrpcAddrs {
		cp.GrpcAddrs[k] = v
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
