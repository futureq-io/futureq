package cmd

import (
	"context"
	stdLogger "log"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/futureq-io/futureq/internal/config"
	pb "github.com/futureq-io/protocol/proto/go"
)

var seedAddr string

// joinCmd represents the join command
var joinCmd = &cobra.Command{
	Use:   "join",
	Short: "Join an existing FutureQ Raft cluster",
	Long: `Join an existing FutureQ cluster as a new Raft member.

The node first joins as a non-voting member to sync state from the leader,
then is automatically promoted to a full voting replica once caught up.

Example:
  futureq join --seed localhost:8443 --config node2.yaml`,
	Run: joinRun,
}

func init() {
	joinCmd.Flags().StringVarP(&cfgFile, "config", "c", "", "Path to config file")
	joinCmd.Flags().StringVar(&seedAddr, "seed", "", "gRPC address of a seed node in the cluster")
	_ = joinCmd.MarkFlagRequired("seed")

	rootCmd.AddCommand(joinCmd)
}

func joinRun(_ *cobra.Command, _ []string) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		stdLogger.Fatalf("failed to load config: %v", err)
	}

	if !cfg.Raft.Enabled {
		stdLogger.Fatalf("raft must be enabled in config to join a cluster")
	}

	conn, err := grpc.NewClient(seedAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		stdLogger.Fatalf("failed to connect to seed node: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	client := pb.NewFutureQClusterClient(conn)

	req := &pb.JoinRequest{
		NodeId:      cfg.Raft.NodeID,
		RaftAddress: cfg.Raft.ListenAddress,
		GrpcAddress: cfg.Server.Listen,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stdLogger.Printf("Joining cluster via seed node %s (node_id=%d)...", seedAddr, cfg.Raft.NodeID)
	resp, err := client.JoinCluster(ctx, req)
	if err != nil {
		stdLogger.Fatalf("JoinCluster RPC failed: %v", err)
	}

	if !resp.Success {
		stdLogger.Fatalf("failed to join cluster: %s", resp.ErrorMessage)
	}

	stdLogger.Printf("Successfully joined the cluster as node %d. You can now start the node.", cfg.Raft.NodeID)
}
