package dispatcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type payloadReadDB struct {
	storage.DB
	payloadReads atomic.Int64
}

func (db *payloadReadDB) Get(key []byte) ([]byte, io.Closer, error) {
	if len(key) == 24 {
		db.payloadReads.Add(1)
	}
	return db.DB.Get(key)
}

func TestFullMessageCacheAvoidsFinalPayloadReadAndOwnsBytes(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, ledger := newPreloadTest(t, engine)
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			now := time.Now().UnixMilli()
			key := putPipelineEvent(t, d, "orders", 1, now-1000)
			observed := &payloadReadDB{DB: d.db}
			d.db = observed
			ledger.db = observed
			chunk := d.collectScheduledTopic("orders", now)
			require.Len(t, chunk.items, 1)
			require.Equal(t, []byte("payload"), chunk.payloads[string(key)].message.Payload)
			require.True(t, chunk.payloads[string(key)].validity.Valid())
			states, err := d.prepareChunk(context.Background(), chunk)
			require.NoError(t, err)
			require.EqualValues(t, 1, observed.payloadReads.Load(), "new manifest preparation still verifies existence via Get")
			observed.payloadReads.Store(0)
			require.Equal(t, 1, d.deliverChunk(chunk, states))
			require.Zero(t, observed.payloadReads.Load(), "valid cached data must avoid final full payload Get/decode")
			require.Equal(t, []byte("payload"), (<-ch).Payload)
		})
	}
}

func TestCachedWorkerCannotBypassDeleteOverwriteOrSnapshot(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, mutation := range []string{"delete", "overwrite", "snapshot"} {
			t.Run(engine+"/"+mutation, func(t *testing.T) {
				d, _ := newPreloadTest(t, engine)
				ch := make(chan *pb.QueueMessage, 1)
				d.hub.Register("local", "orders", "workers", ch)
				key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
				chunk := d.collectScheduledTopic("orders", time.Now().UnixMilli())
				states, err := d.prepareChunk(context.Background(), chunk)
				require.NoError(t, err)
				if mutation == "snapshot" {
					// Real state-machine replacement, including its clearDB phase. Same key,
					// new bytes: old payload must never be authorized by a delayed delete hint.
					var stream bytes.Buffer
					value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("replacement"), EnqueuedAtUnixMs: time.Now().Add(-time.Second).UnixMilli()})
					require.NoError(t, err)
					for _, record := range []struct{ k, v []byte }{{key, value}} {
						require.NoError(t, binary.Write(&stream, binary.LittleEndian, uint32(len(record.k))))
						stream.Write(record.k)
						require.NoError(t, binary.Write(&stream, binary.LittleEndian, uint32(len(record.v))))
						stream.Write(record.v)
					}
					repo, err := repository.NewEventRepository(d.db, zap.NewNop(), time.Millisecond)
					require.NoError(t, err)
					sm := raft.NewEventStateMachineFactory(d.db, repo, nil, zap.NewNop())(1, 1)
					require.NoError(t, sm.RecoverFromSnapshot(&stream, make(chan struct{})))
				} else {
					batch := d.db.NewBatch()
					if mutation == "delete" {
						require.NoError(t, batch.Delete(key))
					} else {
						value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("replacement"), EnqueuedAtUnixMs: time.Now().Add(-time.Second).UnixMilli()})
						require.NoError(t, err)
						require.NoError(t, batch.Set(key, value))
					}
					require.NoError(t, batch.Commit(storage.Sync))
					require.NoError(t, batch.Close())
				}
				require.False(t, chunk.payloads[string(key)].validity.Valid())
				sent := d.deliverChunk(chunk, states)
				if mutation == "delete" || mutation == "snapshot" {
					require.Zero(t, sent)
					require.Empty(t, ch)
				} else {
					require.Equal(t, 1, sent)
					require.Equal(t, []byte("replacement"), (<-ch).Payload)
				}
			})
		}
	}
}

