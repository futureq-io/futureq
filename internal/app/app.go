package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/lni/dragonboat/v4"
	raftconfig "github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/statemachine"
	"go.uber.org/zap"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
)

const gracefulShutdownTimeout = 10 * time.Second

var A *App

type Repositories struct {
	Events *repository.EventRepository
}

type App struct {
	cfg      *config.Config
	DB       storage.DB
	NodeHost *dragonboat.NodeHost
	Ctx      context.Context
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
	switch cfg.Storage.Type {
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

	A = a

	return a, nil
}

// StartRaft starts the Dragonboat NodeHost and both Raft groups.
//
// Must be called after WithRepositories() so the EventRepository is fully
// initialised before the state machine factory captures it.
//
// Starts two Raft groups:
//   1. Event shard (config.Raft.ClusterID) — replicates event data
//   2. Metadata shard (metadata.MetadataShardID) — replicates cluster topology
//
// join controls Dragonboot bootstrap semantics:
//   - false: bootstrap a new cluster using config.Raft.InitialMembers, or
//     restart from local data when members are empty.
//   - true: join an existing cluster as an already-registered member
//     (initialMembers must be empty; membership was registered via JoinCluster).
//
// onDeleteKeys is called by the state machine after a DeleteBatchCmd is applied.
// Wire this to Dispatcher.RemoveInFlightBatch in start.go.
func (a *App) StartRaft(join bool, onDeleteKeys func(keys [][]byte)) error {
	cfg := a.cfg

	// Create the metadata service first — it needs to be registered as the
	// event listener before NodeHost is created.
	var metadataSvc *metadata.Service

	nhc := raftconfig.NodeHostConfig{
		WALDir:         cfg.Raft.DataPath,
		NodeHostDir:    cfg.Raft.DataPath,
		RTTMillisecond: cfg.Raft.RTTMillisecond,
		RaftAddress:    cfg.Raft.ListenAddress,
		// We'll set the listeners after creating the service below.
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

	metadataSvc = metadata.NewService(nil, proposeMetadata, a.Logger)
	nhc.RaftEventListener = metadataSvc
	nhc.SystemEventListener = metadataSvc

	var err error
	nh, err = dragonboat.NewNodeHost(nhc)
	if err != nil {
		return fmt.Errorf("failed to create dragonboat nodehost: %w", err)
	}

	a.NodeHost = nh
	a.MetadataSvc = metadataSvc
	metadataSvc.SetNodeHost(nh)

	// Members are only passed when bootstrapping a brand-new cluster.
	// Dragonboot semantics:
	//   - Fresh bootstrap: join=false + initialMembers populated
	//   - Fresh join:      join=true  + empty members (registered via JoinCluster)
	//   - Restart:         join=false + empty members (local data exists)
	members := make(map[uint64]dragonboat.Target)
	if !join && !a.hasRaftData() {
		for k, v := range cfg.Raft.InitialMembers {
			members[k] = dragonboat.Target(v)
		}
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
		ReplicaID:          cfg.Raft.NodeID,
		ShardID:            metadata.MetadataShardID,
		ElectionRTT:        10,
		HeartbeatRTT:       1,
		CheckQuorum:        true,
		SnapshotEntries:    5,
		CompactionOverhead: 5, // compact aggressively since state is small
	}

	if err := nh.StartReplica(members, join, metadataFactory, metadataRC); err != nil {
		// Dragonboat panics internally on ErrShardNotBootstrapped (the "restarted
		// during a previous bootstrap attempt" case). The panic is recovered by
		// dragonboat's own handler and returned here as an error. When we hit it,
		// the LogDB is in a half-initialised state that hasRaftData() would treat
		// as "existing cluster data" on the next restart, causing an infinite
		// crash loop. Wipe the dir so the next start begins from a clean slate.
		if errors.Is(err, dragonboat.ErrShardNotBootstrapped) {
			a.Logger.Warn("raft bootstrap failed (shard not bootstrapped); wiping raft data dir to allow clean retry",
				zap.String("path", cfg.Raft.DataPath))
			_ = os.RemoveAll(cfg.Raft.DataPath)
		}
		return fmt.Errorf("failed to start metadata raft group: %w", err)
	}

	a.MetadataSM = capturedSM

	// Wire the gRPC address registry into the metadata service so published
	// topologies include client-facing addresses.
	metadataSvc.SetGrpcAddrsSource(capturedSM.GetGrpcAddrs)

	// ── Start the event Raft group ─────────────────────────────────────────────
	eventRC := raftconfig.Config{
		ReplicaID:          cfg.Raft.NodeID,
		ShardID:            cfg.Raft.ClusterID,
		ElectionRTT:        10,
		HeartbeatRTT:       1,
		CheckQuorum:        true,
		SnapshotEntries:    cfg.Raft.SnapshotEntries,
		CompactionOverhead: cfg.Raft.CompactionOverhead,
	}

	// Pass the fully-initialised EventRepository so the state machine uses the
	// same monotonic ID counter and key schema as the standalone write path.
	eventFactory := raft.NewEventStateMachineFactory(a.DB, a.Repositories.Events, onDeleteKeys, a.Logger)
	if err := nh.StartOnDiskReplica(members, join, eventFactory, eventRC); err != nil {
		if errors.Is(err, dragonboat.ErrShardNotBootstrapped) {
			a.Logger.Warn("event raft bootstrap failed (shard not bootstrapped); wiping raft data dir to allow clean retry",
				zap.String("path", cfg.Raft.DataPath))
			_ = os.RemoveAll(cfg.Raft.DataPath)
		}
		return fmt.Errorf("failed to start event raft group: %w", err)
	}

	// ── Announce our gRPC address to the cluster ──────────────────────────────
	// Derive the advertise address from our Raft address (the identity the
	// cluster knows us by) plus the gRPC port from Server.Listen. Both run
	// in the same process, so the host is always the same.
	grpcAdvertise, err := grpcAdvertiseAddr(nh.RaftAddress(), cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("failed to compute gRPC advertise address: %w", err)
	}
	// Retry until the metadata shard has a leader. During initial cluster
	// bootstrap (OrderedReady), the first pod starts before peers exist, so
	// the shard may not be ready yet. Keep retrying until it is.
	for {
		ctx, cancel := context.WithTimeout(a.Ctx, 5*time.Second)
		err := metadataSvc.RegisterNodeAddr(ctx, cfg.Raft.NodeID, grpcAdvertise)
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
	a.MetadataSvc.RegisterShard(cfg.Raft.ClusterID)

	return nil
}

// grpcAdvertiseAddr computes the client-facing gRPC address for this node.
// The host is taken from raftAddr (this node's identity as the cluster sees
// it — always dialable by peers), and the port from grpcListen.
func grpcAdvertiseAddr(raftAddr, grpcListen string) (string, error) {
	host, _, err := net.SplitHostPort(raftAddr)
	if err != nil {
		return "", fmt.Errorf("invalid raft address %q: %w", raftAddr, err)
	}
	_, grpcPort, err := net.SplitHostPort(grpcListen)
	if err != nil {
		return "", fmt.Errorf("invalid grpc listen address %q: %w", grpcListen, err)
	}
	return net.JoinHostPort(host, grpcPort), nil
}

// HasRaftData reports whether local Raft data exists for this node.
// Used by the start command to distinguish a restart from a fresh join.
func (a *App) HasRaftData() bool {
	return a.hasRaftData()
}

// hasRaftData returns true if the Raft data directory contains a previously
// bootstrapped LogDB — meaning this node has been part of a cluster before.
// A bare directory (created by a failed first bootstrap attempt) is NOT
// treated as existing data; Dragonboat will retry the bootstrap from scratch.
func (a *App) hasRaftData() bool {
	// Dragonboat's sharded-pebble LogDB stores each shard in a logdb-N
	// subdirectory. The first shard writes a MANIFEST once initialised.
	manifest := filepath.Join(a.cfg.Raft.DataPath, "logdb-0", "MANIFEST-000001")
	_, err := os.Stat(manifest)
	return err == nil
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
	eventRepo, err := repository.NewEventRepository(a.DB, a.Logger, a.cfg.Storage.TimeBucketSize)
	if err != nil {
		return fmt.Errorf("failed to init event repo: %w", err)
	}

	a.Repositories.Events = eventRepo

	return nil
}
