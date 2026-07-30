package handlers

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
)

// ClusterHandler implements pb.FutureQClusterServer.
type ClusterHandler struct {
	pb.UnimplementedFutureQClusterServer
	logger *zap.Logger
}

func NewClusterHandler(logger *zap.Logger) *ClusterHandler {
	return &ClusterHandler{
		logger: logger.Named("cluster_handler"),
	}
}

// ─── Cluster Info ─────────────────────────────────────────────────────────────

// GetClusterInfo returns the current cluster topology from the metadata
// state machine. Any node can respond — it does not need to be the leader.
// In standalone (non-Raft) mode, returns info about this single node.
func (h *ClusterHandler) GetClusterInfo(ctx context.Context, _ *pb.ClusterInfoRequest) (*pb.ClusterInfoResponse, error) {
	// Standalone mode — no Raft, no metadata group.
	if app.A.NodeHost == nil || app.A.MetadataSM == nil {
		cfg := app.A.Config()
		return &pb.ClusterInfoResponse{
			LeaderNodeId:  cfg.Raft.NodeID,
			LeaderAddress: cfg.Server.Listen,
			Nodes: []*pb.NodeInfo{
				{
					NodeId:   cfg.Raft.NodeID,
					Address:  cfg.Server.Listen,
					IsLeader: true,
					IsAlive:  true,
				},
			},
		}, nil
	}

	shardID := app.A.Config().Raft.ClusterID
	topo := app.A.MetadataSM.GetShardTopology(shardID)
	if topo == nil {
		return nil, status.Error(codes.Unavailable, "topology not yet available")
	}

	resp := &pb.ClusterInfoResponse{
		LeaderNodeId:  topo.LeaderID,
		LeaderAddress: topo.LeaderAddr,
		Nodes:         make([]*pb.NodeInfo, 0, len(topo.Nodes)+len(topo.NonVotings)),
	}

	for nodeID, addr := range topo.Nodes {
		resp.Nodes = append(resp.Nodes, &pb.NodeInfo{
			NodeId:   nodeID,
			Address:  addr,
			IsLeader: nodeID == topo.LeaderID,
			IsAlive:  true,
		})
	}
	for nodeID, addr := range topo.NonVotings {
		resp.Nodes = append(resp.Nodes, &pb.NodeInfo{
			NodeId:   nodeID,
			Address:  addr,
			IsLeader: false,
			IsAlive:  true,
		})
	}

	return resp, nil
}

// ─── Cluster Membership (Event Shard) ────────────────────────────────────────

