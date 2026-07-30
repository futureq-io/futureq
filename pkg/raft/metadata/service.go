package metadata

import (
	"context"
	"sync"
	"time"

	"github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/raftio"
	"go.uber.org/zap"
)

// Service watches Dragonboat for leader and membership changes across all
// shards and replicates topology updates through the metadata Raft group.
//
// It implements raftio.IRaftEventListener (leader changes) and
// raftio.ISystemEventListener (membership changes). Register it on
// NodeHostConfig so Dragonboat calls it on every event.
type Service struct {
	nh     *dragonboat.NodeHost
	logger *zap.Logger

	// propose submits a command to the metadata Raft group.
	propose func(ctx context.Context, cmd []byte) error

	mu     sync.Mutex
	epoch  uint64
	shards map[uint64]struct{} // tracks which shards we know about
}

// NewService creates a metadata Service. The propose function should submit
// a command to the metadata Raft group via NodeHost.SyncPropose.
// nh may be nil during construction — call SetNodeHost before the service
// handles any events.
func NewService(nh *dragonboat.NodeHost, propose func(ctx context.Context, cmd []byte) error, logger *zap.Logger) *Service {
	return &Service{
		nh:     nh,
		propose: propose,
		logger: logger.Named("metadata_svc"),
		shards: make(map[uint64]struct{}),
	}
}

// SetNodeHost sets the NodeHost reference. Must be called before the service
// handles any Dragonboat events.
func (s *Service) SetNodeHost(nh *dragonboat.NodeHost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nh = nh
}

// ─── IRaftEventListener ──────────────────────────────────────────────────────

// LeaderUpdated is called by Dragonboat when a leader changes for any shard.
func (s *Service) LeaderUpdated(info raftio.LeaderInfo) {
	if info.ShardID == MetadataShardID {
		return // don't track the metadata shard itself
	}

	s.logger.Info("leader updated",
		zap.Uint64("shard_id", info.ShardID),
		zap.Uint64("leader_id", info.LeaderID),
		zap.Uint64("term", info.Term),
	)

	s.mu.Lock()
	s.shards[info.ShardID] = struct{}{}
	s.mu.Unlock()

	s.publishTopology(info.ShardID)
}

// ─── ISystemEventListener ────────────────────────────────────────────────────

// MembershipChanged is called by Dragonboat when membership changes for any shard.
func (s *Service) MembershipChanged(info raftio.NodeInfo) {
	if info.ShardID == MetadataShardID {
		return
	}

	s.logger.Info("membership changed",
		zap.Uint64("shard_id", info.ShardID),
		zap.Uint64("replica_id", info.ReplicaID),
	)

	s.mu.Lock()
	s.shards[info.ShardID] = struct{}{}
	s.mu.Unlock()

	s.publishTopology(info.ShardID)
}

// NodeHostShuttingDown is called when the NodeHost is shutting down.
func (s *Service) NodeHostShuttingDown() {}

// NodeUnloaded is called when a shard replica is unloaded.
func (s *Service) NodeUnloaded(info raftio.NodeInfo) {}

// NodeDeleted is called when a shard replica is deleted.
func (s *Service) NodeDeleted(info raftio.NodeInfo) {
	if info.ShardID == MetadataShardID {
		return
	}
	s.publishTopology(info.ShardID)
}

// NodeReady is called when a shard replica is ready.
func (s *Service) NodeReady(info raftio.NodeInfo) {
	if info.ShardID == MetadataShardID {
		return
	}
	s.mu.Lock()
	s.shards[info.ShardID] = struct{}{}
	s.mu.Unlock()
	s.publishTopology(info.ShardID)
}

// ConnectionEstablished is called when a connection is established.
func (s *Service) ConnectionEstablished(info raftio.ConnectionInfo) {}

// ConnectionFailed is called when a connection attempt fails.
func (s *Service) ConnectionFailed(info raftio.ConnectionInfo) {}

