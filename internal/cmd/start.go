/*
Copyright © 2025 FutureQ Authors
*/
package cmd

import (
	"context"
	stdLogger "log"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	grpcserver "github.com/futureq-io/futureq/internal/api/grpc"
	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/dispatcher"
	"github.com/futureq-io/futureq/internal/membership"
	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/pkg/log"
)

// startCmd represents the server command
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the FutureQ broker",
	Run:   startRun,
}

func startRun(_ *cobra.Command, _ []string) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		stdLogger.Fatalf("failed to load config: %v", err)
	}

	logger, err := log.InitLogger(cfg.Observability.Logger)
	if err != nil {
		stdLogger.Fatalf("failed to init logger: %v", err)
	}

	// ── Dispatcher components ─────────────────────────────────────────────────
	wakeCh := make(chan struct{}, 1)
	hub := dispatcher.NewHub(logger, wakeCh)

	inFlightTimeout := time.Duration(cfg.Consumer.InFlightTimeoutMs) * time.Millisecond
	deleteInterval := time.Duration(cfg.Consumer.DeleteBatchIntervalMs) * time.Millisecond
	dispatchInterval := time.Duration(cfg.Consumer.DispatchPollIntervalMs) * time.Millisecond
	janitorInterval := time.Duration(cfg.Consumer.TTLJanitorIntervalMs) * time.Millisecond

	// ── Initialise app: Pebble + repository ──────────────────────────────────
	a, err := app.Init(cfg, logger)
	if err != nil {
		logger.Fatal("failed to init app", zap.Error(err))
	}

	if err := a.WithRepositories(); err != nil {
		logger.Fatal("failed to init repositories", zap.Error(err))
	}

	// ── Build the Raft propose function for the Deleter ───────────────────────
	// In Raft mode: route deletions through SyncPropose(DeleteBatchCmd).
	// In standalone mode: nil → deleter writes directly to Pebble.
	var proposeDelete func(cmd []byte) error
	if cfg.Raft.Enabled {
		proposeDelete = func(cmd []byte) error {
			ctx, cancel := context.WithTimeout(a.Ctx, 5*time.Second)
			defer cancel()
			session := a.NodeHost.GetNoOPSession(cfg.Raft.ClusterID)
			_, err := a.NodeHost.SyncPropose(ctx, session, cmd)
			return err
		}
	}

	deleter := dispatcher.NewDeleter(a.DB, deleteInterval, proposeDelete, logger)
	disp := dispatcher.NewDispatcher(
		a.DB, hub, deleter,
		dispatchInterval, inFlightTimeout,
		wakeCh, logger,
	)

	// Wire the OnDelete callback so the deleter notifies the dispatcher when
	// a direct Pebble delete completes (single-node mode).
	deleter.OnDelete = func(key []byte) {
		disp.RemoveInFlight(key)
	}

	// ── Start Raft (must be after WithRepositories so the repo is ready) ──────
	// onDeleteKeys is called by the state machine after each DeleteBatchCmd
	// is committed. We wire it to the dispatcher so in-flight entries are
	// removed immediately without waiting for the next scan pass.
	if cfg.Raft.Enabled {
		if err := a.StartRaft(disp.RemoveInFlightBatch); err != nil {
			logger.Fatal("failed to start raft", zap.Error(err))
		}
	}


	// ── TTL Janitor ───────────────────────────────────────────────────────────
	janitor := dispatcher.NewTTLJanitor(a.DB, deleter, janitorInterval, logger)

	// ── Gossip membership (cluster mode only) ─────────────────────────────────
	var gossipManager *membership.Manager
	if cfg.Raft.Enabled && len(cfg.Cluster.GossipJoinPeers) > 0 || cfg.Cluster.GossipListenAddress != "" {
		gossipCfg := membership.Config{
			NodeID:      cfg.Raft.NodeID,
			BindAddress: cfg.Cluster.GossipListenAddress,
			GRPCAddress: cfg.Server.Listen,
			RaftAddress: cfg.Raft.ListenAddress,
			JoinPeers:   cfg.Cluster.GossipJoinPeers,
		}
		gm, err := membership.NewManager(gossipCfg, logger)
		if err != nil {
			logger.Warn("failed to start gossip membership; running without it", zap.Error(err))
		} else {
			gossipManager = gm
		}
	}

	// ── Prometheus metrics server ──────────────────────────────────────────────
	metricsSrv := metrics.NewServer(cfg.Cluster.MetricsListenAddress, logger)

	// ── Start background goroutines ───────────────────────────────────────────
	a.RegisterComponentWithShutdown()
	go func() {
		defer a.ComponentShutdownDone()
		deleter.Run(a.Ctx)
	}()

	a.RegisterComponentWithShutdown()
	go func() {
		defer a.ComponentShutdownDone()
		disp.Run(a.Ctx)
	}()

	a.RegisterComponentWithShutdown()
	go func() {
		defer a.ComponentShutdownDone()
		janitor.Run(a.Ctx)
	}()

	a.RegisterComponentWithShutdown()
	go func() {
		defer a.ComponentShutdownDone()
		metricsSrv.Run(a.Ctx)
	}()

	if gossipManager != nil {
		a.RegisterComponentWithShutdown()
		go func() {
			defer a.ComponentShutdownDone()
			<-a.Ctx.Done()
			_ = gossipManager.Leave(context.Background())
			_ = gossipManager.Shutdown()
		}()
	}

	// ── gRPC server ───────────────────────────────────────────────────────────
	grpcserver.New(cfg.Server, hub, deleter, gossipManager, logger).
		Listen().
		WaitForShutdown(a.Ctx)

	// ── Block until SIGTERM / SIGINT ──────────────────────────────────────────
	if err := a.WithGracefulShutdown(); err != nil {
		logger.Fatal("failed to graceful shutdown", zap.Error(err))
	}
}
