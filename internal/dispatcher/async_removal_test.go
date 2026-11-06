package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/stretchr/testify/require"
)

func TestAsyncConsumerRemovalPreservesReplacementAndRebalances(t *testing.T) {
	c := newReplicaTestCluster(t)
	c.subscribe(1, "old-stream", "workers")
	c.subscribe(2, "replacement-stream", "workers")
	c.activate()
	n := c.nodes[1]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.svc.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	require.NoError(t, n.hub.WithConsumerRegistry(func() error {
		n.hub.Unregister("old-stream")
		n.svc.RemoveConsumerAsync(replicaTestShard, "orders", "workers", metadata.ConsumerMember{ID: "old-stream", NodeID: 1})
		return nil
	}))
	require.Eventually(t, func() bool {
		return !n.sm.ConsumerPresent("orders", "workers", "old-stream")
	}, 3*time.Second, time.Millisecond)
	c.activate()
	for _, node := range c.nodes {
		require.False(t, node.sm.ConsumerPresent("orders", "workers", "old-stream"))
		require.True(t, node.sm.ConsumerPresent("orders", "workers", "replacement-stream"))
		_, active := node.sm.ConsumerVersion()
		require.True(t, active)
	}
}
