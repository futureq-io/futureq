package dispatcher

import (
	"bytes"
	"context"
	"fmt"
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

func TestShardDiscoveryReservesDueLaneBeforePreload(t *testing.T) {
	d, _ := newPipelineTest(t)
	owner := &topicPreload{shardScans: make([]shardScanState, 64)}
	for i := range 62 {
		owner.shardScans[i].cursor.shards = &shardedDueScan{idleUntil: noDueBucket, idleRev: 1}
	}
	job := discoveryJob{owner: owner, revision: 1, tracksRevision: true, dueSlots: 1, futureSlots: 1}
	for _, lane := range []int{62, 63, 62, 63} {
		jobs := d.shardDiscoveryJobs(job, time.Now())
		require.Len(t, jobs, 1)
		require.Equal(t, lane, jobs[0].shardLane)
		require.Equal(t, 1, jobs[0].dueSlots)
	}
}

func newShardBudgetTest(t *testing.T, layout, workers int) (*Dispatcher, discoveryJob) {
	t.Helper()
	d, _ := newPipelineTest(t)
	d.Limits.Bytes, d.Limits.WorkTime = 256<<10, time.Second
	d.PreloadLookahead = time.Minute
	d.hub.SetLocalReplica(1)
	d.hub.SetGroupMembership(func(string, string) []metadata.ConsumerMember {
		return []metadata.ConsumerMember{{ID: "local", NodeID: 1}}
	})
	const topic = "orders"
	d.hub.Register("local", topic, "group", make(chan *pb.QueueMessage, 1))
	view := d.hub.InterestView(topic)
	owner := &topicPreload{epoch: view.Epoch, assignmentEpoch: view.assignmentEpoch, shardScans: splitShardScans(view, workers, layout)}
	return d, discoveryJob{
		topic: topic, owner: owner, dueSlots: d.Limits.Candidates,
		cursor: scanCursor{epoch: view.Epoch, assignmentEpoch: view.assignmentEpoch},
	}
}

func putShardBudgetPayload(t *testing.T, d *Dispatcher, topic string, slot, id, size int, at time.Time) []byte {
	t.Helper()
	value, err := proto.Marshal(&storagepb.StoredMessage{
		Topic: topic, Payload: bytes.Repeat([]byte("x"), size), EnqueuedAtUnixMs: at.UnixMilli(),
	})
	require.NoError(t, err)
	bucket := utils.ShardStorageBucket(slot, utils.CalculateBucket(at.UnixMilli(), d.TimeBucket))
	key := bucket.EventKey(utils.TopicHash(topic), uint64(id))
	require.NoError(t, storeProbeEvent(d.db, key, value))
	return key
}

func runShardBudgetPass(d *Dispatcher, base discoveryJob) []discoveryResult {
	jobs := d.shardDiscoveryJobs(base, time.Now())
	results := make(chan discoveryResult, len(jobs))
	d.runDiscoveryPass(context.Background(), jobs, results)
	pass := make([]discoveryResult, 0, len(jobs))
	for range jobs {
		pass = append(pass, <-results)
	}
	return pass
}

func TestShardDiscoverySharesPayloadBudgetAcrossLanes(t *testing.T) {
	for _, workers := range []int{1, 64} {
		t.Run(fmt.Sprintf("%d_workers", workers), func(t *testing.T) {
			d, base := newShardBudgetTest(t, 64, workers)
			due := time.Now().Add(-time.Second)
			for slot := 64; slot < 128; slot++ {
				putShardBudgetPayload(t, d, base.topic, slot, slot, 96<<10, due)
			}
			results := runShardBudgetPass(d, base)
			require.Len(t, results, workers)
			retained := 0
			for _, result := range results {
				for _, payload := range result.chunk.payloads {
					retained += len(payload.message.Payload)
				}
			}
			require.Equal(t, 192<<10, retained, "the pass can afford exactly two payloads")
		})
	}
}

func TestShardDiscoveryRetriesOversizedPayloadOnRotatingLead(t *testing.T) {
	d, base := newShardBudgetTest(t, 2, 2)
	due := time.Now().Add(-time.Second)
	first := putShardBudgetPayload(t, d, base.topic, 2, 1, 320<<10, due)
	second := putShardBudgetPayload(t, d, base.topic, 3, 2, 320<<10, due)
	for _, want := range [][]byte{first, second} {
		var admitted [][]byte
		for _, result := range runShardBudgetPass(d, base) {
			base.owner.shardScans[result.job.shardLane].cursor = result.cursor
			for _, item := range result.chunk.items {
				admitted = append(admitted, item.Key)
				base.owner.addKey([utils.EventKeySize]byte(item.Key))
			}
		}
		require.Equal(t, [][]byte{want}, admitted)
	}
}

func TestShardDiscoveryPreloadSharesPayloadBudgetAndRetriesDeferredKey(t *testing.T) {
	d, base := newShardBudgetTest(t, 2, 1)
	now := time.Now()
	putShardBudgetPayload(t, d, base.topic, 2, 1, 96<<10, now.Add(-time.Second))
	putShardBudgetPayload(t, d, base.topic, 2, 2, 96<<10, now.Add(-time.Second))
	future := putShardBudgetPayload(t, d, base.topic, 2, 3, 96<<10, now.Add(30*time.Second))
	base.futureSlots = 1
	results := runShardBudgetPass(d, base)
	require.Len(t, results, 1)
	require.Len(t, results[0].chunk.items, 2)
	require.Empty(t, results[0].future)
	require.Equal(t, future, results[0].after)
	require.False(t, results[0].futureExhausted)
	base.owner.shardScans[0].futureAfter = results[0].after
	base.dueSlots = 0
	results = runShardBudgetPass(d, base)
	require.Len(t, results, 1)
	require.Len(t, results[0].future, 1)
	require.Equal(t, [utils.EventKeySize]byte(future), results[0].future[0].key)
}

func TestShardDiscoveryAtMaxLayoutFindsSparseDueAndFutureKeys(t *testing.T) {
	d, base := newShardBudgetTest(t, utils.MaxVirtualShards, 2)
	d.Limits.Examined = 32
	base.futureSlots = 2
	now := time.Now()
	const firstSlot = utils.MaxVirtualShards
	const lastSlot = 2*utils.MaxVirtualShards - 1
	due := putShardBudgetPayload(t, d, base.topic, firstSlot+1024, 1, 32, now.Add(-time.Second))
	future := putShardBudgetPayload(t, d, base.topic, lastSlot, 2, 32, now.Add(30*time.Second))
	byLane := make(map[int]discoveryResult)
	for _, result := range runShardBudgetPass(d, base) {
		byLane[result.job.shardLane] = result
	}
	require.Len(t, byLane, 2)
	require.Len(t, byLane[0].chunk.items, 1)
	require.Equal(t, due, byLane[0].chunk.items[0].Key)
	require.Empty(t, byLane[1].chunk.items)
	require.Len(t, byLane[1].future, 1)
	require.Equal(t, [utils.EventKeySize]byte(future), byLane[1].future[0].key)
}

func TestShardDiscoveryResumesSparseSeekWithOneKeyBudget(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("tracked=%t", tracked), func(t *testing.T) {
			d, base := newShardBudgetTest(t, utils.MaxVirtualShards, 1)
			d.Limits.Examined = 1
			due := putShardBudgetPayload(t, d, base.topic, 2*utils.MaxVirtualShards-1, 1, 32, time.Now().Add(-time.Second))
			if tracked {
				watch := d.watchTopic(utils.TopicHash(base.topic))
				defer watch.Close()
				base.shardRevisions = new(storage.TopicRevisionSnapshot)
				base.revision = watch.Read(base.shardRevisions)
				base.tracksRevision = true
			}
			for range 6 {
				results := runShardBudgetPass(d, base)
				require.Len(t, results, 1)
				result := results[0]
				base.owner.shardScans[0].cursor = result.cursor
				if len(result.chunk.items) != 0 {
					require.Len(t, result.chunk.items, 1)
					require.Equal(t, due, result.chunk.items[0].Key)
					return
				}
			}
			t.Fatal("discovery did not reach the last shard within six passes")
		})
	}
}

