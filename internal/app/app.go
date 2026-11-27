package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/futureq-io/stormoar/v4"
	raftconfig "github.com/futureq-io/stormoar/v4/config"
	"github.com/futureq-io/stormoar/v4/statemachine"
	"go.uber.org/zap"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/raft/leaderpersist"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/internal/subscriptions"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
)

const gracefulShutdownTimeout = 10 * time.Second

var A *App

type Repositories struct {
	Events *repository.EventRepository
}

type App struct {
	Subscriptions *subscriptions.Service
	cfg           *config.Config
	DB            storage.DB
	NodeHost      *dragonboat.NodeHost
	Ctx           context.Context
	// ShutCtx is the 10-second shutdown window context. It is populated by
	// WithGracefulShutdown immediately before a.Ctx is cancelled, so any
	// goroutine watching a.Ctx.Done() can safely read ShutCtx.
	ShutCtx      context.Context
	Repositories Repositories
	cancel       context.CancelCauseFunc
	Logger       *zap.Logger
	wg           sync.WaitGroup

	// MetadataSvc watches Dragonboat events and replicates topology changes
	// through the metadata Raft group. Nil when Raft is disabled.
	MetadataSvc *metadata.Service

	// MetadataSM is the in-memory metadata state machine instance.
	// Provides direct read access to cluster topology. Nil when Raft is disabled.
	MetadataSM *metadata.MetadataStateMachine

	// LeaderTracker tracks in-flight LEADER-ack proposals and fires when the
	// leader's local Tan log has durably written them.
	LeaderTracker *leaderpersist.Tracker
}

// Init initialises the application: sets up Pebble storage and creates the App
// singleton. Call WithRepositories() next to load the EventRepository, then
// StartRaft() if clustering is enabled.
func Init(cfg *config.Config, logger *zap.Logger) (*App, error) {
	a := &App{
		cfg:    cfg,
		Logger: logger.Named("app"),
	}

	a.Ctx, a.cancel = context.WithCancelCause(context.Background())

	var s storage.DB
	var err error
	switch cfg.Storage.Engine {
	case "pebble":
		s, err = storage.NewPebble(cfg.Storage.Pebble, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize pebble storage: %w", err)
		}
	case "bolt":
		s, err = storage.NewBoltDB(cfg.Storage.Bolt)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize bolt storage: %w", err)
		}
	}

	a.DB = s
	a.Subscriptions = subscriptions.New(s, cfg.Delivery.Subscriptions.DisconnectTimeout, logger)
	if cfg.Delivery.Subscriptions.RenewalWorkers > 0 {
		a.Subscriptions.RenewalWorkers = cfg.Delivery.Subscriptions.RenewalWorkers
	}
	if cfg.Delivery.Subscriptions.DisconnectWorkers > 0 {
		a.Subscriptions.DisconnectWorkers = cfg.Delivery.Subscriptions.DisconnectWorkers
	}
	if cfg.Cluster.Enabled {
		a.Subscriptions.Propose = func(ctx context.Context, cmd []byte) (statemachine.Result, error) {
			// Dragonboat requires a deadline, even when the RPC client omits one.
			ctx, cancel := context.WithTimeout(ctx, cfg.Publish.ProposalTimeout)
			defer cancel()
			return a.NodeHost.SyncPropose(ctx, a.NodeHost.GetNoOPSession(cfg.Cluster.ShardID), cmd)
		}
		a.Subscriptions.IsLeader = func() bool {
			id, _, valid, err := a.NodeHost.GetLeaderID(cfg.Cluster.ShardID)
			return err == nil && valid && id == cfg.Cluster.NodeID
		}
	}

	A = a

	return a, nil
}

