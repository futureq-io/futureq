package dispatcher

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
)

// TTLJanitor periodically performs a full Pebble scan and removes messages
// whose TTL has elapsed. Unlike the dispatcher (which only scans active-topic
// ranges), the janitor sweeps all keys so expired messages are cleaned up even
// when no consumer is connected.
//
// Expired keys are forwarded to the Deleter, which routes them through Raft
// (or Pebble directly in single-node mode) as a batched DeleteBatchCmd.
type TTLJanitor struct {
	db       storage.DB
	deleter  *Deleter
	interval time.Duration
	logger   *zap.Logger
}

// NewTTLJanitor constructs a TTLJanitor. interval controls how often the full
// scan runs (e.g., 60 seconds). Shorter intervals mean faster cleanup at the
// cost of more I/O.
func NewTTLJanitor(db storage.DB, deleter *Deleter, interval time.Duration, logger *zap.Logger) *TTLJanitor {
	return &TTLJanitor{
		db:       db,
		deleter:  deleter,
		interval: interval,
		logger:   logger.Named("ttl_janitor"),
	}
}

// Run starts the TTL janitor loop. It blocks until ctx is cancelled.
func (j *TTLJanitor) Run(ctx context.Context) {
	// Run the first pass after one full interval to avoid startup contention.
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.sweep()
		}
	}
}

// sweep performs one full scan of Pebble and collects expired message keys.
func (j *TTLJanitor) sweep() {
	iter, err := j.db.NewIter(nil) // no bounds — full scan
	if err != nil {
		j.logger.Error("TTL janitor: failed to create iterator", zap.Error(err))
		return
	}
	defer iter.Close()

	nowMs := time.Now().UnixMilli()
	var expiredKeys [][]byte

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()

		// Only consider 24-byte event keys.
		if _, _, _, err := utils.ParseEventKey(key); err != nil {
			j.logger.Error(
				"failed to parse event key",
				zap.ByteString("key", key),
				zap.Error(err),
			)
			continue
		}

		val := iter.Value()
		var msg storagepb.StoredMessage
		if err := proto.Unmarshal(val, &msg); err != nil {
			// Skip keys we can't parse.
			continue
		}

		if msg.TtlMs <= 0 {
			// No TTL set — message lives forever.
			continue
		}

		expiresAt := msg.EnqueuedAtUnixMs + msg.TtlMs
		if nowMs >= expiresAt {
			keyCopy := make([]byte, len(key))
			copy(keyCopy, key)
			expiredKeys = append(expiredKeys, keyCopy)
		}
	}

	if err := iter.Error(); err != nil {
		j.logger.Error("TTL janitor: iterator error", zap.Error(err))
	}

	if len(expiredKeys) == 0 {
		return
	}

	// Enqueue expired keys for batched deletion via the Deleter.
	for _, k := range expiredKeys {
		j.deleter.MarkDeleted(k)
	}

	j.logger.Info("TTL janitor: marked expired messages for deletion",
		zap.Int("count", len(expiredKeys)),
	)
}
