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

	"github.com/lni/dragonboat/v4"
	raftconfig "github.com/lni/dragonboat/v4/config"
	"go.uber.org/zap"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
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

// StartRaft starts the Dragonboat NodeHost and the on-disk Raft replica.
//
// Must be called after WithRepositories() so the EventRepository is fully
// initialised before the state machine factory captures it.
//
// onDeleteKeys is called by the state machine after a DeleteBatchCmd is applied.
// Wire this to Dispatcher.RemoveInFlightBatch in start.go.
func (a *App) StartRaft(onDeleteKeys func(keys [][]byte)) error {
	cfg := a.cfg

	nhc := raftconfig.NodeHostConfig{
		WALDir:         cfg.Raft.DataPath,
		NodeHostDir:    cfg.Raft.DataPath,
		RTTMillisecond: cfg.Raft.RTTMillisecond,
		RaftAddress:    cfg.Raft.ListenAddress,
	}

	nh, err := dragonboat.NewNodeHost(nhc)
	if err != nil {
		return fmt.Errorf("failed to create dragonboat nodehost: %w", err)
	}

	a.NodeHost = nh

	rc := raftconfig.Config{
		ReplicaID:          cfg.Raft.NodeID,
		ShardID:            cfg.Raft.ClusterID,
		ElectionRTT:        10,
		HeartbeatRTT:       1,
		CheckQuorum:        true,
		SnapshotEntries:    cfg.Raft.SnapshotEntries,
		CompactionOverhead: cfg.Raft.CompactionOverhead,
	}

	members := make(map[uint64]dragonboat.Target)
	for k, v := range cfg.Raft.InitialMembers {
		members[k] = dragonboat.Target(v)
	}

	// Pass the fully-initialised EventRepository so the state machine uses the
	// same monotonic ID counter and key schema as the standalone write path.
	factory := raft.NewEventStateMachineFactory(a.DB, a.Repositories.Events, onDeleteKeys, a.Logger)
	if err := nh.StartOnDiskReplica(members, false, factory, rc); err != nil {
		return fmt.Errorf("failed to start raft cluster: %w", err)
	}

	return nil
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
