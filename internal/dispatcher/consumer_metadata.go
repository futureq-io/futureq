package dispatcher

import (
	"context"
	"slices"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"go.uber.org/zap"
)

// ConsumerCoordinator drains local deliveries and activates assignments even
// when an event replica is unavailable, after its delivery permit expires.
type ConsumerCoordinator struct {
	hub           *Hub
	sm            *metadata.MetadataStateMachine
	svc           *metadata.Service
	shardID       uint64
	nodeID        uint64
	timeout       time.Duration
	logger        *zap.Logger
	drain         RebalanceDrain
	lastReconcile time.Time
	lastIDs       []string
	reconciled    bool
}

func NewConsumerCoordinator(hub *Hub, sm *metadata.MetadataStateMachine, svc *metadata.Service, shardID, nodeID uint64, timeout time.Duration, logger *zap.Logger) *ConsumerCoordinator {
	return &ConsumerCoordinator{hub: hub, sm: sm, svc: svc, shardID: shardID, nodeID: nodeID, timeout: timeout, logger: logger}
}

func (c *ConsumerCoordinator) Run(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if err := c.Tick(callCtx, now); err != nil && ctx.Err() == nil {
				c.logger.Warn("consumer metadata maintenance failed", zap.Error(err))
			}
			cancel()
		}
	}
}

// Tick expires deliveries and maintains membership using the same path in the
// broker loop and cluster tests. The fence clock starts after observing metadata.
func (c *ConsumerCoordinator) Tick(ctx context.Context, now time.Time) error {
	c.hub.ExpireInFlight(now)
	if err := c.svc.SyncConsumers(ctx); err != nil {
		return err // a minority partition must not activate or restore streams
	}
	if err := c.hub.WithConsumerRegistry(func() error {
		for _, consumer := range c.hub.LocalConsumers() {
			if c.sm.ConsumerPresent(consumer.Topic, consumer.Group, consumer.ID) {
				continue
			}
			member := metadata.ConsumerMember{ID: consumer.ID, NodeID: c.nodeID}
			if err := c.svc.ChangeConsumer(ctx, c.shardID, consumer.Topic, consumer.Group, member, true); err != nil {
				return err
			}
		}
		if now.Sub(c.lastReconcile) >= 5*time.Second {
			ids := c.hub.LocalConsumerIDs()
			if !c.reconciled || !slices.Equal(ids, c.lastIDs) {
				if err := c.svc.ReconcileNodeConsumers(ctx, c.shardID, c.nodeID, ids); err != nil {
					c.reconciled = false
					return err
				}
				c.lastIDs, c.reconciled = ids, true
			}
			c.lastReconcile = now
		}
		return nil
	}); err != nil {
		return err
	}
	epoch, pending, acked := c.sm.ConsumerStatus(c.nodeID)
	// A slow quorum read can observe a freeze committed after this tick started.
	// Counting that read's duration would fence an old permit prematurely.
	expired := c.drain.Expired(epoch, pending, time.Now(), c.timeout)
	if pending && !acked && c.hub.GroupInFlightCount() == 0 {
		return c.svc.AcknowledgeConsumers(ctx, epoch, c.nodeID)
	}
	if expired {
		return c.svc.FenceConsumers(ctx, epoch)
	}
	return nil
}