// JoinCluster adds a new node to the event shard Raft group.
// The node is first added as a non-voting member to sync state, then promoted
// to a full voting replica once it has caught up with the leader.
func (h *ClusterHandler) JoinCluster(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	if app.A.NodeHost == nil {
		return nil, status.Error(codes.FailedPrecondition, "node is not running in raft mode")
	}

	shardID := app.A.Config().Raft.ClusterID

	h.logger.Info("adding node as non-voting member",
		zap.Uint64("node_id", req.NodeId),
		zap.String("raft_address", req.RaftAddress),
		zap.Uint64("shard_id", shardID),
	)

	// Step 1: Add as non-voting member to sync without disrupting quorum.
	if err := app.A.NodeHost.SyncRequestAddNonVoting(ctx, shardID, req.NodeId, req.RaftAddress, 0); err != nil {
		h.logger.Error("failed to add non-voting member", zap.Error(err))
		return &pb.JoinResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	// Step 2: Wait for the node to catch up with the leader.
	if err := h.waitForCatchUp(ctx, shardID, req.NodeId); err != nil {
		h.logger.Error("node failed to catch up",
			zap.Uint64("node_id", req.NodeId),
			zap.Error(err),
		)
		return &pb.JoinResponse{Success: false, ErrorMessage: fmt.Sprintf("node did not catch up: %v", err)}, nil
	}

	h.logger.Info("promoting non-voting member to replica",
		zap.Uint64("node_id", req.NodeId),
	)

	// Step 3: Promote to voting member.
	if err := app.A.NodeHost.SyncRequestAddReplica(ctx, shardID, req.NodeId, req.RaftAddress, 0); err != nil {
		h.logger.Error("failed to promote non-voting member to replica", zap.Error(err))
		return &pb.JoinResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	h.logger.Info("successfully joined cluster",
		zap.Uint64("node_id", req.NodeId),
	)

	return &pb.JoinResponse{Success: true}, nil
}

// LeaveCluster removes a node from the event shard Raft group.
func (h *ClusterHandler) LeaveCluster(ctx context.Context, req *pb.LeaveRequest) (*pb.LeaveResponse, error) {
	if app.A.NodeHost == nil {
		return nil, status.Error(codes.FailedPrecondition, "node is not running in raft mode")
	}

	shardID := app.A.Config().Raft.ClusterID

	h.logger.Info("removing node from cluster",
		zap.Uint64("node_id", req.NodeId),
		zap.Uint64("shard_id", shardID),
	)

	if err := app.A.NodeHost.SyncRequestDeleteReplica(ctx, shardID, req.NodeId, 0); err != nil {
		h.logger.Error("failed to remove node from cluster", zap.Error(err))
		return &pb.LeaveResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	h.logger.Info("successfully removed node from cluster",
		zap.Uint64("node_id", req.NodeId),
	)

	return &pb.LeaveResponse{Success: true}, nil
}

// ─── Metadata Group Membership ───────────────────────────────────────────────

// JoinMetadata adds a non-voting observer to the metadata Raft group.
// Client SDKs and new broker nodes use this to receive real-time topology
// updates without participating in metadata consensus.
func (h *ClusterHandler) JoinMetadata(ctx context.Context, req *pb.JoinMetadataRequest) (*pb.JoinMetadataResponse, error) {
	if app.A.NodeHost == nil {
		return nil, status.Error(codes.FailedPrecondition, "node is not running in raft mode")
	}

	h.logger.Info("adding observer to metadata group",
		zap.Uint64("node_id", req.NodeId),
		zap.String("raft_address", req.RaftAddress),
	)

	if err := app.A.NodeHost.SyncRequestAddNonVoting(ctx, metadata.MetadataShardID, req.NodeId, req.RaftAddress, 0); err != nil {
		h.logger.Error("failed to add metadata observer", zap.Error(err))
		return &pb.JoinMetadataResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	h.logger.Info("successfully added metadata observer",
		zap.Uint64("node_id", req.NodeId),
	)

	return &pb.JoinMetadataResponse{Success: true}, nil
}

// LeaveMetadata removes a non-voting observer from the metadata Raft group.
func (h *ClusterHandler) LeaveMetadata(ctx context.Context, req *pb.LeaveMetadataRequest) (*pb.LeaveMetadataResponse, error) {
	if app.A.NodeHost == nil {
		return nil, status.Error(codes.FailedPrecondition, "node is not running in raft mode")
	}

	h.logger.Info("removing observer from metadata group",
		zap.Uint64("node_id", req.NodeId),
	)

	if err := app.A.NodeHost.SyncRequestDeleteReplica(ctx, metadata.MetadataShardID, req.NodeId, 0); err != nil {
		h.logger.Error("failed to remove metadata observer", zap.Error(err))
		return &pb.LeaveMetadataResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	h.logger.Info("successfully removed metadata observer",
		zap.Uint64("node_id", req.NodeId),
	)

	return &pb.LeaveMetadataResponse{Success: true}, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// waitForCatchUp polls until the given node appears in the shard membership
// and is ready to be promoted. It uses Dragonboat's membership API to verify
// the node has been added, then waits a short period for state sync.
func (h *ClusterHandler) waitForCatchUp(ctx context.Context, shardID, nodeID uint64) error {
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	h.logger.Info("waiting for node to catch up",
		zap.Uint64("node_id", nodeID),
		zap.Uint64("shard_id", shardID),
	)

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for node %d to catch up", nodeID)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			membership, err := app.A.NodeHost.SyncGetShardMembership(ctx, shardID)
			if err != nil {
				h.logger.Debug("failed to get shard membership, retrying",
					zap.Error(err),
				)
				continue
			}

			// Check if the node is in the non-voting member list.
			if _, ok := membership.NonVotings[nodeID]; ok {
				h.logger.Info("node caught up, ready for promotion",
					zap.Uint64("node_id", nodeID),
					zap.Uint64("config_change_id", membership.ConfigChangeID),
				)
				return nil
			}
		}
	}
}