func TestSlowDiscoveryCannotBlockCachedDeadline(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.interval = time.Second
	slow := make(chan *pb.QueueMessage, 1)
	fast := make(chan *pb.QueueMessage, 1)
	d.hub.Register("fast", "b-fast", "workers", fast)
	due := time.Now().Add(120 * time.Millisecond).UnixMilli()
	putPipelineEvent(t, d, "b-fast", 1, due)
	require.Zero(t, d.dispatchAll())
	require.Equal(t, 1, d.preloadCount)
	d.hub.Register("slow", "a-slow", "workers", slow)
	// Registration changes standalone epoch: refill the cache for that epoch.
	require.Zero(t, d.dispatchAll())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	db := d.db
	d.db = blockingTopicDB{DB: db, topic: "a-slow", block: func() { once.Do(func() { close(entered) }); <-release }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { close(release); cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow scan did not enter")
	}
	select {
	case msg := <-fast:
		require.Equal(t, "b-fast", msg.Topic)
	case <-time.After(700 * time.Millisecond):
		t.Fatal("blocked storage scan delayed cached deadline")
	}
}

type blockingTopicDB struct {
	storage.DB
	topic string
	block func()
}

func (db blockingTopicDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	if opts != nil && len(opts.LowerBound) >= 8 && bytes.Equal(opts.LowerBound[:8], utils.TopicLowerBound(utils.TopicHash(db.topic))) {
		db.block()
	}
	return db.DB.NewIter(opts)
}

func TestPoppedHeapSlotsReleasePayloadReferences(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	now := time.Now().UnixMilli()
	putPipelineEvent(t, d, "orders", 1, now+1000)
	require.Empty(t, d.collectScheduledTopic("orders", now).items)
	p := d.preloads["orders"]
	backing := p.items[:cap(p.items)]
	chunk := d.collectPreloaded("orders", p, d.hub.InterestView("orders"), now+1000)
	require.Len(t, chunk.items, 1)
	require.NotNil(t, chunk.payloads[string(chunk.items[0].Key)].message)
	for _, slot := range backing {
		require.Nil(t, slot.payload.message)
	}
	require.Nil(t, p.items)
	require.Nil(t, p.keys)
}

func TestZeroPreloadSettingsKeepBoundedDueDelivery(t *testing.T) {
	for _, setting := range []string{"lookahead", "count"} {
		t.Run(setting, func(t *testing.T) {
			d, _ := newPreloadTest(t, "pebble")
			if setting == "count" {
				d.PreloadMaxMessages = 0
			} else {
				d.PreloadLookahead = 0
			}
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			select {
			case <-ch:
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("zero future settings disabled due delivery")
			}
			cancel()
			<-done
			require.Zero(t, d.preloadCount)
			require.Zero(t, d.readyCount)
		})
	}
}

func TestCachedWorkerUsesCommittedRecipientACKsAfterPreparation(t *testing.T) {
	d, ledger := newPreloadTest(t, "pebble")
	first, second := make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2)
	d.hub.Register("a", "orders", "a", first)
	d.hub.Register("b", "orders", "b", second)
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	chunk := d.collectScheduledTopic("orders", time.Now().UnixMilli())
	states, err := d.prepareChunk(context.Background(), chunk)
	require.NoError(t, err)
	require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
	require.Equal(t, 1, d.deliverChunk(chunk, states))
	require.Empty(t, first)
	require.Len(t, second, 1)
}

func TestRemovedGenerationRejectsOldCachedDelivery(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	chunk := d.collectScheduledTopic("orders", time.Now().UnixMilli())
	states, err := d.prepareChunk(context.Background(), chunk)
	require.NoError(t, err)
	generation := &atomic.Bool{}
	generation.Store(true)
	chunk.generation = generation
	generation.Store(false)
	require.Zero(t, d.deliverChunk(chunk, states))
	require.Empty(t, ch)
}

func TestAsyncRaftCachePreservesFollowerFanout(t *testing.T) {
	c := newReplicaTestCluster(t)
	first := c.subscribe(1, "first", "workers")
	second := c.subscribe(2, "second", "workers")
	universal := c.subscribe(3, "universal", "")
	c.activate()
	due := time.Now().Add(500 * time.Millisecond).UnixMilli()
	value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("cached-raft"), EnqueuedAtUnixMs: due - 1000, DelayMs: 1000})
	require.NoError(t, err)
	cmd, err := raft.MarshalStoreBatchCmd([]raft.StoreBatchItem{{ID: 10, Bucket: utils.CalculateBucket(due, time.Second), TopicHash: utils.TopicHash("orders"), Msg: value}, {ID: 11, Bucket: utils.CalculateBucket(due, time.Second), TopicHash: utils.TopicHash("orders"), Msg: value}})
	require.NoError(t, err)
	callCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_, err = c.nodes[1].nh.SyncPropose(callCtx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(t, err)
	c.sync()
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	for _, node := range c.nodes {
		d := node.dispatcher
		d.PreloadLookahead = time.Second
		d.PreloadMaxMessages = 4
		workers.Add(1)
		go func() { defer workers.Done(); d.Run(ctx) }()
	}
	defer func() { cancel(); workers.Wait() }()
	get := func(ch chan *pb.QueueMessage) *pb.QueueMessage {
		select {
		case msg := <-ch:
			require.GreaterOrEqual(t, time.Now().UnixMilli(), due)
			require.Equal(t, []byte("cached-raft"), msg.Payload)
			return msg
		case <-time.After(3 * time.Second):
			t.Fatal("replica did not deliver cached message")
			return nil
		}
	}
	a, b := get(first), get(second)
	u, v := get(universal), get(universal)
	require.NotEqual(t, a.DeliveryTag, b.DeliveryTag)
	require.ElementsMatch(t, [][]byte{a.DeliveryTag, b.DeliveryTag}, [][]byte{u.DeliveryTag, v.DeliveryTag})
}

func TestDiscoveryRejectsResultCapturedBeforeCommittedDelete(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
	key := putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	scanned := d.collectTopic("orders", time.Now().UnixMilli())
	require.Len(t, scanned.items, 1)
	batch := d.db.NewBatch()
	require.NoError(t, batch.Delete(key))
	require.NoError(t, batch.Commit(storage.Sync))
	require.NoError(t, batch.Close())
	p := &topicPreload{}
	d.admitChunk(p, scanned, nil)
	require.Empty(t, p.items)
	require.Zero(t, d.readyCount)
}

func TestAsyncOldPreparationCannotReviveReplacedSubscription(t *testing.T) {
	d, ledger := newPreloadTest(t, "pebble")
	d.interval = time.Millisecond
	old, newQueue := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
	d.hub.Register("old", "orders", "workers", old)
	putPipelineEvent(t, d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return ledger.PrepareBatch(ctx, items)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("preparation did not start")
	}
	d.hub.Unregister("old")
	d.hub.Register("new", "orders", "workers", newQueue)
	close(release)
	select {
	case msg := <-newQueue:
		require.Equal(t, []byte("payload"), msg.Payload)
	case <-time.After(time.Second):
		t.Fatal("replacement subscription was lost behind stale job")
	}
	require.Empty(t, old, "old job cannot enqueue into an unsubscribed generation")
}