// SendSnapshotStarted is called when sending a snapshot starts.
func (s *Service) SendSnapshotStarted(info raftio.SnapshotInfo) {}

// SendSnapshotCompleted is called when sending a snapshot completes.
func (s *Service) SendSnapshotCompleted(info raftio.SnapshotInfo) {}

// SendSnapshotAborted is called when sending a snapshot is aborted.
func (s *Service) SendSnapshotAborted(info raftio.SnapshotInfo) {}

// SnapshotReceived is called when a snapshot is received.
func (s *Service) SnapshotReceived(info raftio.SnapshotInfo) {}

// SnapshotRecovered is called when snapshot recovery completes.
func (s *Service) SnapshotRecovered(info raftio.SnapshotInfo) {}

// SnapshotCreated is called when a snapshot is created.
func (s *Service) SnapshotCreated(info raftio.SnapshotInfo) {}

// SnapshotCompacted is called when a snapshot is compacted.
func (s *Service) SnapshotCompacted(info raftio.SnapshotInfo) {}

// LogCompacted is called when the Raft log is compacted.
func (s *Service) LogCompacted(info raftio.EntryInfo) {}

// LogDBCompacted is called when the LogDB is compacted.
func (s *Service) LogDBCompacted(info raftio.EntryInfo) {}

// ─── Topology Publishing ────────────────────────────────────────────────────

// publishTopology queries Dragonboat for the current shard topology and
// proposes it to the metadata Raft group.
func (s *Service) publishTopology(shardID uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Get membership info.
	membership, err := s.nh.SyncGetShardMembership(ctx, shardID)
	if err != nil {
		s.logger.Error("failed to get shard membership",
			zap.Uint64("shard_id", shardID),
			zap.Error(err),
		)
		return
	}

	// Get leader info.
	leaderID, term, valid, err := s.nh.GetLeaderID(shardID)
	if err != nil || !valid {
		leaderID = 0
		term = 0
	}

	// Resolve leader address.
	leaderAddr := ""
	if leaderID > 0 {
		if addr, ok := membership.Nodes[leaderID]; ok {
			leaderAddr = addr
		}
	}

	s.mu.Lock()
	s.epoch++
	epoch := s.epoch
	s.mu.Unlock()

	topo := &ShardTopology{
		ShardID:        shardID,
		LeaderID:       leaderID,
		LeaderAddr:     leaderAddr,
		Term:           term,
		Epoch:          epoch,
		ConfigChangeID: membership.ConfigChangeID,
		Nodes:          membership.Nodes,
		NonVotings:     membership.NonVotings,
		Witnesses:      membership.Witnesses,
	}

	cmd, err := MarshalUpdateTopologyCmd(topo)
	if err != nil {
		s.logger.Error("failed to marshal topology command", zap.Error(err))
		return
	}

	if err := s.propose(ctx, cmd); err != nil {
		s.logger.Error("failed to propose topology update",
			zap.Uint64("shard_id", shardID),
			zap.Error(err),
		)
		return
	}

	s.logger.Debug("published topology",
		zap.Uint64("shard_id", shardID),
		zap.Uint64("leader_id", leaderID),
		zap.Uint64("epoch", epoch),
	)
}

// RefreshAll re-publishes topology for all known shards.
// Useful after startup to ensure the metadata group has the latest state.
func (s *Service) RefreshAll() {
	s.mu.Lock()
	shardIDs := make([]uint64, 0, len(s.shards))
	for id := range s.shards {
		shardIDs = append(shardIDs, id)
	}
	s.mu.Unlock()

	for _, id := range shardIDs {
		s.publishTopology(id)
	}
}

// RegisterShard explicitly adds a shard to the tracking set.
// Called during startup for shards that may not have fired events yet.
func (s *Service) RegisterShard(shardID uint64) {
	s.mu.Lock()
	s.shards[shardID] = struct{}{}
	s.mu.Unlock()
	s.publishTopology(shardID)
}
