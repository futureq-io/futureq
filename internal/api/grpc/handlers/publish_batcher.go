package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type queuedPublish struct {
	ctx     context.Context
	done    chan error // buffered so cancellation never blocks a worker
	bytes   int
	count   int
	command []byte
	items   []raft.StoreBatchItem
}

var deliveryStateKeyOverhead = len(raft.DeliveryStateKey(nil))

type publishBatcher struct {
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	// The lane is the requested ACK level in a cluster. Standalone flushes
	// share a lane because every ACK mode uses the same synchronous commit.
	lanes [3]chan *queuedPublish
	slots [3]chan struct{} // bound receipt reads and encoded commands awaiting handoff
}

func newPublishBatcher(cfg config.Publish, clustered bool, flush func(context.Context, pb.AckLevel, []*queuedPublish)) *publishBatcher {
	ctx, cancel := context.WithCancel(context.Background())
	b := &publishBatcher{ctx: ctx, cancel: cancel}
	maxBytes, _ := cfg.BatchMaxBytes.Bytes() // configuration validated at startup
	for i := range b.lanes {
		if i > 0 && !clustered {
			break
		}
		lane := make(chan *queuedPublish)
		b.lanes[i] = lane
		b.slots[i] = make(chan struct{}, cfg.BatchMaxRequests)
		b.workers.Go(func() {
			b.runLane(lane, pb.AckLevel(i), cfg.BatchMaxRequests, int(maxBytes), cfg.BatchMaxWait, flush)
		})
	}
	return b
}

// reserve leaves requests waiting for capacity in their own gRPC handler,
// before allocating encoded commands or reading subscription receipts.
func (b *publishBatcher) reserve(ctx context.Context, level pb.AckLevel) (func(), error) {
	slots := b.slots[level]
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-b.ctx.Done():
		return nil, status.Error(codes.Unavailable, "publish batcher stopping")
	}
}

func (b *publishBatcher) runLane(lane <-chan *queuedPublish, level pb.AckLevel, maxRequests, maxBytes int, maxWait time.Duration, flush func(context.Context, pb.AckLevel, []*queuedPublish)) {
	group := make([]*queuedPublish, 0, maxRequests)
	var next *queuedPublish
	for b.ctx.Err() == nil {
		if next == nil {
			select {
			case next = <-lane:
			case <-b.ctx.Done():
				return
			}
		}
		if next.ctx.Err() != nil {
			next = nil
			continue
		}
		group = append(group[:0], next)
		bytes := 1 + next.bytes // includes the Raft group tag when encoded
		next = nil
		if bytes < maxBytes {
			timer := time.NewTimer(maxWait)
		collect:
			for len(group) < maxRequests && bytes < maxBytes {
				select {
				case req := <-lane:
					if req.ctx.Err() != nil {
						continue
					}
					if req.bytes > maxBytes-bytes {
						next = req
						break collect
					}
					group = append(group, req)
					bytes += req.bytes
				case <-timer.C:
					break collect
				case <-b.ctx.Done():
					timer.Stop()
					return
				}
			}
			timer.Stop()
		}
		active := group[:0]
		for _, req := range group {
			if req.ctx.Err() == nil {
				active = append(active, req)
			}
		}
		if len(active) > 0 && b.ctx.Err() == nil {
			flush(b.ctx, level, active)
		}
		clear(group)
	}
}

func (ph *ProducerHandler) queuePublish(ctx context.Context, batch *pb.PublishBatch, nowMs int64, level, lane pb.AckLevel, receipts map[string][]byte) error {
	req := &queuedPublish{ctx: ctx, done: make(chan error, 1), count: len(batch.Messages)}
	var err error
	if app.A.NodeHost != nil {
		if err := checkPublishLeader(); err != nil {
			return err
		}
		req.command, err = ph.buildPublishCommand(batch, nowMs, level, receipts)
		req.bytes = len(req.command) + raft.PublishGroupFrameOverhead
	} else {
		req.items, err = ph.buildStoreItems(batch, nowMs, receipts, false)
		for _, item := range req.items {
			req.bytes += len(item.Msg) + len(item.Receipt) + 2*utils.EventKeySize + deliveryStateKeyOverhead
			for _, index := range item.Indexes {
				req.bytes += len(index) + utils.EventKeySize
			}
		}
	}
	if err != nil {
		return err
	}
	b := ph.batcher
	select {
	case b.lanes[lane] <- req:
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-b.ctx.Done():
		return status.Error(codes.Unavailable, "publish batcher stopping")
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-b.ctx.Done():
		return status.Error(codes.Unavailable, "publish batcher stopping")
	}
}

func (ph *ProducerHandler) flushPublishGroup(ctx context.Context, level pb.AckLevel, group []*queuedPublish) {
	replyAll := func(err error) {
		for _, req := range group {
			req.done <- err
		}
	}
	if app.A.NodeHost == nil {
		replyAll(ph.commitPublishGroup(group))
		return
	}
	if err := checkPublishLeader(); err != nil {
		replyAll(err)
		return
	}

	command := group[0].command
	if len(group) > 1 {
		commands := make([][]byte, len(group))
		for i, req := range group {
			commands[i] = req.command
		}
		var err error
		command, err = raft.MarshalPublishGroupCmd(commands)
		if err != nil {
			replyAll(err)
			return
		}
	}
	result, err := ph.proposePublish(ctx, level, command)
	if err != nil || level != pb.AckLevel_ACK_LEVEL_QUORUM {
		replyAll(publishProposalError(err))
		return
	}
	if len(group) == 1 {
		replyAll(publishProposalError(raft.CheckPublishResult(result, group[0].count)))
		return
	}
	results, err := raft.UnmarshalPublishGroupResults(result, len(group))
	if err != nil {
		replyAll(publishProposalError(err))
		return
	}
	for i, req := range group {
		req.done <- publishProposalError(raft.CheckPublishResult(results[i], req.count))
	}
}

func (ph *ProducerHandler) commitPublishGroup(group []*queuedPublish) error {
	batch := app.A.DB.NewBatch()
	defer batch.Close() //nolint:errcheck
	repo := app.A.Repositories.Events
	var lastID uint64
	for _, req := range group {
		for _, item := range req.items {
			lastID = repo.NextID()
			key, err := repo.StoreRawWithBatch(batch, lastID, utils.ShardStorageBucket(0, item.Bucket), item.TopicHash, item.Indexes, item.Msg)
			if err != nil {
				return err
			}
			if err := batch.Set(raft.DeliveryStateKey(key), item.Receipt); err != nil {
				return err
			}
		}
	}
	if err := repo.PersistLastID(batch, lastID); err != nil {
		return err
	}
	if err := batch.Commit(storage.Sync); err != nil {
		ph.logger.Error("failed to commit standalone publish group", zap.Error(err))
		return errBatchSave
	}
	return nil
}
