package handlers

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/raft/leaderpersist"
	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/lni/dragonboat/v4"
	raftconfig "github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type admissionReadDB struct {
	storage.DB
	scans atomic.Int64
}

func (db *admissionReadDB) Scan(opts *storage.IterOptions, yield func([]byte, []byte) error) error {
	if opts != nil && bytes.HasPrefix(opts.LowerBound, raft.SubscriptionPrefix) {
		db.scans.Add(1)
	}
	return db.DB.Scan(opts, yield)
}

type observedAdmissionSM struct {
	statemachine.IOnDiskStateMachine
	reads    atomic.Int64
	mu       sync.Mutex
	commands [][]byte
	block    <-chan struct{}
	entered  chan struct{}
}

func (sm *observedAdmissionSM) Lookup(query interface{}) (interface{}, error) {
	sm.reads.Add(1)
	return sm.IOnDiskStateMachine.Lookup(query)
}
func (sm *observedAdmissionSM) Update(entries []statemachine.Entry) ([]statemachine.Entry, error) {
	for _, entry := range entries {
		if len(entry.Cmd) == 0 {
			continue
		}
		sm.mu.Lock()
		sm.commands = append(sm.commands, append([]byte(nil), entry.Cmd...))
		block, entered := sm.block, sm.entered
		sm.mu.Unlock()
		if raft.CommandType(entry.Cmd[0]) == raft.StoreDeliveryBatchCmd && block != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-block
		}
	}
	return sm.IOnDiskStateMachine.Update(entries)
}
func (sm *observedAdmissionSM) holdEarlyApply() (chan struct{}, chan struct{}) {
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	sm.mu.Lock()
	sm.block = release
	sm.entered = entered
	sm.mu.Unlock()
	return release, entered
}
func (sm *observedAdmissionSM) publishedCommands() [][]byte {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	var commands [][]byte
	for _, cmd := range sm.commands {
		if raft.CommandType(cmd[0]) == raft.PublishBatchCmd || raft.CommandType(cmd[0]) == raft.StoreDeliveryBatchCmd {
			commands = append(commands, cmd)
		}
	}
	return commands
}
func openRaftAdmissionTest(t *testing.T) (*app.App, *ProducerHandler, *observedAdmissionSM, *admissionReadDB) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	root := t.TempDir()
	cfg := &config.Config{Cluster: config.Cluster{Enabled: true, NodeID: 1, ShardID: 1}, Storage: config.Storage{Engine: "pebble", Pebble: config.Pebble{Mode: "disk", DataDir: filepath.Join(root, "events"), WALEnabled: false}}, Delivery: config.DefaultDelivery(), Publish: config.Publish{MinAckLevel: config.NoAck, ProposalTimeout: 3 * time.Second}}
	previous := app.A
	a, err := app.Init(cfg, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, a.WithRepositories())
	tracker := leaderpersist.NewTracker()
	nhc := raftconfig.NodeHostConfig{NodeHostDir: filepath.Join(root, "raft"), RaftAddress: address, RTTMillisecond: 10}
	nhc.Expert.LogDBFactory = leaderpersist.NewFactory(tracker)
	nhc.Expert.Engine = raftconfig.EngineConfig{ExecShards: 1, CommitShards: 1, ApplyShards: 1, SnapshotShards: 1, CloseShards: 1}
	nh, err := dragonboat.NewNodeHost(nhc)
	require.NoError(t, err)
	a.NodeHost = nh
	a.LeaderTracker = tracker
	sm := &observedAdmissionSM{IOnDiskStateMachine: raft.NewEventStateMachineFactory(a.DB, a.Repositories.Events, nil, zap.NewNop())(1, 1)}
	rc := raftconfig.Config{ShardID: 1, ReplicaID: 1, ElectionRTT: 10, HeartbeatRTT: 1, CheckQuorum: true, SnapshotEntries: 20, CompactionOverhead: 5}
	require.NoError(t, nh.StartOnDiskReplica(map[uint64]string{1: address}, false, func(uint64, uint64) statemachine.IOnDiskStateMachine { return sm }, rc))
	t.Cleanup(func() { nh.Close(); require.NoError(t, a.DB.Close()); app.A = previous })
	require.Eventually(t, func() bool { id, _, valid, err := nh.GetLeaderID(1); return err == nil && valid && id == 1 }, 5*time.Second, 10*time.Millisecond)
	// The application facade is observed separately from state-machine storage:
	// admission scans must move into apply for quorum, and remain for early ACKs.
	observed := &admissionReadDB{DB: a.DB}
	a.DB = observed
	return a, NewProducerHandler(zap.NewNop()), sm, observed
}
func TestRaftPublishAdmissionReadCountsAndAllAckModes(t *testing.T) {
	a, producer, sm, db := openRaftAdmissionTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	group, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	universal, err := a.Subscriptions.Create(ctx, "orders", "", "audit")
	require.NoError(t, err)
	for _, mode := range []pb.AckLevel{pb.AckLevel_ACK_LEVEL_QUORUM, pb.AckLevel_ACK_LEVEL_LEADER, pb.AckLevel_ACK_LEVEL_NO_ACK} {
		t.Run(mode.String(), func(t *testing.T) {
			reads, scans := sm.reads.Load(), db.scans.Load()
			ack, err := producer.processBatch(ctx, &pb.PublishBatch{AckLevel: mode, Messages: []*pb.PublishMessage{{Topic: "orders", Payload: []byte("payload")}}})
			require.NoError(t, err)
			require.True(t, ack.Success)
			expected := int64(1)
			if mode == pb.AckLevel_ACK_LEVEL_QUORUM {
				expected = 0
			}
			require.Equal(t, expected, sm.reads.Load()-reads, "admission SyncRead calls")
			require.Equal(t, expected, db.scans.Load()-scans, "handler-side subscription scans")
		})
	}
	require.Eventually(t, func() bool { return len(publishedKeys(t, a.DB)) == 3 }, time.Second, time.Millisecond)
	commands := sm.publishedCommands()
	require.Len(t, commands, 3)
	require.Equal(t, byte(raft.PublishBatchCmd), commands[0][0])
	for _, cmd := range commands[1:] {
		items, err := raft.UnmarshalStoreDeliveryBatchCmd(cmd)
		require.NoError(t, err)
		require.NotEmpty(t, items[0].Receipt, "early ACKs persist frozen destinations in the log")
	}
	for _, key := range publishedKeys(t, a.DB) {
		state, err := raft.ReadDeliveryState(a.DB, key)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{group.Recipient, universal.Recipient}, state.Recipients)
	}
	for _, mode := range []pb.AckLevel{pb.AckLevel_ACK_LEVEL_QUORUM, pb.AckLevel_ACK_LEVEL_LEADER, pb.AckLevel_ACK_LEVEL_NO_ACK} {
		reads := sm.reads.Load()
		ack, err := producer.processBatch(ctx, &pb.PublishBatch{AckLevel: mode, Messages: []*pb.PublishMessage{{Topic: "orders"}, {Topic: "missing"}}})
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.False(t, ack.Success)
		if mode == pb.AckLevel_ACK_LEVEL_QUORUM {
			require.Equal(t, reads, sm.reads.Load())
		}
		require.Len(t, publishedKeys(t, a.DB), 3, "whole unroutable batch must write no messages")
	}
	ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}})
	require.NoError(t, err)
	require.True(t, ack.Success, "rejected admission must not halt the state machine")
	ack, err = producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: ""}}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.False(t, ack.Success)
}
func TestRaftEarlyAcksStillReturnBeforeApply(t *testing.T) {
	a, producer, sm, _ := openRaftAdmissionTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	for _, mode := range []pb.AckLevel{pb.AckLevel_ACK_LEVEL_LEADER, pb.AckLevel_ACK_LEVEL_NO_ACK} {
		t.Run(mode.String(), func(t *testing.T) {
			release, entered := sm.holdEarlyApply()
			defer close(release)
			returned := make(chan error, 1)
			go func() {
				ack, err := producer.processBatch(ctx, &pb.PublishBatch{AckLevel: mode, Messages: []*pb.PublishMessage{{Topic: "orders"}}})
				if err == nil && !ack.Success {
					err = context.Canceled
				}
				returned <- err
			}()
			select {
			case err := <-returned:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("early ACK waited for state-machine application")
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("store command never reached blocked apply")
			}
		})
	}
}
