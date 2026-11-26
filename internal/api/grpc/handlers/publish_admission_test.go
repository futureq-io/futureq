package handlers

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/futureq-io/stormoar/v4"
	raftconfig "github.com/futureq-io/stormoar/v4/config"
	"github.com/futureq-io/stormoar/v4/raftio"
	"github.com/futureq-io/stormoar/v4/raftpb"
	"github.com/futureq-io/stormoar/v4/statemachine"
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
	reads                atomic.Int64
	admissionUnavailable atomic.Bool
	mu                   sync.Mutex
	commands             [][]byte
	applyGate            atomic.Pointer[publishGate]
	logGate              atomic.Pointer[publishGate]
}

func (sm *observedAdmissionSM) Lookup(query interface{}) (interface{}, error) {
	sm.reads.Add(1)
	if _, ok := query.(raft.PublishRecipientsQuery); ok && sm.admissionUnavailable.Load() {
		return nil, errors.New("snapshot recovery in progress")
	}
	return sm.IOnDiskStateMachine.Lookup(query)
}

type publishGate struct {
	release chan struct{}
	entered chan struct{}
}

func holdPublish(t testing.TB, hook *atomic.Pointer[publishGate]) (func(), <-chan struct{}) {
	gate := &publishGate{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	hook.Store(gate)
	release := sync.OnceFunc(func() {
		hook.Store(nil)
		close(gate.release)
	})
	t.Cleanup(release)
	return release, gate.entered
}

func (g *publishGate) wait() {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
}

func (sm *observedAdmissionSM) Update(entries []statemachine.Entry) ([]statemachine.Entry, error) {
	for _, entry := range entries {
		if len(entry.Cmd) == 0 || raft.CommandType(entry.Cmd[0]) != raft.PublishCmd {
			continue
		}
		sm.mu.Lock()
		sm.commands = append(sm.commands, append([]byte(nil), entry.Cmd...))
		sm.mu.Unlock()
		if gate := sm.applyGate.Load(); gate != nil {
			gate.wait()
		}
	}
	return sm.IOnDiskStateMachine.Update(entries)
}

func (sm *observedAdmissionSM) publishedCommands() [][]byte {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return append([][]byte(nil), sm.commands...)
}

type observedPublishLogFactory struct {
	raftconfig.LogDBFactory
	gate *atomic.Pointer[publishGate]
}

func (f observedPublishLogFactory) Create(cfg raftconfig.NodeHostConfig, cb raftconfig.LogDBCallback, dirs, wals []string) (raftio.ILogDB, error) {
	db, err := f.LogDBFactory.Create(cfg, cb, dirs, wals)
	if err != nil {
		return nil, err
	}
	return observedPublishLogDB{ILogDB: db, gate: f.gate}, nil
}

type observedPublishLogDB struct {
	raftio.ILogDB
	gate *atomic.Pointer[publishGate]
}

func (db observedPublishLogDB) SaveRaftState(updates []raftpb.Update, shardID uint64) error {
	if gate := db.gate.Load(); gate != nil {
		for _, update := range updates {
			for _, entry := range update.EntriesToSave {
				cmd := entry.Cmd
				if entry.Type == raftpb.EncodedEntry && len(cmd) > 0 && cmd[0] == 0 {
					cmd = cmd[1:]
				}
				if len(cmd) > 0 && raft.CommandType(cmd[0]) == raft.PublishCmd {
					gate.wait()
				}
			}
		}
	}
	return db.ILogDB.SaveRaftState(updates, shardID)
}

func openRaftAdmissionTest(t *testing.T) (*app.App, *ProducerHandler, *observedAdmissionSM, *admissionReadDB) {
	return openRaftPublishFixture(t, config.DefaultPublish())
}

func openRaftPublishFixture(t testing.TB, publish config.Publish) (*app.App, *ProducerHandler, *observedAdmissionSM, *admissionReadDB) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	root := t.TempDir()
	publish.MinAckLevel, publish.ProposalTimeout = config.NoAck, 3*time.Second
	cfg := &config.Config{Cluster: config.Cluster{Enabled: true, NodeID: 1, ShardID: 1}, Storage: config.Storage{Engine: "pebble", VirtualShards: 1, Pebble: config.Pebble{Mode: "disk", DataDir: filepath.Join(root, "events"), WALEnabled: false}}, Delivery: config.DefaultDelivery(), Publish: publish}
	previous := app.A
	a, err := app.Init(cfg, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, a.WithRepositories())
	tracker := leaderpersist.NewTracker()
	nhc := raftconfig.NodeHostConfig{NodeHostDir: filepath.Join(root, "raft"), RaftAddress: address, RTTMillisecond: 10}
	sm := &observedAdmissionSM{IOnDiskStateMachine: raft.NewEventStateMachineFactory(a.DB, a.Repositories.Events, nil, zap.NewNop())(1, 1)}
	nhc.Expert.LogDBFactory = observedPublishLogFactory{LogDBFactory: leaderpersist.NewFactory(tracker), gate: &sm.logGate}
	nhc.Expert.Engine = raftconfig.EngineConfig{ExecShards: 1, CommitShards: 1, ApplyShards: 1, SnapshotShards: 1, CloseShards: 1}
	nh, err := dragonboat.NewNodeHost(nhc)
	require.NoError(t, err)
	a.NodeHost = nh
	a.LeaderTracker = tracker
	rc := raftconfig.Config{ShardID: 1, ReplicaID: 1, ElectionRTT: 10, HeartbeatRTT: 1, CheckQuorum: true, SnapshotEntries: 100000, CompactionOverhead: 5}
	require.NoError(t, nh.StartOnDiskReplica(map[uint64]string{1: address}, false, func(uint64, uint64) statemachine.IOnDiskStateMachine { return sm }, rc))
	t.Cleanup(func() { nh.Close(); require.NoError(t, a.DB.Close()); app.A = previous })
	require.Eventually(t, func() bool { id, _, valid, err := nh.GetLeaderID(1); return err == nil && valid && id == 1 }, 5*time.Second, 10*time.Millisecond)
	// The application facade is observed separately from state-machine storage:
	// quorum admission scans during apply, and early ACKs scan during lookup.
	observed := &admissionReadDB{DB: a.DB}
	a.DB = observed
	require.NoError(t, a.Subscriptions.CreateTopic(context.Background(), "orders", nil))
	producer := NewProducerHandler(zap.NewNop())
	t.Cleanup(producer.Close)
	return a, producer, sm, observed
}

