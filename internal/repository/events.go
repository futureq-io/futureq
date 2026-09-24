package repository

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// LastIDKey stores the highest event ID ever applied, as a durable high-water
// mark. It travels inside Raft snapshots (full DB scan), so snapshot recovery
// restores it automatically.
var LastIDKey = []byte("metadata/event-repo/last-id")

// EventRepository builds event keys and stores events.
//
// Event IDs are assigned exclusively by the Raft leader (or the single node in
// standalone mode) at propose time via NextID. The assigned ID travels inside
// the Raft command, so every replica writes the identical key — no per-replica
// ID generation exists, because per-replica counters make replicas diverge.
type EventRepository struct {
	db         storage.DB
	logger     *zap.Logger
	nextID     atomic.Uint64
	bucketSize time.Duration
}

func NewEventRepository(db storage.DB, logger *zap.Logger, bucketSize time.Duration) (*EventRepository, error) {
	repo := &EventRepository{
		db:         db,
		logger:     logger,
		bucketSize: bucketSize,
	}

	// Restore the durable high-water mark so a restarted node never reuses IDs
	// even before Raft log replay begins.
	val, closer, err := db.Get(LastIDKey)
	if err != nil {
		if !errors.Is(err, pebble.ErrNotFound) {
			return nil, err
		}
	} else {
		repo.nextID.Store(binary.BigEndian.Uint64(val))
		closer.Close() //nolint:errcheck
	}

	return repo, nil
}

// NextID reserves a new monotonic event ID. Must only be called on the Raft
// leader (or the single node in standalone mode) while building a batch.
// IDs are monotonic, not contiguous: failed proposals simply skip IDs.
func (er *EventRepository) NextID() uint64 {
	return er.nextID.Add(1)
}

// StoreWithBatch marshals msg and adds it to an existing Pebble batch using
// the caller-supplied leader-assigned ID. It returns the 24-byte Pebble key.
func (er *EventRepository) StoreWithBatch(b storage.Batch, id uint64, msg *storagepb.StoredMessage) ([]byte, error) {
	fireAtMs := msg.EnqueuedAtUnixMs + msg.DelayMs
	bucket := utils.CalculateBucket(fireAtMs, er.bucketSize)
	topicHash := utils.TopicHash(msg.Topic)
	key := utils.EventKey(bucket, topicHash, id)

	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message for topic %q: %w", msg.Topic, err)
	}

	if err := b.Set(key, data); err != nil {
		return nil, err
	}

	// Standalone path only (Raft path goes through StoreRawWithBatch +
	// PersistLastID): keep the high-water mark durable across restarts.
	if err := er.PersistLastID(b, id); err != nil {
		return nil, err
	}

	for _, idx := range msg.GetIndexes() {
		idxBytes, err := proto.Marshal(idx)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal index to bytes: %w", err)
		}

		if err := b.Set(idxBytes, key); err != nil {
			return nil, err
		}
	}

	return key, nil
}

// StoreRawWithBatch stores the raw msg value bytes under the leader-assigned
// ID from the Raft command. It never generates IDs itself — replicas apply the
// command verbatim, so all replicas converge on identical keys.
// It returns the 24-byte Pebble key for the caller to use as a delivery_tag.
func (er *EventRepository) StoreRawWithBatch(b storage.Batch, id uint64, bucket uint64, topicHash uint64, indexes [][]byte, msg []byte) ([]byte, error) {
	key := utils.EventKey(bucket, topicHash, id)

	if err := b.Set(key, msg); err != nil {
		return nil, err
	}

	for _, idx := range indexes {
		if err := b.Set(idx, key); err != nil {
			return nil, err
		}
	}

	return key, nil
}

// ObserveID advances the local reservation counter past id. Replicas call this
// when applying commands (or after snapshot recovery) so that a follower
// promoted to leader never reuses IDs.
func (er *EventRepository) ObserveID(id uint64) {
	for {
		cur := er.nextID.Load()
		if id <= cur || er.nextID.CompareAndSwap(cur, id) {
			return
		}
	}
}

// PersistLastID adds the durable high-water mark to an existing batch.
// Called once per applied Raft entry — piggybacks on the entry's batch commit,
// so durability costs no extra fsync. The key is included in snapshots, making
// the high-water mark survive snapshot-based recovery.
func (er *EventRepository) PersistLastID(b storage.Batch, id uint64) error {
	idBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(idBytes, id)
	return b.Set(LastIDKey, idBytes)
}

func (er *EventRepository) DeleteWithBatch(b storage.Batch, key []byte) error {
	return b.Delete(key)
}