func TestSingleShardLayoutDiscoversDueAndFutureKeys(t *testing.T) {
	d, base := newShardBudgetTest(t, 1, 1)
	d.PreloadMaxMessages = 2
	now := time.Now()
	due := putShardBudgetPayload(t, d, base.topic, 1, 1, 32, now.Add(-time.Second))
	future := putShardBudgetPayload(t, d, base.topic, 1, 2, 32, now.Add(30*time.Second))

	chunk := d.collectTopic(base.topic, now.UnixMilli())
	require.Len(t, chunk.items, 1)
	require.Equal(t, due, chunk.items[0].Key)

	preload := &topicPreload{}
	require.True(t, d.preloadTopic(base.topic, preload, d.hub.InterestView(base.topic), now.UnixMilli()))
	require.Len(t, preload.items, 1)
	require.Equal(t, [utils.EventKeySize]byte(future), preload.items[0].key)
}

func TestShardDiscoveryResumesSeekWhileCurrentShardReceivesWrites(t *testing.T) {
	d, base := newShardBudgetTest(t, utils.MaxVirtualShards, 1)
	d.Limits.Examined = 1
	watch := d.watchTopic(utils.TopicHash(base.topic))
	defer watch.Close()
	base.shardRevisions = new(storage.TopicRevisionSnapshot)
	base.tracksRevision = true

	now := time.Now()
	const writingSlot = utils.MaxVirtualShards
	const lastSlot = 2*utils.MaxVirtualShards - 1
	due := putShardBudgetPayload(t, d, base.topic, lastSlot, 1, 32, now.Add(-time.Second))
	for pass := range 6 {
		putShardBudgetPayload(t, d, base.topic, writingSlot, pass+2, 32, now.Add(time.Hour))
		base.revision = watch.Read(base.shardRevisions)

		results := runShardBudgetPass(d, base)
		require.Len(t, results, 1)
		result := results[0]
		base.owner.shardScans[0].cursor = result.cursor
		if len(result.chunk.items) != 0 {
			require.Len(t, result.chunk.items, 1)
			require.Equal(t, due, result.chunk.items[0].Key)
			return
		}
	}
	t.Fatal("discovery did not reach the last shard while the first shard received writes")
}
