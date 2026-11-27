package dispatcher

import (
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

type parallelScanDB struct {
	storage.DB
	ctx     context.Context
	started chan parallelScanStart
	release map[uint64]chan struct{}
}

type parallelScanStart struct {
	topic    uint64
	snapshot *parallelScanSnapshot
}

type parallelScanSnapshot struct {
	storage.IteratorSnapshot
	db *parallelScanDB
}

func (db *parallelScanDB) NewIteratorSnapshot() storage.IteratorSnapshot {
	return &parallelScanSnapshot{IteratorSnapshot: db.DB.(storage.IteratorSnapshotter).NewIteratorSnapshot(), db: db}
}

func (s *parallelScanSnapshot) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	topic := binary.BigEndian.Uint64(opts.LowerBound[:8])
	select {
	case s.db.started <- parallelScanStart{topic: topic, snapshot: s}:
	case <-s.db.ctx.Done():
		return nil, s.db.ctx.Err()
	}
	select {
	case <-s.db.release[topic]:
		return s.IteratorSnapshot.NewIter(opts)
	case <-s.db.ctx.Done():
		return nil, s.db.ctx.Err()
	}
}

func TestShardScanConcurrencyMultipliesAndWaitingTopicsGetATurn(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.DiscoveryWorkers, d.ShardWorkers = 3, 2
	d.MaintenanceRecipients = nil
	d.hub.SetLocalReplica(1)
	d.hub.SetGroupMembership(func(topic, _ string) []metadata.ConsumerMember {
		return []metadata.ConsumerMember{{ID: topic, NodeID: 1}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	db := &parallelScanDB{DB: d.db, ctx: ctx, started: make(chan parallelScanStart, 16), release: make(map[uint64]chan struct{})}
	d.db = db
	hashes := make(map[uint64]string)
	topics := []string{"a", "b", "c", "d"}
	batch := d.db.NewBatch()
	defer batch.Close() //nolint:errcheck
	at := time.Now().Add(-time.Second).UnixMilli()
	value, err := proto.Marshal(&storagepb.StoredMessage{EnqueuedAtUnixMs: at})
	require.NoError(t, err)
	for _, topic := range topics {
		hash := utils.TopicHash(topic)
		hashes[hash] = topic
		db.release[hash] = make(chan struct{})
		d.hub.Register(topic, topic, "group", make(chan *pb.QueueMessage, 1))
		for slot := 2; slot < 4; slot++ {
			bucket := utils.ShardStorageBucket(slot, utils.CalculateBucket(at, d.TimeBucket))
			require.NoError(t, batch.Set(bucket.EventKey(hash, uint64(slot)), value))
		}
	}
	require.NoError(t, batch.Commit(storage.Sync))
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("dispatcher did not close its shard scans")
		}
	})
	readStart := func() parallelScanStart {
		t.Helper()
		select {
		case event := <-db.started:
			return event
		case <-time.After(5 * time.Second):
			t.Fatal("shard scan did not start")
			return parallelScanStart{}
		}
	}
	started := make(map[string][]*parallelScanSnapshot)
	for range 6 {
		event := readStart()
		topic := hashes[event.topic]
		started[topic] = append(started[topic], event.snapshot)
	}
	require.Len(t, started, 3)
	var waiting, released string
	for _, topic := range topics {
		if len(started[topic]) == 0 {
			waiting = topic
			continue
		}
		released = topic
		require.Len(t, started[topic], 2)
		require.Same(t, started[topic][0], started[topic][1], "both shard scans share the topic's snapshot")
	}
	close(db.release[utils.TopicHash(released)])
	var waitingScans []*parallelScanSnapshot
	for len(waitingScans) < 2 {
		event := readStart()
		if hashes[event.topic] == waiting {
			waitingScans = append(waitingScans, event.snapshot)
		}
	}
	require.Same(t, waitingScans[0], waitingScans[1])
}

func TestShardScanRangesCoverOnlyOwnedShards(t *testing.T) {
	members := []metadata.ConsumerMember{{ID: "remote", NodeID: 1}, {ID: "local", NodeID: 2}}
	view := InterestView{groups: []localGroup{{
		replica: describeReplica(members, 2, metadata.ShardDistributionConsumerCount),
		local:   []*ConsumerEntry{{ID: "local"}},
	}}}
	lanes := splitShardScans(view, 2, 64)
	require.Len(t, lanes, 2)
	require.True(t, lanes[0].rangeKeys.includeLegacy)
	require.False(t, lanes[1].rangeKeys.includeLegacy)
	for shard := range 64 {
		require.Equal(t, shard >= 32 && shard < 48, lanes[0].rangeKeys.slots.next(64+shard) == 64+shard)
		require.Equal(t, shard >= 48, lanes[1].rangeKeys.slots.next(64+shard) == 64+shard)
	}
}

