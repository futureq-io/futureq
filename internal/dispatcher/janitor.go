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

// TTLJanitor periodically performs a full storage scan and removes messages
// whose TTL has elapsed. Unlike the dispatcher (which only scans active-topic
// ranges), the janitor sweeps all keys so expired messages are cleaned up even
// when no consumer is connected.
//
// Expired keys are forwarded to the Deleter, which routes them through Raft
// (or the local storage engine directly in single-node mode) as a batched
// DeleteBatchCmd.
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

// sweep performs one full scan of the storage engine and collects expired
// message keys. Uses Scan for lower overhead — no manual iterator lifecycle.
func (j *TTLJanitor) sweep() {
	nowMs := time.Now().UnixMilli()
	var expiredKeys [][]byte

	err := j.db.Scan(nil, func(key, val []byte) error {
		// Only consider 24-byte event keys.
		if _, _, _, err := utils.ParseEventKey(key); err != nil {
			// Not an event key (metadata, indexes, etc.) — skip silently.
			return nil
		}

		var msg storagepb.StoredMessage
		if err := proto.Unmarshal(val, &msg); err != nil {
			// Skip keys we can't parse.
			return nil
		}

		if msg.TtlMs <= 0 {
			// No TTL set — message lives forever.
			return nil
		}

		if nowMs >= msg.EnqueuedAtUnixMs+msg.TtlMs {
			// Must copy — key is only valid during yield.
			keyCopy := make([]byte, len(key))
			copy(keyCopy, key)
			expiredKeys = append(expiredKeys, keyCopy)
		}

		return nil
	})

	if err != nil {
		j.logger.Error("TTL janitor: scan error", zap.Error(err))
		return
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
