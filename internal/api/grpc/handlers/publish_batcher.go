package handlers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type queuedPublish struct {
	ctx     context.Context
	done    chan error
	release func() // admission belongs to the worker after handoff
	bytes   int
	count   int
	frame   []byte
	items   []raft.StoreBatchItem
}

type publishBatcher struct {
	ctx      context.Context
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	flushes  sync.WaitGroup
	inFlight chan struct{} // shared across Raft ACK lanes
	lanes    [3]chan *queuedPublish
}

func newPublishBatcher(cfg config.Publish, clustered bool, flush func(context.Context, pb.AckLevel, []*queuedPublish)) *publishBatcher {
	ctx, cancel := context.WithCancel(context.Background())
	b := &publishBatcher{ctx: ctx, cancel: cancel}
	if clustered {
		b.inFlight = make(chan struct{}, cfg.BatchMaxInFlight)
	}
	maxBytes, _ := cfg.BatchMaxBytes.Bytes() // configuration validated at startup
	for i := range b.lanes {
		if i > 0 && !clustered {
			break
		}
		lane := make(chan *queuedPublish)
		b.lanes[i] = lane
		b.workers.Go(func() {
			b.runLane(lane, pb.AckLevel(i), cfg.BatchMaxRequests, int(maxBytes), cfg.BatchMaxWait, flush)
		})
	}
	return b
}

func (b *publishBatcher) queue(req *queuedPublish, level pb.AckLevel) (bool, error) {
	if b.inFlight == nil {
		level = pb.AckLevel_ACK_LEVEL_QUORUM
	}
	select {
	case b.lanes[level] <- req:
	case <-req.ctx.Done():
		return false, status.FromContextError(req.ctx.Err()).Err()
	case <-b.ctx.Done():
		return false, status.Error(codes.Unavailable, "publish batcher stopping")
	}
	select {
	case err := <-req.done:
		return true, err
	case <-req.ctx.Done():
		return true, status.FromContextError(req.ctx.Err()).Err()
	case <-b.ctx.Done():
		return true, status.Error(codes.Unavailable, "publish batcher stopping")
	}
}

func (b *publishBatcher) runLane(lane <-chan *queuedPublish, level pb.AckLevel, maxRequests, maxBytes int, maxWait time.Duration, flush func(context.Context, pb.AckLevel, []*queuedPublish)) {
	var next *queuedPublish
	defer func() {
		if next != nil {
			next.releaseAdmission()
		}
	}()
	for b.ctx.Err() == nil {
		if next == nil {
			select {
			case next = <-lane:
			case <-b.ctx.Done():
				return
			}
		}
		first := next
		next = nil
		if first.ctx.Err() != nil {
			first.releaseAdmission()
			continue
		}
		group := []*queuedPublish{first}
		size := first.bytes
		if b.inFlight != nil {
			size += raft.PublishGroupHeaderSize
		}
		if size < maxBytes {
			timer := time.NewTimer(maxWait)
		collect:
			for len(group) < maxRequests && size < maxBytes {
				select {
				case req := <-lane:
					if req.ctx.Err() != nil {
						req.releaseAdmission()
						continue
					}
					if req.bytes > maxBytes-size {
						next = req
						break collect
					}
					group = append(group, req)
					size += req.bytes
				case <-timer.C:
					break collect
				case <-b.ctx.Done():
					break collect
				}
			}
			timer.Stop()
		}

		run := func() {
			defer releasePublishGroup(group)
			active := make([]*queuedPublish, 0, len(group))
			for _, req := range group {
				if req.ctx.Err() == nil {
					active = append(active, req)
				}
			}
			if len(active) > 0 && b.ctx.Err() == nil {
				flush(b.ctx, level, active)
			}
		}
		if b.inFlight == nil {
			run()
			continue
		}
		select {
		case b.inFlight <- struct{}{}:
			b.flushes.Go(func() {
				defer func() { <-b.inFlight }()
				run()
			})
		case <-b.ctx.Done():
			releasePublishGroup(group)
			return
		}
	}
}

func releasePublishGroup(group []*queuedPublish) {
	for _, req := range group {
		req.releaseAdmission()
	}
}

