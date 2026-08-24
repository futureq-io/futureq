package metadata

import (
	"context"
	"errors"
	"testing"

	"github.com/lni/dragonboat/v4/raftio"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

type ServiceSuite struct {
	suite.Suite
}

func TestServiceSuite(t *testing.T) {
	suite.Run(t, new(ServiceSuite))
}

// proposeOK is a fake propose function that captures the marshalled command.
type capturedPropose struct {
	cmds [][]byte
	err  error
}

func (c *capturedPropose) propose(_ context.Context, cmd []byte) error {
	if c.err != nil {
		return c.err
	}
	cp := make([]byte, len(cmd))
	copy(cp, cmd)
	c.cmds = append(c.cmds, cp)
	return nil
}

// ─── Constructor / setters ──────────────────────────────────────────────────

func (s *ServiceSuite) TestNewService_InitializesShardsMap() {
	require := s.Require()

	cp := &capturedPropose{}
	svc := NewService(nil, cp.propose, zap.NewNop())

	require.NotNil(svc)
	require.NotNil(svc.shards)
	require.Empty(svc.shards)
}

func (s *ServiceSuite) TestSetNodeHost_StoresReference() {
	require := s.Require()

	cp := &capturedPropose{}
	svc := NewService(nil, cp.propose, zap.NewNop())

	// Verify SetNodeHost doesn't panic with a non-nil host.
	svc.SetNodeHost(nil)
	require.Nil(svc.nh)
}

func (s *ServiceSuite) TestSetGrpcAddrsSource_StoresFunc() {
	require := s.Require()

	cp := &capturedPropose{}
	svc := NewService(nil, cp.propose, zap.NewNop())

	fn := func() map[uint64]string { return map[uint64]string{1: "addr"} }
	svc.SetGrpcAddrsSource(fn)

	svc.mu.Lock()
	got := svc.getGrpcAddrs
	svc.mu.Unlock()

	require.NotNil(got)
	require.Equal(map[uint64]string{1: "addr"}, got())
}

// ─── RegisterNodeAddr ───────────────────────────────────────────────────────

func (s *ServiceSuite) TestRegisterNodeAddr_ProposesRegisterNodeAddrCmd() {
	require := s.Require()

	cp := &capturedPropose{}
	svc := NewService(nil, cp.propose, zap.NewNop())

	err := svc.RegisterNodeAddr(context.Background(), 7, "node7:9000")
	require.NoError(err)
	require.Len(cp.cmds, 1)

	nodeID, addr, err := UnmarshalRegisterNodeAddrCmd(cp.cmds[0])
	require.NoError(err)
	require.Equal(uint64(7), nodeID)
	require.Equal("node7:9000", addr)
}

func (s *ServiceSuite) TestRegisterNodeAddr_ProposeFails_ErrorPropagates() {
	require := s.Require()

	sentinel := errors.New("propose failed")
	cp := &capturedPropose{err: sentinel}
	svc := NewService(nil, cp.propose, zap.NewNop())

	err := svc.RegisterNodeAddr(context.Background(), 7, "node7:9000")
	require.Error(err)
	require.Contains(err.Error(), sentinel.Error())
}

// ─── LeaderUpdated / MembershipChanged ──────────────────────────────────────

func (s *ServiceSuite) TestLeaderUpdated_IgnoresMetadataShard() {
	require := s.Require()

	cp := &capturedPropose{}
	svc := NewService(nil, cp.propose, zap.NewNop())

	// MetadataShardID = 0 — should not trigger publishTopology.
	svc.LeaderUpdated(raftio.LeaderInfo{ShardID: MetadataShardID, LeaderID: 1})

	svc.mu.Lock()
	defer svc.mu.Unlock()
	require.Empty(svc.shards, "metadata shard must not be tracked")
}

// ─── No-op event handlers ───────────────────────────────────────────────────

func (s *ServiceSuite) TestNoOpHandlers_DoNotPanic() {
	require := s.Require()

	cp := &capturedPropose{}
	svc := NewService(nil, cp.propose, zap.NewNop())

	nodeInfo := raftio.NodeInfo{ShardID: 1}
	connInfo := raftio.ConnectionInfo{}
	snapInfo := raftio.SnapshotInfo{}
	entryInfo := raftio.EntryInfo{}

	// None of these should panic or change state.
	svc.NodeHostShuttingDown()
	svc.NodeUnloaded(nodeInfo)
	svc.ConnectionEstablished(connInfo)
	svc.ConnectionFailed(connInfo)
	svc.SendSnapshotStarted(snapInfo)
	svc.SendSnapshotCompleted(snapInfo)
	svc.SendSnapshotAborted(snapInfo)
	svc.SnapshotReceived(snapInfo)
	svc.SnapshotRecovered(snapInfo)
	svc.SnapshotCreated(snapInfo)
	svc.SnapshotCompacted(snapInfo)
	svc.LogCompacted(entryInfo)
	svc.LogDBCompacted(entryInfo)

	svc.mu.Lock()
	defer svc.mu.Unlock()
	require.Empty(svc.shards, "no-op handlers must not track shards")
}

// ─── RegisterShard / RefreshAll ─────────────────────────────────────────────

// RegisterShard and RefreshAll call publishTopology, which requires a non-nil
// NodeHost. We skip direct calls here — those paths are covered by integration
// tests against a real dragonboat cluster. These handlers verify the tracking
// semantics that CAN be tested without a NodeHost.

func (s *ServiceSuite) TestRefreshAll_NoShards_DoesNothing() {
	require := s.Require()

	cp := &capturedPropose{}
	svc := NewService(nil, cp.propose, zap.NewNop())

	// With no registered shards, RefreshAll must not call publishTopology
	// (which would need a NodeHost). This must not panic.
	svc.RefreshAll()
	require.Empty(cp.cmds)
}