// OpenRaft opens the NodeHost and its LogDB before choosing how to bootstrap
// the local replicas.
func (a *App) OpenRaft() error {
	cfg := a.cfg
	nhc := raftconfig.NodeHostConfig{
		WALDir:         cfg.Cluster.Raft.DataDir,
		NodeHostDir:    cfg.Cluster.Raft.DataDir,
		RTTMillisecond: uint64(cfg.Cluster.Raft.RTT.Milliseconds()),
		RaftAddress:    cfg.Cluster.Raft.Advertise,
		ListenAddress:  cfg.Cluster.Raft.Listen,
	}
	logDBConfig, err := cfg.Cluster.Raft.LogDB.DragonboatConfig()
	if err != nil {
		return err
	}
	nhc.Expert.LogDB = logDBConfig
	nhc.Expert.Engine = raftconfig.GetDefaultEngineConfig()
	nhc.Expert.Engine.ExecShards = cfg.Cluster.Raft.LogDB.Workers
	var tracker *leaderpersist.Tracker
	if cfg.Cluster.Raft.LogDB.Backend == config.LogDBTan {
		tracker = leaderpersist.NewTracker()
		nhc.Expert.LogDBFactory = leaderpersist.NewFactory(tracker)
	}

	// We need the NodeHost reference to create the propose function, but
	// Dragonboat needs the listeners at creation time. Use a forward reference:
	// create the service with a lazy propose function that captures nh later.
	var nh *dragonboat.NodeHost
	proposeMetadata := func(ctx context.Context, cmd []byte) error {
		session := nh.GetNoOPSession(metadata.MetadataShardID)
		_, err := nh.SyncPropose(ctx, session, cmd)
		return err
	}

	metadataSvc := metadata.NewService(nil, proposeMetadata, a.Logger)
	metadataSvc.ConsumerReadPermit = cfg.Delivery.ReadPermit
	if cfg.Delivery.ConsumerRemovalWorkers > 0 {
		metadataSvc.ConsumerRemovalWorkers = cfg.Delivery.ConsumerRemovalWorkers
	}
	nhc.RaftEventListener = metadataSvc
	nhc.SystemEventListener = metadataSvc

	nh, err = dragonboat.NewNodeHost(nhc)
	if err != nil {
		return fmt.Errorf("open Raft NodeHost with %s LogDB in %q: %w", cfg.Cluster.Raft.LogDB.Backend, cfg.Cluster.Raft.DataDir, err)
	}

	a.NodeHost = nh
	a.MetadataSvc = metadataSvc
	a.LeaderTracker = tracker
	metadataSvc.SetNodeHost(nh)
	return nil
}

// StartRaft starts the metadata and event replicas after OpenRaft and
// WithRepositories. join applies to replicas without a local bootstrap record.
func (a *App) StartRaft(join bool, onDeleteKeys func(keys [][]byte), onCompletedAttempts func([]raft.CompletedAttempt), onSnapshotRecovered func() error) error {
	cfg := a.cfg
	nh, metadataSvc := a.NodeHost, a.MetadataSvc
	receiptCache := cfg.Delivery.ReceiptCache
	receiptCacheBytes, err := receiptCache.Bytes()
	if err != nil {
		return fmt.Errorf("delivery.receiptCache.maxBytes: %w", err)
	}

	// Bootstrap records are per shard. A missing replica still needs the
	// original bootstrap or join settings after an interrupted first start.
	members := cfg.Cluster.Raft.InitialMembers
	bootstrap := func(shardID uint64) (map[uint64]dragonboat.Target, bool) {
		if nh.HasNodeInfo(shardID, cfg.Cluster.NodeID) {
			return nil, false
		}
		if join {
			return nil, true
		}
		return members, false
	}

	// ── Start the metadata Raft group ──────────────────────────────────────────
	// Wrap the factory to capture the state machine instance for direct reads.
	var capturedSM *metadata.MetadataStateMachine
	baseFactory := metadata.NewMetadataStateMachineFactory(a.Logger)
	metadataFactory := func(clusterID, nodeID uint64) statemachine.IStateMachine {
		sm := baseFactory(clusterID, nodeID)
		if msm, ok := sm.(*metadata.MetadataStateMachine); ok {
			capturedSM = msm
		}
		return sm
	}
	metadataRC := raftconfig.Config{
		ReplicaID:          cfg.Cluster.NodeID,
		ShardID:            metadata.MetadataShardID,
		ElectionRTT:        10,
		HeartbeatRTT:       1,
		CheckQuorum:        true,
		SnapshotEntries:    5,
		CompactionOverhead: 5, // compact aggressively since state is small
	}

	metadataMembers, joinMetadata := bootstrap(metadataRC.ShardID)
	if err := nh.StartReplica(metadataMembers, joinMetadata, metadataFactory, metadataRC); err != nil {
		return fmt.Errorf("failed to start metadata raft group: %w", err)
	}

	a.MetadataSM = capturedSM

	// Wire the gRPC address registry into the metadata service so published
	// topologies include client-facing addresses.
	metadataSvc.SetGrpcAddrsSource(capturedSM.GetGrpcAddrs)

	// ── Start the event Raft group ─────────────────────────────────────────────
	eventRC := raftconfig.Config{
		ReplicaID:          cfg.Cluster.NodeID,
		ShardID:            cfg.Cluster.ShardID,
		ElectionRTT:        10,
		HeartbeatRTT:       1,
		CheckQuorum:        true,
		SnapshotEntries:    cfg.Cluster.Raft.SnapshotEntries,
		CompactionOverhead: cfg.Cluster.Raft.CompactionOverhead,
	}

	// Pass the fully-initialised EventRepository so the state machine uses the
	// same monotonic ID counter and key schema as the standalone write path.
	eventFactory := raft.NewEventStateMachineFactoryWithReceiptCache(
		a.DB, a.Repositories.Events, onDeleteKeys, onCompletedAttempts, onSnapshotRecovered, a.Logger, receiptCacheBytes, receiptCache.MaxEntries,
	)
	eventMembers, joinEvent := bootstrap(eventRC.ShardID)
	if err := nh.StartOnDiskReplica(eventMembers, joinEvent, eventFactory, eventRC); err != nil {
		return fmt.Errorf("failed to start event raft group: %w", err)
	}

	// ── Announce the explicitly configured client address ────────────────────
	// Retry until the metadata shard has a leader. During initial cluster
	// bootstrap (OrderedReady), the first pod starts before peers exist, so
	// the shard may not be ready yet. Keep retrying until it is.
	for {
		ctx, cancel := context.WithTimeout(a.Ctx, 5*time.Second)
		err := metadataSvc.RegisterNodeAddr(ctx, cfg.Cluster.NodeID, cfg.API.GRPC.Advertise)
		cancel()
		if err == nil {
			break
		}
		a.Logger.Warn("metadata shard not ready, retrying gRPC address registration",
			zap.Error(err))
		select {
		case <-a.Ctx.Done():
			return fmt.Errorf("failed to register gRPC address: %w", err)
		case <-time.After(2 * time.Second):
		}
	}

	// Register the event shard with the metadata service so it publishes
	// initial topology.
	a.MetadataSvc.RegisterShard(cfg.Cluster.ShardID)
	// Event callbacks only enqueue topology refreshes. Start their worker after
	// the node address and initial topology have been published synchronously.
	a.RegisterComponentWithShutdown()
	go func() {
		defer a.ComponentShutdownDone()
		metadataSvc.Run(a.Ctx)
	}()

	return nil
}

