/*
Copyright © 2025 FutureQ Authors
*/
package main

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
	"github.com/lni/dragonboat/v4/statemachine"
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

func startRun(cmd *cobra.Command, _ []string) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		stdLogger.Fatalf("failed to load config: %v", err)
	}

	logger, err := log.InitLogger(cfg.Observability.Logging)
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
	seeds := cfg.Cluster.JoinSeeds
	if cmd.Flags().Changed("join") {
		seeds = joinSeeds
	}
	joining := false
	if cfg.Cluster.Enabled && len(seeds) > 0 {
		if a.HasRaftData() {
			logger.Info("local raft data found, skipping join flow")
		} else {
			joinCluster(cfg, seeds, logger)
			joining = true
		}
	}
	if cfg.Cluster.Enabled && !a.HasRaftData() && !joining && len(cfg.Cluster.Raft.InitialMembers) == 0 {
		logger.Fatal("fresh cluster node requires cluster.raft.initialMembers or cluster.joinSeeds")
	}

	// ── Dispatcher components ─────────────────────────────────────────────────
	wakeCh := make(chan struct{}, 1)
	strategy := dispatcher.NewRoundRobinStrategy()
	hub := dispatcher.NewHub(strategy, logger, wakeCh)

	inFlightTimeout := cfg.Delivery.InFlightTimeout
	deleteInterval := cfg.Delivery.DeleteBatchInterval
	dispatchInterval := cfg.Delivery.DispatchPollInterval
	janitorInterval := cfg.Delivery.TTLSweepInterval

	// ── Build the delete backend ────────────────────────────────────────────────
	// In Raft mode: route deletions through SyncPropose(DeleteBatchCmd).
	// In standalone mode: write deletions directly to the local storage engine.
	var deleteBackend dispatcher.DeleteBackend
	if cfg.Cluster.Enabled {
		proposeDelete := func(cmd []byte) error {
			ctx, cancel := context.WithTimeout(a.Ctx, cfg.Publish.ProposalTimeout)
			defer cancel()
			session := a.NodeHost.GetNoOPSession(cfg.Cluster.ShardID)
			_, err := a.NodeHost.SyncPropose(ctx, session, cmd)
			return err
		}
		deleteBackend = dispatcher.NewRaftDeleteBackend(proposeDelete, logger)
	} else {
		deleteBackend = dispatcher.NewDirectDeleteBackend(a.DB, logger)
	}

	var proposeDelivery func([]byte) (statemachine.Result, error)
	recipients := hub.LocalDeliveryRecipients
	if cfg.Cluster.Enabled {
		recipients = func(topic string) (uint64, []string, bool) {
			return a.MetadataSM.DeliveryRecipients(topic)
		}
		proposeDelivery = func(cmd []byte) (statemachine.Result, error) {
			ctx, cancel := context.WithTimeout(a.Ctx, cfg.Publish.ProposalTimeout)
			defer cancel()
			return a.NodeHost.SyncPropose(ctx, a.NodeHost.GetNoOPSession(cfg.Cluster.ShardID), cmd)
		}
	}
	ledger := dispatcher.NewDeliveryLedger(a.DB, recipients, deleteBackend, proposeDelivery)
	if cfg.Cluster.Enabled {
		ledger.ProposeContext = func(ctx context.Context, cmd []byte) (statemachine.Result, error) {
			return a.NodeHost.SyncPropose(ctx, a.NodeHost.GetNoOPSession(cfg.Cluster.ShardID), cmd)
		}
	}
	deleter := dispatcher.NewDeleter(ledger, deleteInterval, logger)
	deleter.AcknowledgeBatch = ledger.Acknowledge
	disp := dispatcher.NewDispatcher(
		a.DB, hub, deleter,
		dispatchInterval, inFlightTimeout,
		wakeCh, logger,
	)
	disp.PrepareBatch = ledger.PrepareBatch
	disp.MaintenanceRecipients = recipients
	disp.TimeBucket = cfg.Delivery.TimeBucket
	prepareBytes, _ := cfg.Delivery.PrepareBatchBytes.Bytes() // validated during Load
	disp.Limits = dispatcher.ScanLimits{
		Candidates: cfg.Delivery.PrepareBatchSize, Examined: cfg.Delivery.ScanMaxKeys,
		Bytes: int(prepareBytes), WorkTime: cfg.Delivery.ScanWorkBudget,
		PrepareTimeout: cfg.Delivery.PrepareTimeout, Workers: cfg.Delivery.PrepareWorkers,
	}
	ledger.OnDelete = func(keys [][]byte) {
		disp.RemoveInFlightBatch(keys)
		hub.RemoveDeletedBatch(keys)
	}

	// Wire the OnDelete callback so the deleter notifies the dispatcher when
	// a delete completes — removes the key from the in-flight tracker.
	deleter.OnDelete = func(key []byte) {
		disp.RemoveInFlight(key)
		hub.RemoveDeletedBatch([][]byte{key})
	}
	hub.OnNack = disp.RemoveInFlight

	// ── Prometheus metrics server ──────────────────────────────────────────────
	// Start before Raft so the liveness/readiness probes have something to hit
	// while the Raft cluster is still forming.
	metricsSrv := metrics.NewServer(cfg.Observability.Metrics.Listen, logger)
	a.RegisterComponentWithShutdown()
	go func() {
		defer a.ComponentShutdownDone()
		metricsSrv.Run(a.Ctx)
	}()

	// ── Start Raft (must be after WithRepositories so the repo is ready) ──────
	// onDeleteKeys is called by the state machine after each DeleteBatchCmd
	// is committed. We wire it to the dispatcher so in-flight entries are
	// removed immediately without waiting for the next scan pass.
	if cfg.Cluster.Enabled {
		if err := a.StartRaft(joining, func(keys [][]byte) {
			disp.RemoveInFlightBatch(keys)
			hub.RemoveDeletedBatch(keys)
		}); err != nil {
			logger.Fatal("failed to start raft", zap.Error(err))
		}
		hub.SetGroupMembership(a.MetadataSM.ConsumerGroup)
		hub.SetDeliveryView(a.MetadataSM.TopicDeliverySnapshot)
		hub.SetDeliveryFence(a.MetadataSM.ConsumerVersion, inFlightTimeout)
		a.MetadataSM.SetConsumerBarrier(hub.WithRebalanceBarrier)
		disp.ReadBarrier = dispatcher.NewConsumerReadBarrier(a.Ctx, a.NodeHost, a.MetadataSM, hub, cfg.Cluster.ShardID)
		// Clear subscriptions from an earlier process before this node serves
		// new streams. A crash cannot run the stream's unregister defer.
		ctx, cancel := context.WithTimeout(a.Ctx, 10*time.Second)
		if err := a.MetadataSvc.ReconcileNodeConsumers(ctx, cfg.Cluster.ShardID, cfg.Cluster.NodeID, nil); err != nil {
			logger.Warn("initial consumer reconciliation failed", zap.Error(err))
		}
		cancel()
		a.RegisterComponentWithShutdown()
		go func() {
			defer a.ComponentShutdownDone()
			consumerMetadataLoop(a.Ctx, a, hub, logger)
		}()
	}

	// ── TTL Janitor ───────────────────────────────────────────────────────────
	janitor := dispatcher.NewTTLJanitor(a.DB, deleter, janitorInterval, logger)

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

	// ── gRPC server ───────────────────────────────────────────────────────────
	grpcserver.New(cfg.API.GRPC, hub, deleter, logger).
		Listen().
		WaitForShutdown(a.Ctx)

	// ── Block until SIGTERM / SIGINT ──────────────────────────────────────────
	if err := a.WithGracefulShutdown(); err != nil {
		logger.Fatal("failed to graceful shutdown", zap.Error(err))
	}
}