func (req *queuedPublish) releaseAdmission() {
	if req.release != nil {
		req.release()
		req.release = nil
	}
}

func (req *queuedPublish) finish(err error) {
	req.releaseAdmission()
	req.done <- err
}

func (ph *ProducerHandler) preparePublish(ctx context.Context, batch *pb.PublishBatch, nowMs int64, level pb.AckLevel, receipts map[string][]byte) (*queuedPublish, error) {
	clustered := app.A.NodeHost != nil
	items, err := ph.buildStoreItems(batch, nowMs, receipts, clustered)
	if err != nil {
		return nil, err
	}
	req := &queuedPublish{ctx: ctx, done: make(chan error, 1), count: len(items)}
	var size int
	for _, item := range items {
		itemBytes := raft.PublishItemStorageBytes(item)
		if itemBytes > ph.maxPublishBytes-size {
			return nil, status.Error(codes.ResourceExhausted, "publish batch exceeds storage byte budget")
		}
		size += itemBytes
	}
	req.bytes = size
	if clustered {
		req.frame, err = raft.MarshalPublishFrame(items, level != pb.AckLevel_ACK_LEVEL_QUORUM)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal publish batch: %w", err)
		}
		req.bytes = max(req.bytes, len(req.frame)+raft.PublishGroupFrameOverhead)
		if raft.PublishGroupHeaderSize+req.bytes > ph.maxPublishBytes {
			return nil, status.Error(codes.ResourceExhausted, "publish batch exceeds byte limit")
		}
	} else {
		req.items = items
	}
	return req, nil
}

func (ph *ProducerHandler) flushPublishGroup(ctx context.Context, level pb.AckLevel, group []*queuedPublish) {
	replyAll := func(err error) {
		for _, req := range group {
			req.finish(err)
		}
	}
	if app.A.NodeHost == nil {
		ph.standaloneWriter.Lock()
		defer ph.standaloneWriter.Unlock()
		if err := ctx.Err(); err != nil {
			replyAll(status.FromContextError(err).Err())
			return
		}
		replyAll(ph.commitPublishGroup(group))
		return
	}
	if err := checkPublishLeader(); err != nil {
		replyAll(err)
		return
	}

	frames := make([][]byte, len(group))
	for i, req := range group {
		frames[i] = req.frame
	}
	frozen := level != pb.AckLevel_ACK_LEVEL_QUORUM
	command, err := raft.MarshalPublishGroupCmd(frames, frozen, ph.maxPublishBytes)
	if err != nil {
		replyAll(err)
		return
	}
	if err := ctx.Err(); err != nil {
		replyAll(status.FromContextError(err).Err())
		return
	}
	result, err := ph.proposePublish(ctx, level, command)
	if err != nil || frozen {
		replyAll(publishProposalError(err))
		return
	}
	results, err := raft.UnmarshalPublishGroupResults(result, len(group))
	if err != nil {
		replyAll(publishProposalError(err))
		return
	}
	for i, req := range group {
		req.finish(publishProposalError(raft.CheckPublishResult(results[i], req.count)))
	}
}

func (ph *ProducerHandler) commitPublishGroup(group []*queuedPublish) error {
	batch := app.A.DB.NewBatch()
	defer batch.Close() //nolint:errcheck
	repo := app.A.Repositories.Events
	var published map[string]*raft.DeliveryState
	var decoded map[string]*raft.DeliveryState
	if ph.StandalonePublishLedger != nil {
		published = make(map[string]*raft.DeliveryState)
		decoded = make(map[string]*raft.DeliveryState)
	}
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
			if published != nil {
				state, seen := decoded[string(item.Receipt)]
				if !seen {
					state, err = raft.UnmarshalDeliveryState(item.Receipt)
					if err != nil {
						ph.logger.Warn("failed to decode published receipt for cache", zap.Error(err))
					}
					decoded[string(item.Receipt)] = state
				}
				if state != nil {
					published[string(key)] = state
				}
			}
		}
	}
	if err := repo.PersistLastID(batch, lastID); err != nil {
		return err
	}
	if err := ph.commitStandalonePublish(batch, published); err != nil {
		ph.logger.Error("failed to commit standalone publish group", zap.Error(err))
		return errBatchSave
	}
	return nil
}
