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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	grpcserver "github.com/futureq-io/futureq/internal/api/grpc"
	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/dispatcher"
	"github.com/futureq-io/futureq/internal/metrics"
	"github.com/futureq-io/futureq/pkg/log"
	pb "github.com/futureq-io/protocol/proto/go"
)

var joinSeeds []string

// startCmd represents the server command
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the FutureQ broker",
	Long: `Start the FutureQ broker.

To join an existing cluster, pass one or more seed addresses:
  futureq start -c node2.yaml --join 10.0.0.1:8443 --join 10.0.0.2:8443

On first start, the node contacts each seed in order until one accepts
its JoinCluster request. Membership is registered on both the event
shard and the metadata group. Subsequent restarts skip the join flow
automatically (local Raft data is detected).`,
	Run: startRun,
}

func init() {
	startCmd.Flags().StringSliceVar(&joinSeeds, "join", nil, "gRPC addresses of seed nodes to join (repeatable)")
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

	// ── Initialise app: storage + repository ───────────────────────────────────
	a, err := app.Init(cfg, logger)
	if err != nil {
		logger.Fatal("failed to init app", zap.Error(err))
	}

	if err := a.WithRepositories(); err != nil {
		logger.Fatal("failed to init repositories", zap.Error(err))
	}

	// ── Join an existing cluster if requested ────────────────────────────────
	// Only performed on a fresh node (no local Raft data). Restarts detect
	// the existing data and skip the join flow entirely.
	joining := false
	if cfg.Raft.Enabled && len(joinSeeds) > 0 {
		if a.HasRaftData() {
			logger.Info("local raft data found, skipping join flow")
		} else {
			joinCluster(cfg, joinSeeds, logger)
			joining = true
		}
	}

	// ── Dispatcher components ─────────────────────────────────────────────────
	wakeCh := make(chan struct{}, 1)
	strategy := dispatcher.NewRoundRobinStrategy()
	hub := dispatcher.NewHub(strategy, logger, wakeCh)

	inFlightTimeout := time.Duration(cfg.Consumer.InFlightTimeoutMs) * time.Millisecond
	deleteInterval := time.Duration(cfg.Consumer.DeleteBatchIntervalMs) * time.Millisecond
	dispatchInterval := time.Duration(cfg.Consumer.DispatchPollIntervalMs) * time.Millisecond
	janitorInterval := time.Duration(cfg.Consumer.TTLJanitorIntervalMs) * time.Millisecond

	// ── Build the delete backend ────────────────────────────────────────────────
	// In Raft mode: route deletions through SyncPropose(DeleteBatchCmd).
	// In standalone mode: write deletions directly to the local storage engine.
	var deleteBackend dispatcher.DeleteBackend
	if cfg.Raft.Enabled {
		proposeDelete := func(cmd []byte) error {
			ctx, cancel := context.WithTimeout(a.Ctx, 5*time.Second)
			defer cancel()
			session := a.NodeHost.GetNoOPSession(cfg.Raft.ClusterID)
			_, err := a.NodeHost.SyncPropose(ctx, session, cmd)
			return err
		}
		deleteBackend = dispatcher.NewRaftDeleteBackend(proposeDelete, logger)
	} else {
		deleteBackend = dispatcher.NewDirectDeleteBackend(a.DB, logger)
	}

	deleter := dispatcher.NewDeleter(deleteBackend, deleteInterval, logger)
	disp := dispatcher.NewDispatcher(
		a.DB, hub, deleter,
		dispatchInterval, inFlightTimeout,
		wakeCh, logger,
	)

	// Wire the OnDelete callback so the deleter notifies the dispatcher when
	// a delete completes — removes the key from the in-flight tracker.
	deleter.OnDelete = func(key []byte) {
		disp.RemoveInFlight(key)
	}

	// ── Start Raft (must be after WithRepositories so the repo is ready) ──────
	// onDeleteKeys is called by the state machine after each DeleteBatchCmd
	// is committed. We wire it to the dispatcher so in-flight entries are
	// removed immediately without waiting for the next scan pass.
	if cfg.Raft.Enabled {
		if err := a.StartRaft(joining, disp.RemoveInFlightBatch); err != nil {
			logger.Fatal("failed to start raft", zap.Error(err))
		}
	}

	// ── TTL Janitor ───────────────────────────────────────────────────────────
	janitor := dispatcher.NewTTLJanitor(a.DB, deleter, janitorInterval, logger)

	// ── Prometheus metrics server ──────────────────────────────────────────────
	metricsSrv := metrics.NewServer(cfg.Observability.Metrics.Addr, logger)

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

	// ── gRPC server ───────────────────────────────────────────────────────────
	grpcserver.New(cfg.Server, hub, deleter, logger).
		Listen().
		WaitForShutdown(a.Ctx)

	// ── Block until SIGTERM / SIGINT ──────────────────────────────────────────
	if err := a.WithGracefulShutdown(); err != nil {
		logger.Fatal("failed to graceful shutdown", zap.Error(err))
	}
}

// joinCluster contacts each seed in order until one accepts this node's
// JoinCluster request. Membership is registered on both the event shard
// and the metadata group by the seed.
func joinCluster(cfg *config.Config, seeds []string, logger *zap.Logger) {
	req := &pb.JoinRequest{
		NodeId:      cfg.Raft.NodeID,
		RaftAddress: cfg.Raft.ListenAddress,
		GrpcAddress: cfg.Server.Listen,
	}

	for _, seed := range seeds {
		logger.Info("attempting to join cluster via seed",
			zap.String("seed", seed),
			zap.Uint64("node_id", cfg.Raft.NodeID),
		)

		conn, err := grpc.NewClient(seed, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			logger.Warn("failed to connect to seed", zap.String("seed", seed), zap.Error(err))
			continue
		}

		client := pb.NewFutureQClusterClient(conn)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		resp, err := client.JoinCluster(ctx, req)
		cancel()
		_ = conn.Close()

		if err != nil {
			logger.Warn("JoinCluster RPC failed",
				zap.String("seed", seed),
				zap.Error(err),
			)
			continue
		}
		if !resp.Success {
			logger.Warn("seed rejected join",
				zap.String("seed", seed),
				zap.String("error", resp.ErrorMessage),
			)
			continue
		}

		logger.Info("successfully joined cluster",
			zap.String("seed", seed),
			zap.Uint64("node_id", cfg.Raft.NodeID),
		)
		return
	}

	logger.Fatal("failed to join cluster: all seeds exhausted",
		zap.Strings("seeds", seeds),
	)
}
