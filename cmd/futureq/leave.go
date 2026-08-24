package main

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

var leaveSeedAddr string

// leaveCmd represents the leave command
var leaveCmd = &cobra.Command{
	Use:   "leave",
	Short: "Gracefully remove a node from the FutureQ Raft cluster",
	Long: `Gracefully remove a node from the FutureQ cluster.

The node is removed from the Raft group's voting membership. After leaving,
the node's Raft data can be safely deleted.

Example:
  futureq leave --seed localhost:8443 --config node2.yaml`,
	Run: leaveRun,
}

func init() {
	leaveCmd.Flags().StringVar(&leaveSeedAddr, "seed", "", "gRPC address of a seed node in the cluster")
	_ = leaveCmd.MarkFlagRequired("seed")

	rootCmd.AddCommand(leaveCmd)
}

func leaveRun(_ *cobra.Command, _ []string) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		stdLogger.Fatalf("failed to load config: %v", err)
	}

	if !cfg.Raft.Enabled {
		stdLogger.Fatalf("raft must be enabled in config to leave a cluster")
	}

	conn, err := grpc.NewClient(leaveSeedAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		stdLogger.Fatalf("failed to connect to seed node: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	client := pb.NewFutureQClusterClient(conn)

	req := &pb.LeaveRequest{
		NodeId: cfg.Raft.NodeID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stdLogger.Printf("Requesting node %d to leave cluster via seed %s...", cfg.Raft.NodeID, leaveSeedAddr)
	resp, err := client.LeaveCluster(ctx, req)
	if err != nil {
		stdLogger.Fatalf("LeaveCluster RPC failed: %v", err)
	}

	if !resp.Success {
		stdLogger.Fatalf("failed to leave cluster: %s", resp.ErrorMessage)
	}

	stdLogger.Printf("Node %d successfully left the cluster. Raft data can now be safely deleted.", cfg.Raft.NodeID)
}
