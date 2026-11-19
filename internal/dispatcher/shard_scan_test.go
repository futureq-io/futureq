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
	for _, topic := range []string{"a", "b", "c", "d"} {
		hash := utils.TopicHash(topic)
		hashes[hash] = topic
		db.release[hash] = make(chan struct{})
		d.hub.Register(topic, topic, "group", make(chan *pb.QueueMessage, 1))
	}
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
	for _, topic := range []string{"a", "b", "c"} {
		require.Len(t, started[topic], 2)
		require.Same(t, started[topic][0], started[topic][1], "both shard scans share the topic's snapshot")
	}
	require.Empty(t, started["d"], "the fourth topic waits for a discovery worker")
	close(db.release[utils.TopicHash("b")])
	first, second := readStart(), readStart()
	require.Equal(t, "d", hashes[first.topic])
	require.Equal(t, "d", hashes[second.topic])
	require.Same(t, first.snapshot, second.snapshot)
}

func TestShardScanRangesCoverOnlyOwnedShards(t *testing.T) {
	members := []metadata.ConsumerMember{{ID: "remote", NodeID: 1}, {ID: "local", NodeID: 2}}
	view := InterestView{groups: []localGroup{{
		replica: describeReplica(members, 2, metadata.ShardDistributionConsumerCount),
		local:   []*ConsumerEntry{{ID: "local"}},
	}}}
	lanes := splitShardScans(view, 2)
	require.Len(t, lanes, 2)
	require.True(t, lanes[0].rangeKeys.includeLegacy)
	require.False(t, lanes[1].rangeKeys.includeLegacy)
	for shard := range 64 {
		require.Equal(t, shard >= 32 && shard < 48, lanes[0].rangeKeys.slots[64+shard])
		require.Equal(t, shard >= 48, lanes[1].rangeKeys.slots[64+shard])
	}
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
			key := utils.EventKey(utils.ShardStorageBucket(64+shard, bucket), hash, uint64(id*2+i+1))
			require.NoError(t, batch.Set(key, value))
		}
	}
	require.NoError(t, batch.Commit(storage.Sync))
	view := d.hub.InterestView("orders")
	owner := &topicPreload{epoch: view.Epoch, shardScans: splitShardScans(view, 2)}
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
