package dispatcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type slowLayoutProbeDB struct {
	storage.DB
	ctx    context.Context
	writes uint64
}

func (db *slowLayoutProbeDB) CacheRevision() uint64 {
	return db.DB.(storage.CacheRevisioner).CacheRevision()
}

func (db *slowLayoutProbeDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	if len(opts.LowerBound) >= 8 {
		hash := binary.BigEndian.Uint64(opts.LowerBound[:8])
		if bytes.Equal(opts.LowerBound, shardScanBound(hash, 2)) && bytes.Equal(opts.UpperBound, topicScanEnd(hash)) {
			// With one discovery worker, each probe outlasts the other
			// topic's refresh interval and changes its observed revision.
			timer := time.NewTimer(18 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-db.ctx.Done():
				return nil, db.ctx.Err()
			}
			db.writes++
			key := utils.EventKey(0, utils.TopicHash("unrelated"), db.writes)
			if err := storeProbeEvent(db.DB, key, nil); err != nil {
				return nil, err
			}
		}
	}
	return db.DB.NewIter(opts)
}

func storeProbeEvent(db storage.DB, key, value []byte) error {
	batch := db.NewBatch()
	defer batch.Close() //nolint:errcheck
	if err := batch.Set(key, value); err != nil {
		return err
	}
	return batch.Commit(storage.NoSync)
}

func TestSlowLayoutRefreshesAllowDueDeliveryOnBothTopics(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.DiscoveryWorkers, d.ShardWorkers = 1, 1
	d.interval = 5 * time.Millisecond
	d.MaintenanceRecipients = nil
	d.hub.SetLocalReplica(1)
	d.hub.SetGroupMembership(func(topic, _ string) []metadata.ConsumerMember {
		return []metadata.ConsumerMember{{ID: topic, NodeID: 1}}
	})
	streams := make(map[string]chan *pb.QueueMessage)
	due := time.Now().Add(-time.Second).UnixMilli()
	bucket := utils.ShardStorageBucket(2, utils.CalculateBucket(due, d.TimeBucket))
	for _, topic := range []string{"a", "b"} {
		streams[topic] = make(chan *pb.QueueMessage, 1)
		d.hub.Register(topic, topic, "group", streams[topic])
		value, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, EnqueuedAtUnixMs: due})
		require.NoError(t, err)
		require.NoError(t, storeProbeEvent(d.db, utils.EventKey(bucket, utils.TopicHash(topic), 1), value))
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.db = &slowLayoutProbeDB{DB: d.db, ctx: ctx}
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for _, topic := range []string{"a", "b"} {
		require.Equal(t, topic, receiveReplicaMessage(t, streams[topic]).Topic)
	}
}

type countedTopicRevisionDB struct {
	storage.DB
	opens int
}

func (db *countedTopicRevisionDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	db.opens++
	return db.DB.NewIter(opts)
}

func (db *countedTopicRevisionDB) TopicCacheRevision(hash uint64) uint64 {
	return db.DB.(storage.TopicCacheRevisioner).TopicCacheRevision(hash)
}

func TestFutureOnlyShardIdleHintsSurviveUnrelatedWrites(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.Limits.Examined, d.Limits.WorkTime = 256, time.Second
	d.hub.SetLocalReplica(1)
	d.hub.SetGroupMembership(func(string, string) []metadata.ConsumerMember {
		return []metadata.ConsumerMember{{ID: "local", NodeID: 1}}
	})
	const topic = "orders"
	d.hub.Register("local", topic, "group", make(chan *pb.QueueMessage, 1))
	db := &countedTopicRevisionDB{DB: d.db}
	d.db = db
	now := time.Now().UnixMilli()
	hash := utils.TopicHash(topic)
	future := now + time.Hour.Milliseconds()
	futureValue, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, EnqueuedAtUnixMs: future})
	require.NoError(t, err)
	for shard := range 64 {
		bucket := utils.ShardStorageBucket(64+shard, utils.CalculateBucket(future, d.TimeBucket))
		require.NoError(t, storeProbeEvent(db, utils.EventKey(bucket, hash, uint64(shard+1)), futureValue))
	}
	require.Empty(t, d.collectTopic(topic, now).items)
	require.Positive(t, db.opens)
	opens := db.opens
	for i := range 3 {
		key := utils.EventKey(0, utils.TopicHash("unrelated"), uint64(i+1))
		require.NoError(t, storeProbeEvent(db, key, nil))
		require.Empty(t, d.collectTopic(topic, now+int64(i+1)).items)
		require.Equal(t, opens, db.opens, "no iterators needed while the topic's idle hints are valid")
	}
	dueValue, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, EnqueuedAtUnixMs: now})
	require.NoError(t, err)
	key := utils.EventKey(utils.ShardStorageBucket(127, utils.CalculateBucket(now, d.TimeBucket)), hash, 65)
	require.NoError(t, storeProbeEvent(db, key, dueValue))
	items := d.collectTopic(topic, now+4).items
	require.Len(t, items, 1)
	require.Equal(t, key, items[0].Key)
}

func TestDiscoveryChecksOneReservationPerGroupAndEachUniversal(t *testing.T) {
	d, _ := newPipelineTest(t)
	const topic = "orders"
	first := &ConsumerEntry{ID: "first", Topic: topic, Group: "group"}
	second := &ConsumerEntry{ID: "second", Topic: topic, Group: "group"}
	other := &ConsumerEntry{ID: "other", Topic: topic, Group: "other-group"}
	universal := &ConsumerEntry{ID: "universal", Topic: topic}
	view := InterestView{
		groups:    []localGroup{{consumers: []*ConsumerEntry{first, second}}, {consumers: []*ConsumerEntry{other}}},
		universal: []*ConsumerEntry{universal},
	}
	key := utils.EventKey(0, utils.TopicHash(topic), 1)
	tag := string(key)
	var owners []string
	for owner := range view.localOwners(key) {
		owners = append(owners, owner.ID)
	}
	require.Equal(t, []string{"universal", "first", "other"}, owners)
	d.hub.groupInFlight[groupDeliveryKey{topic: topic, group: "group", key: tag}] = second.ID
	d.hub.deliveryRecords[universal.ID] = map[string]deliveryRecord{tag: {}}
	owned, ready := d.hub.hasReadyOwner(view, key)
	require.True(t, owned && ready, "the other group can still receive the key")
	d.hub.groupInFlight[groupDeliveryKey{topic: topic, group: "other-group", key: tag}] = other.ID
	owned, ready = d.hub.hasReadyOwner(view, key)
	require.True(t, owned)
	require.False(t, ready)
	delete(d.hub.deliveryRecords[universal.ID], tag)
	owned, ready = d.hub.hasReadyOwner(view, key)
	require.True(t, owned && ready, "universal delivery is independent of named groups")
}