// HasRaftData reports whether the open NodeHost has a local bootstrap record.
func (a *App) HasRaftData() bool {
	cl := a.cfg.Cluster
	return a.NodeHost != nil && (a.NodeHost.HasNodeInfo(metadata.MetadataShardID, cl.NodeID) ||
		a.NodeHost.HasNodeInfo(cl.ShardID, cl.NodeID))
}

// Config returns the application configuration.
func (a *App) Config() *config.Config {
	return a.cfg
}

// RegisterComponentWithShutdown increments the application wait group to track active components during shutdown.
func (a *App) RegisterComponentWithShutdown() {
	a.wg.Add(1)
}

// ComponentShutdownDone decrements the application wait group.
func (a *App) ComponentShutdownDone() {
	a.wg.Done()
}

func (a *App) WithGracefulShutdown() error {
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigterm)

	<-sigterm
	a.Logger.Info("received interrupt, shutting down gracefully...")

	// 1. Create the shared shutdown window.
	// We MUST use context.Background() as the parent, because if we use a.Ctx,
	// calling a.cancel() will immediately cancel shutCtx, causing an instant timeout.
	shutCtx, shutCancel := context.WithTimeoutCause(
		context.Background(),
		gracefulShutdownTimeout,
		errors.New("graceful shutdown timeout exceeded"),
	)
	defer shutCancel()

	a.ShutCtx = shutCtx

	// 2. Signal all components to start winding down.
	a.cancel(errors.New("graceful shutdown triggered"))

	// 3. Wait for all registered components to finish, or the timeout to expire.
	waitDone := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		a.Logger.Info("graceful shutdown completed before timeout")
	case <-shutCtx.Done():
		a.Logger.Warn("graceful shutdown timeout exceeded, forcing exit")
	}

	if a.NodeHost != nil {
		a.Logger.Info("closing Dragonboat NodeHost...")
		a.NodeHost.Close()
		a.Logger.Info("Dragonboat NodeHost closed successfully")
	}

	if err := a.DB.Flush(); err != nil {
		a.Logger.Error("failed to flush pebble on shutdown", zap.Error(err))
	}

	// 4. Safely close DB.
	if a.DB != nil {
		a.Logger.Info("closing DB...")
		if err := a.DB.Close(); err != nil {
			a.Logger.Error("failed to close DB", zap.Error(err))
		} else {
			a.Logger.Info("DB closed successfully")
		}
	}

	return nil
}

func (a *App) WithRepositories() error {
	eventRepo, err := repository.NewEventRepository(a.DB, a.Logger, a.cfg.Delivery.TimeBucket)
	if err != nil {
		return fmt.Errorf("failed to init event repo: %w", err)
	}

	a.Repositories.Events = eventRepo

	return nil
}
