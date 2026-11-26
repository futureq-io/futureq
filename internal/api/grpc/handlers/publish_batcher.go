package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/utils"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type queuedPublish struct {
	ctx     context.Context
	done    chan error
	release func() // admission belongs to the worker after handoff
	bytes   int
	items   []raft.StoreBatchItem
}

type publishBatcher struct {
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	queueIn chan *queuedPublish
}

func newPublishBatcher(cfg config.Publish, write func(context.Context, []*queuedPublish) error) *publishBatcher {
	ctx, cancel := context.WithCancel(context.Background())
	b := &publishBatcher{ctx: ctx, cancel: cancel, queueIn: make(chan *queuedPublish)}
	maxBytes, _ := cfg.BatchMaxBytes.Bytes() // configuration validated at startup
	b.workers.Go(func() {
		b.run(cfg.BatchMaxRequests, int(maxBytes), cfg.BatchMaxWait, write)
	})
	return b
}

func (b *publishBatcher) close() {
	b.cancel()
	b.workers.Wait()
}

func (b *publishBatcher) queue(req *queuedPublish) (bool, error) {
	select {
	case b.queueIn <- req:
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

func (b *publishBatcher) run(maxRequests, maxBytes int, maxWait time.Duration, write func(context.Context, []*queuedPublish) error) {
	var next *queuedPublish
	defer func() {
		if next != nil {
			next.releaseAdmission()
		}
	}()
	for b.ctx.Err() == nil {
		if next == nil {
			select {
			case next = <-b.queueIn:
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
		if size < maxBytes {
			timer := time.NewTimer(maxWait)
		collect:
			for len(group) < maxRequests && size < maxBytes {
				select {
				case req := <-b.queueIn:
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

		active := group[:0]
		for _, req := range group {
			if req.ctx.Err() != nil || b.ctx.Err() != nil {
				req.releaseAdmission()
			} else {
				active = append(active, req)
			}
		}
		if len(active) != 0 {
			err := write(b.ctx, active)
			for _, req := range active {
				req.releaseAdmission()
				req.done <- err
			}
		}
	}
}

func (req *queuedPublish) releaseAdmission() {
	if req.release != nil {
		req.release()
		req.release = nil
	}
}

func (ph *ProducerHandler) writeStandaloneGroup(ctx context.Context, group []*queuedPublish) error {
	ph.standaloneWriter.Lock()
	defer ph.standaloneWriter.Unlock()
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
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