// consumerMetadataLoop drains deliveries, reconciles subscriptions, and fences
// unavailable replicas after their bounded delivery authorization expires.
func consumerMetadataLoop(ctx context.Context, a *app.App, hub *dispatcher.Hub, logger *zap.Logger) {
	coordinator := dispatcher.NewConsumerCoordinator(hub, a.MetadataSM, a.MetadataSvc,
		a.Config().Cluster.ShardID, a.Config().Cluster.NodeID, a.Config().Delivery.InFlightTimeout, logger)
	coordinator.Run(ctx)
}

// joinCluster contacts each seed in order until one accepts this node's
// JoinCluster request. Membership is registered on both the event shard
// and the metadata group by the seed.
func joinCluster(cfg *config.Config, seeds []string, logger *zap.Logger) {
	req := &pb.JoinRequest{
		NodeId:      cfg.Cluster.NodeID,
		RaftAddress: cfg.Cluster.Raft.Advertise,
		GrpcAddress: cfg.API.GRPC.Advertise,
	}

	for _, seed := range seeds {
		logger.Info("attempting to join cluster via seed",
			zap.String("seed", seed),
			zap.Uint64("node_id", cfg.Cluster.NodeID),
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
			zap.Uint64("node_id", cfg.Cluster.NodeID),
		)
		return
	}

	logger.Fatal("failed to join cluster: all seeds exhausted",
		zap.Strings("seeds", seeds),
	)
}