func TestStandaloneShardWorkersDeliverBothRangesFromOneSnapshot(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.hub.EnableStandaloneShardScans()
	d.DiscoveryWorkers, d.ShardWorkers = 1, 2
	d.MaintenanceRecipients = nil
	const topic = "orders"
	hash := utils.TopicHash(topic)
	messages := make(chan *pb.QueueMessage, 3)
	d.hub.Register("local", topic, "workers", messages)
	due := time.Now().Add(-time.Second)
	putPipelineEvent(t, d, topic, 1, due.UnixMilli()) // slot zero
	first := putShardBudgetPayload(t, d, topic, 2, 2, 32, due)
	second := putShardBudgetPayload(t, d, topic, 3, 3, 32, due)

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	db := &parallelScanDB{
		DB: d.db, ctx: ctx,
		started: make(chan parallelScanStart, 16),
		release: map[uint64]chan struct{}{hash: release},
	}
	d.db = db
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("standalone dispatcher did not stop")
		}
	})

	var snapshot *parallelScanSnapshot
	for range 2 {
		select {
		case scan := <-db.started:
			require.Equal(t, hash, scan.topic)
			if snapshot == nil {
				snapshot = scan.snapshot
			} else {
				require.Same(t, snapshot, scan.snapshot)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("both shard workers must start before either finishes")
		}
	}
	close(release)

	var tags [][]byte
	for range 2 {
		select {
		case msg := <-messages:
			tags = append(tags, msg.DeliveryTag)
		case <-time.After(5 * time.Second):
			t.Fatal("standalone dispatcher did not deliver both shards")
		}
	}
	require.ElementsMatch(t, [][]byte{first, second}, tags)
}

func TestShardScansFindDueAndFutureKeysInBothRanges(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.Limits.WorkTime, d.PreloadLookahead = time.Second, time.Minute
	d.hub.SetLocalReplica(2)
	d.hub.SetGroupMembership(func(_, _ string) []metadata.ConsumerMember {
		return []metadata.ConsumerMember{{ID: "remote", NodeID: 1}, {ID: "local", NodeID: 2}}
	})
	d.hub.Register("local", "orders", "group", make(chan *pb.QueueMessage, 1))
	now := time.Now()
	hash := utils.TopicHash("orders")
	batch := d.db.NewBatch()
	defer batch.Close() //nolint:errcheck
	for id, shard := range []int{7, 36, 52} {
		for i, due := range []time.Time{now.Add(-time.Second), now.Add(30 * time.Second)} {
			value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", EnqueuedAtUnixMs: due.UnixMilli()})
			require.NoError(t, err)
			bucket := utils.CalculateBucket(due.UnixMilli(), d.TimeBucket)
			key := utils.ShardStorageBucket(64+shard, bucket).EventKey(hash, uint64(id*2+i+1))
			require.NoError(t, batch.Set(key, value))
		}
	}
	require.NoError(t, batch.Commit(storage.Sync))
	view := d.hub.InterestView("orders")
	owner := &topicPreload{epoch: view.Epoch, shardScans: splitShardScans(view, 2, 64)}
	jobs := d.shardDiscoveryJobs(discoveryJob{topic: "orders", owner: owner, cursor: scanCursor{epoch: view.Epoch}, dueSlots: 4, futureSlots: 4}, now)
	require.Len(t, jobs, 2)
	require.Equal(t, d.Limits.Examined, jobs[0].examined+jobs[1].examined)
	require.Equal(t, d.Limits.Bytes, jobs[0].bytes+jobs[1].bytes)
	results := make(chan discoveryResult, len(jobs))
	d.runDiscoveryPass(context.Background(), jobs, results)
	for range jobs {
		result := <-results
		wantSlot := []int{64 + 36, 64 + 52}[result.job.shardLane]
		require.Len(t, result.chunk.items, 1)
		require.Len(t, result.future, 1)
		require.Equal(t, wantSlot, utils.EventShardSlot(result.chunk.items[0].Key))
		require.Equal(t, wantSlot, utils.EventShardSlot(result.future[0].key[:]))
	}
}