func TestRaftPublishPersistsEachVirtualShardLayout(t *testing.T) {
	a, producer, _, _ := openRaftAdmissionTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	group, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)
	counts := []int{utils.MaxVirtualShards, 64, 8, 1}
	for _, count := range counts {
		a.Config().Storage.VirtualShards = count
		ack, err := producer.processBatch(ctx, &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}})
		require.NoError(t, err)
		require.True(t, ack.Success)
	}
	keys := publishedKeys(t, a.DB)
	require.Len(t, keys, len(counts))
	for _, key := range keys {
		_, bucket, id, err := utils.ParseEventKey(key)
		require.NoError(t, err)
		require.Positive(t, bucket)
		wanted := counts[id-1]
		slot := utils.EventShardSlot(key)
		require.Positive(t, slot)
		require.Equal(t, wanted, utils.SlotShardCount(slot))
		state, err := raft.ReadDeliveryState(a.DB, key)
		require.NoError(t, err)
		require.Equal(t, []string{group.Recipient}, state.Recipients)
	}
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
			require.Equal(t, int64(0), db.scans.Load()-scans, "admission must not scan outside the state machine")
		})
	}
	require.Eventually(t, func() bool { return len(publishedKeys(t, a.DB)) == 3 }, time.Second, time.Millisecond)
	commands := sm.publishedCommands()
	require.Len(t, commands, 3)
	for i, cmd := range commands {
		require.Equal(t, i != 0, cmd[1] == 1, "only early ACKs freeze destinations in the log")
	}
	for _, key := range publishedKeys(t, a.DB) {
		state, err := raft.ReadDeliveryState(a.DB, key)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{group.Recipient, universal.Recipient}, state.Recipients)
	}
	require.NoError(t, a.Subscriptions.CreateTopic(ctx, "empty", nil))
	for _, mode := range []pb.AckLevel{pb.AckLevel_ACK_LEVEL_QUORUM, pb.AckLevel_ACK_LEVEL_LEADER, pb.AckLevel_ACK_LEVEL_NO_ACK} {
		reads := sm.reads.Load()
		for _, rejection := range []struct {
			topic string
			code  codes.Code
		}{{"missing", codes.NotFound}, {"empty", codes.FailedPrecondition}} {
			ack, err := producer.processBatch(ctx, &pb.PublishBatch{AckLevel: mode, Messages: []*pb.PublishMessage{{Topic: "orders"}, {Topic: rejection.topic}}})
			require.Equal(t, rejection.code, status.Code(err))
			require.False(t, ack.Success)
		}
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

func TestRaftEarlyAckRejectsUnavailableAdmissionBeforeProposal(t *testing.T) {
	a, producer, sm, _ := openRaftAdmissionTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
	require.NoError(t, err)

	modes := []pb.AckLevel{pb.AckLevel_ACK_LEVEL_LEADER, pb.AckLevel_ACK_LEVEL_NO_ACK}
	sm.admissionUnavailable.Store(true)
	for _, mode := range modes {
		ack, err := producer.processBatch(ctx, &pb.PublishBatch{
			AckLevel: mode, Messages: []*pb.PublishMessage{{Topic: "orders"}},
		})
		require.Equal(t, codes.Unavailable, status.Code(err), mode.String())
		require.False(t, ack.GetSuccess())
	}
	require.Empty(t, sm.publishedCommands())
	require.Empty(t, publishedKeys(t, a.DB))

	sm.admissionUnavailable.Store(false)
	for _, mode := range modes {
		ack, err := producer.processBatch(ctx, &pb.PublishBatch{
			AckLevel: mode, Messages: []*pb.PublishMessage{{Topic: "orders"}},
		})
		require.NoError(t, err, mode.String())
		require.True(t, ack.GetSuccess())
	}
	require.Eventually(t, func() bool { return len(publishedKeys(t, a.DB)) == len(modes) }, time.Second, time.Millisecond)
}

func TestRaftPublishAckGuarantees(t *testing.T) {
	for _, mode := range []pb.AckLevel{pb.AckLevel_ACK_LEVEL_QUORUM, pb.AckLevel_ACK_LEVEL_LEADER, pb.AckLevel_ACK_LEVEL_NO_ACK} {
		t.Run(mode.String(), func(t *testing.T) {
			publish := config.DefaultPublish()
			publish.BatchMaxRequests, publish.BatchMaxWait = 2, 500*time.Millisecond
			a, producer, sm, _ := openRaftPublishFixture(t, publish)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := a.Subscriptions.Create(ctx, "orders", "workers", "")
			require.NoError(t, err)

			releaseLog, logged := holdPublish(t, &sm.logGate)
			releaseApply, applying := holdPublish(t, &sm.applyGate)
			returned := make(chan error, 1)
			go func() {
				ack, err := producer.processBatch(ctx, &pb.PublishBatch{AckLevel: mode, Messages: []*pb.PublishMessage{{Topic: "orders"}, {Topic: "orders"}}})
				if err == nil && !ack.Success {
					err = errors.New("publish was not acknowledged")
				}
				returned <- err
			}()
			awaitPublishSignal(t, logged)
			if mode == pb.AckLevel_ACK_LEVEL_NO_ACK {
				require.NoError(t, awaitPublishError(t, returned))
			} else {
				assertPublishWaiting(t, returned)
			}
			releaseLog()
			awaitPublishSignal(t, applying)
			require.Empty(t, publishedKeys(t, a.DB))
			if mode == pb.AckLevel_ACK_LEVEL_LEADER {
				require.NoError(t, awaitPublishError(t, returned))
			} else if mode == pb.AckLevel_ACK_LEVEL_QUORUM {
				assertPublishWaiting(t, returned)
			}
			releaseApply()
			if mode == pb.AckLevel_ACK_LEVEL_QUORUM {
				require.NoError(t, awaitPublishError(t, returned))
			}
			require.Eventually(t, func() bool { return len(publishedKeys(t, a.DB)) == 2 }, time.Second, time.Millisecond)
			commands := sm.publishedCommands()
			require.Len(t, commands, 1)
			require.Nil(t, producer.batcher, "standalone grouping must be ignored in clustered mode")
		})
	}
}

func TestRaftConcurrentPublishesHaveIndependentBudgets(t *testing.T) {
	cfg := config.DefaultPublish()
	cfg.BatchMaxRequests, cfg.BatchMaxWait, cfg.BatchMaxBytes = 2, 500*time.Millisecond, "480B"
	a, producer, sm, _ := openRaftPublishFixture(t, cfg)
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	require.Nil(t, producer.batcher)
	batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders", Payload: make([]byte, 256)}}}
	returned := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := producer.processBatch(context.Background(), batch)
			returned <- err
		}()
	}
	require.NoError(t, awaitPublishError(t, returned))
	require.NoError(t, awaitPublishError(t, returned))
	require.Len(t, sm.publishedCommands(), 2)
	require.Len(t, publishedKeys(t, a.DB), 2)
}

func TestPublishAdmissionLimitAppliesAcrossAckLevels(t *testing.T) {
	cfg := config.DefaultPublish()
	cfg.MaxPendingRequests = 1
	a, producer, sm, _ := openRaftPublishFixture(t, cfg)
	require.NoError(t, createTestInterest(context.Background(), "orders", "workers"))
	release, applying := holdPublish(t, &sm.applyGate)
	batch := &pb.PublishBatch{Messages: []*pb.PublishMessage{{Topic: "orders"}}}
	returned := make(chan error, 1)
	go func() {
		_, err := producer.processBatch(context.Background(), batch)
		returned <- err
	}()
	awaitPublishSignal(t, applying)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	early := &pb.PublishBatch{AckLevel: pb.AckLevel_ACK_LEVEL_NO_ACK, Messages: batch.Messages}
	_, err := producer.processBatch(ctx, early)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Zero(t, sm.reads.Load(), "overloaded requests should never reach receipt lookup")
	release()
	require.NoError(t, awaitPublishError(t, returned))
	require.Len(t, publishedKeys(t, a.DB), 1)
}
