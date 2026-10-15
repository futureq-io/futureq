package dispatcher

import (
	"context"
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
	lastHeartbeat time.Time
	liveness      map[uint64]consumerHeartbeat
}

const consumerHeartbeatInterval = time.Second
const consumerHeartbeatTimeout = 3 * consumerHeartbeatInterval

type consumerHeartbeat struct {
	sequence uint64
	observed time.Time
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
	if time.Since(c.lastHeartbeat) >= consumerHeartbeatInterval {
		if err := c.svc.HeartbeatConsumers(ctx, c.shardID, c.nodeID); err != nil {
			return err
		}
		c.lastHeartbeat = time.Now()
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
			// An uncertain add/remove can change metadata without changing
			// this local snapshot. Always reconcile after the interval.
			if err := c.svc.ReconcileNodeConsumers(ctx, c.shardID, c.nodeID, ids); err != nil {
				return err
			}
			c.lastReconcile = now
		}
		return nil
	}); err != nil {
		return err
	}
	if epoch, suspect := c.observeHeartbeats(time.Now()); suspect {
		return c.svc.ProbeConsumers(ctx, epoch)
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

func (c *ConsumerCoordinator) observeHeartbeats(now time.Time) (uint64, bool) {
	epoch, pending, beats := c.sm.ConsumerHeartbeats()
	if c.liveness == nil {
		c.liveness = make(map[uint64]consumerHeartbeat)
	}
	for node := range c.liveness {
		if _, serving := beats[node]; !serving {
			delete(c.liveness, node)
		}
	}
	suspect := false
	for node, sequence := range beats {
		last, exists := c.liveness[node]
		if !exists || last.sequence != sequence {
			last = consumerHeartbeat{sequence: sequence, observed: now}
			c.liveness[node] = last
		}
		if now.Sub(last.observed) >= consumerHeartbeatTimeout {
			suspect = true
		}
	}
	return epoch, suspect && !pending
}
