package dispatcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/futureq-io/futureq/internal/config"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type PreloadSuite struct {
	suite.Suite
}

func TestPreloadSuite(t *testing.T) {
	suite.Run(t, new(PreloadSuite))
}

func newPreloadTest(t *testing.T, engine string) (*Dispatcher, *DeliveryLedger) {
	t.Helper()
	d, ledger := newPipelineTest(t)
	if engine == "bolt" {
		db, err := storage.NewBoltDB(config.Bolt{File: filepath.Join(t.TempDir(), "events.db")})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		d.db = db
		ledger = NewDeliveryLedger(db, d.hub.LocalDeliveryRecipients, NewDirectDeleteBackend(db, zap.NewNop()), nil)
		d.deleter = NewDeleter(ledger, time.Hour, zap.NewNop())
		d.PrepareBatch = ledger.PrepareBatch
	}
	d.PreloadLookahead, d.PreloadMaxMessages = 10*time.Second, 4
	d.Limits.WorkTime = time.Hour
	return d, ledger
}

func (s *PreloadSuite) TestPreloadBoundsExactHorizonAndDeduplication() {
	for _, engine := range []string{"pebble", "bolt"} {
		s.Run(engine, func() {
			require := s.Require()

			d, _ := newPreloadTest(s.T(), engine)
			d.PreloadMaxMessages = 3
			d.TimeBucket = time.Hour
			d.hub.Register("a", "a", "workers", make(chan *pb.QueueMessage, 8))
			d.hub.Register("b", "b", "workers", make(chan *pb.QueueMessage, 8))
			now := time.Now().UnixMilli()
			first := putPipelineEvent(s.T(), d, "a", 1, now+2000)
			putPipelineEvent(s.T(), d, "a", 2, now+4000)
			putPipelineEvent(s.T(), d, "a", 3, now+10001) // same bucket, outside exact horizon
			putPipelineEvent(s.T(), d, "b", 4, now+3000)
			putPipelineEvent(s.T(), d, "b", 5, now+5000)
			for range 3 {
				require.Empty(d.collectScheduledTopic("a", now).items)
			}
			require.Equal(2, d.preloadCount, "repeat scans deduplicate candidates")
			require.Equal([24]byte(first), d.preloads["a"].items[0].key)
			require.Empty(d.collectScheduledTopic("b", now).items)
			require.Equal(3, d.preloadCount, "cap applies across topics")
			state, err := raft.ReadDeliveryState(d.db, first)
			require.NoError(err)
			require.Nil(state, "preloading cannot freeze recipients or prepare early")
			d.prunePreloads(map[string]bool{"b": true})
			require.Equal(1, d.preloadCount)
			d.prunePreloads(nil)
			require.Zero(d.preloadCount)
		})
	}
}

func (s *PreloadSuite) TestPreloadCachedCollectionAvoidsDueIteratorAndStillChecksActualTime() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	ch := make(chan *pb.QueueMessage, 4)
	d.hub.Register("local", "orders", "workers", ch)
	now := time.Now().UnixMilli()
	key := putPipelineEvent(s.T(), d, "orders", 1, now+5000)
	require.Zero(d.dispatchAll())
	require.Equal(1, d.preloadCount)
	d.db = scanStartupDB{DB: d.db, beforeNewIter: func() { s.T().Fatal("cached due candidates must not need a range scan") }}
	chunk := d.collectScheduledTopic("orders", now+5000)
	require.Len(chunk.items, 1)
	require.Equal(key, chunk.items[0].Key)
	// A future caller timestamp is not delivery authorization, even for a cache hit.
	require.Zero(d.deliverChunk(chunk, map[string]*raft.DeliveryState{string(key): {Epoch: chunk.items[0].Epoch, Recipients: map[string]bool{"g:workers": false}}}))
	require.Empty(ch)
}

func (s *PreloadSuite) TestPreloadFullBufferDoesNotHideNewOverdueArrivals() {
	for _, engine := range []string{"pebble", "bolt"} {
		s.Run(engine, func() {
			require := s.Require()

			d, _ := newPreloadTest(s.T(), engine)
			d.PreloadMaxMessages = 1
			d.interval = 10 * time.Millisecond
			ch := make(chan *pb.QueueMessage, 4)
			d.hub.Register("local", "orders", "workers", ch)
			putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(5*time.Second).UnixMilli())
			require.Zero(d.dispatchAll())
			require.Equal(1, d.preloadCount)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			s.T().Cleanup(func() { cancel(); <-done })
			// Deliberately omit a producer wake: the periodic live scan must find it.
			late := putPipelineEvent(s.T(), d, "orders", 2, time.Now().Add(-time.Minute).UnixMilli())
			select {
			case msg := <-ch:
				require.Equal(late, msg.DeliveryTag)
			case <-time.After(time.Second):
				s.T().Fatal("a full future buffer hid an un-preloaded overdue message")
			}
		})
	}
}

func (s *PreloadSuite) TestPreloadAlternatesLiveScansAndRetriesFailures() {
	require := s.Require()

	d, ledger := newPreloadTest(s.T(), "pebble")
	d.Limits.Candidates = 1
	ch := make(chan *pb.QueueMessage, 8)
	d.hub.Register("local", "orders", "workers", ch)
	due := time.Now().Add(40 * time.Millisecond).UnixMilli()
	first := putPipelineEvent(s.T(), d, "orders", 1, due)
	putPipelineEvent(s.T(), d, "orders", 2, due)
	require.Zero(d.dispatchAll())
	time.Sleep(time.Until(time.UnixMilli(due)))
	d.PrepareBatch = func(context.Context, []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
		return nil, errors.New("uncertain preparation failure")
	}
	require.Zero(d.dispatchAll()) // pops a candidate whose preparation fails
	d.PrepareBatch = ledger.PrepareBatch
	late := putPipelineEvent(s.T(), d, "orders", 3, due-60000)
	seen := make(map[string]bool)
	for range 12 {
		d.dispatchAll()
		for len(ch) > 0 {
			seen[string((<-ch).DeliveryTag)] = true
		}
	}
	require.Len(seen, 3)
	require.True(seen[string(first)], "failed preparation remains recoverable by live scanning")
	require.True(seen[string(late)], "an older insertion must be found while cached candidates drain")
}

func (s *PreloadSuite) TestPreloadTimerWakesBeforePollAndDoesNotPrepareEarly() {
	require := s.Require()

	d, ledger := newPreloadTest(s.T(), "pebble")
	d.interval = 2 * time.Second
	ch := make(chan *pb.QueueMessage, 4)
	d.hub.Register("local", "orders", "workers", ch)
	due := time.Now().Add(100 * time.Millisecond).UnixMilli()
	key := putPipelineEvent(s.T(), d, "orders", 1, due)
	var early atomic.Bool
	d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
		if time.Now().UnixMilli() < due {
			early.Store(true)
		}
		return ledger.PrepareBatch(ctx, items)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	s.T().Cleanup(func() { cancel(); <-done })
	select {
	case msg := <-ch:
		require.Equal(key, msg.DeliveryTag)
		require.GreaterOrEqual(time.Now().UnixMilli(), due)
		require.False(early.Load())
	case <-time.After(750 * time.Millisecond):
		s.T().Fatal("preloaded candidate waited for the 2-second poll instead of its due timer")
	}
}

func (s *PreloadSuite) TestPreloadRevalidatesPayloadAndRecipientsAtDelivery() {
	for _, fault := range []string{"deleted", "expired", "new-subscriber", "full-queue"} {
		s.Run(fault, func() {
			require := s.Require()

			d, _ := newPreloadTest(s.T(), "pebble")
			ch, added := make(chan *pb.QueueMessage, 4), make(chan *pb.QueueMessage, 4)
			d.hub.Register("local", "orders", "", ch)
			due := time.Now().Add(40 * time.Millisecond).UnixMilli()
			key := putPipelineEvent(s.T(), d, "orders", 1, due)
			if fault == "expired" {
				value, closer, err := d.db.Get(key)
				require.NoError(err)
				var msg storagepb.StoredMessage
				require.NoError(proto.Unmarshal(value, &msg))
				require.NoError(closer.Close())
				msg.TtlMs = 990 // expires 10ms before its due time
				value, err = proto.Marshal(&msg)
				require.NoError(err)
				batch := d.db.NewBatch()
				require.NoError(batch.Set(key, value))
				require.NoError(batch.Commit(storage.Sync))
				require.NoError(batch.Close())
			}
			require.Zero(d.dispatchAll())
			require.Equal(1, d.preloadCount)
			if fault == "deleted" {
				batch := d.db.NewBatch()
				require.NoError(batch.Delete(key))
				require.NoError(batch.Commit(storage.Sync))
				require.NoError(batch.Close())
			} else if fault == "new-subscriber" {
				d.hub.Register("added", "orders", "", added)
			} else if fault == "full-queue" {
				for range cap(ch) {
					ch <- &pb.QueueMessage{}
				}
			}
			time.Sleep(time.Until(time.UnixMilli(due)))
			if fault == "new-subscriber" {
				require.Equal(1, d.dispatchAll())
				require.Len(ch, 1)
				require.Len(added, 1, "preloading must not freeze the old recipient list")
			} else {
				require.Zero(d.dispatchAll())
				if fault == "full-queue" {
					for len(ch) > 0 {
						<-ch
					}
					require.Equal(1, d.dispatchAll(), "queue failure must remain eligible for retry")
				} else {
					require.Empty(ch)
				}
			}
		})
	}
}

func (s *PreloadSuite) TestPreloadCannotBypassPostPreparationBarrier() {
	for _, fault := range []string{"minority", "pending", "renew"} {
		s.Run(fault, func() {
			require := s.Require()

			d, ledger := newPreloadTest(s.T(), "pebble")
			epoch, active := uint64(9), true
			d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, active }, time.Second)
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			prepared := false
			d.ReadBarrier = func() error {
				if fault == "minority" && prepared {
					return errors.New("no quorum")
				}
				d.hub.GrantDeliveryPermit(epoch, time.Now())
				return nil
			}
			due := time.Now().Add(40 * time.Millisecond).UnixMilli()
			putPipelineEvent(s.T(), d, "orders", 1, due)
			require.Zero(d.dispatchAll())
			require.Equal(1, d.preloadCount)
			d.PrepareBatch = func(ctx context.Context, items []raft.DeliveryPrepare) (map[string]*raft.DeliveryState, error) {
				prepared = true
				d.hub.GrantDeliveryPermit(epoch, time.Now().Add(-2*ConsumerReadPermit))
				if fault == "pending" {
					d.hub.WithRebalanceBarrier(func() { epoch++; active = false })
				}
				return ledger.PrepareBatch(ctx, items)
			}
			time.Sleep(time.Until(time.UnixMilli(due)))
			if fault == "renew" {
				require.Equal(1, d.dispatchAll())
				require.Len(ch, 1)
			} else {
				require.Zero(d.dispatchAll())
				require.Empty(ch)
			}
			require.True(prepared)
		})
	}
}

func (s *PreloadSuite) TestPreloadPartialDrainBoundsRetainedCapacityAcrossTopics() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	d.PreloadMaxMessages, d.PreloadLookahead = 128, time.Minute
	d.preloads = make(map[string]*topicPreload)
	const topics = 64
	for i := range topics {
		topic := fmt.Sprintf("topic-%d", i)
		d.hub.Register(topic, topic, "workers", make(chan *pb.QueueMessage, 1))
	}
	now := time.Now().UnixMilli()
	for i := range topics {
		topic := fmt.Sprintf("topic-%d", i)
		view := d.hub.InterestView(topic)
		p := &topicPreload{epoch: view.Epoch}
		d.preloads[topic] = p
		available := d.PreloadMaxMessages - d.preloadCount
		batch := d.db.NewBatch()
		var retainedKey [24]byte
		for id := range available {
			due := now + 1000
			if id == 0 {
				due = now + 50000 // keep one candidate after the partial drain
			}
			key := utils.EventKey(utils.CalculateBucket(due, d.TimeBucket), utils.TopicHash(topic), uint64(id))
			if id == 0 {
				retainedKey = [24]byte(key)
			}
			value, err := proto.Marshal(&storagepb.StoredMessage{Topic: topic, EnqueuedAtUnixMs: due - 1000, DelayMs: 1000})
			require.NoError(err)
			require.NoError(batch.Set(key, value))
		}
		require.NoError(batch.Commit(storage.Sync))
		require.NoError(batch.Close())
		d.preloadTopic(topic, p, view, now)
		require.Equal(d.PreloadMaxMessages, d.preloadCount)
		require.Len(p.items, available)
		oldKeys := p.keys
		seen := make(map[[24]byte]bool)
		for len(p.items) > 1 {
			chunk := d.collectPreloaded(topic, p, view, now+1000)
			require.NotEmpty(chunk.items)
			for _, item := range chunk.items {
				key := [24]byte(item.Key)
				require.False(seen[key], "compaction must preserve heap contents without duplicates")
				seen[key] = true
			}
		}
		require.Len(seen, available-1)
		require.Equal(retainedKey, p.items[0].key)
		require.Len(p.keys, 1)
		// Go maps do not expose capacity. Prove the oversized map was replaced,
		// rather than merely deleting entries from its existing allocation.
		oldKeys[retainedKey] = false
		require.True(p.keys[retainedKey], "partial drain must rebuild the sparse deduplication map")
		retainedSlots := 0
		for _, cached := range d.preloads {
			retainedSlots += cap(cached.items)
		}
		require.LessOrEqual(retainedSlots, 4*d.preloadCount, "retained storage must track live candidates across topics")
	}
	require.Equal(topics, d.preloadCount)
	for topic, p := range d.preloads {
		// Repeated scanning must still deduplicate the retained future key.
		d.preloadTopic(topic, p, d.hub.InterestView(topic), now+1000)
		require.Len(p.items, 1)
		chunk := d.collectPreloaded(topic, p, d.hub.InterestView(topic), now+50000)
		require.Len(chunk.items, 1)
		require.Nil(p.items)
		require.Nil(p.keys)
	}
	require.Zero(d.preloadCount)
}

func (s *PreloadSuite) TestPreloadFailedRenewalDoesNotSpinOnOverdueTimer() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	d.interval = time.Second
	d.Limits.WorkTime = time.Millisecond
	d.Limits.PrepareTimeout = time.Second
	epoch := uint64(9)
	d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, true }, time.Second)
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
	d.ReadBarrier = func() error {
		d.hub.GrantDeliveryPermit(epoch, time.Now())
		return nil
	}
	putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(25*time.Millisecond).UnixMilli())
	require.Zero(d.dispatchAll())
	require.Equal(1, d.preloadCount)
	// Drain the registration wake so only the due timer could trigger a retry.
	select {
	case <-d.wakeCh:
	default:
	}
	var calls atomic.Int64
	d.ReadBarrier = func() error {
		calls.Add(1)
		return errors.New("renewal unavailable while old permit is still valid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	d.Run(ctx)
	require.EqualValues(1, calls.Load(), "a failed renewal must wait for the normal poll or an external wake, not spin on a cached deadline")
}

func (s *PreloadSuite) TestPreloadRaftReplicasPreserveOwnershipAndFanout() {
	require := s.Require()

	c := newReplicaTestCluster(s.T())
	first := c.subscribe(1, "first", "workers")
	second := c.subscribe(2, "second", "workers")
	universal := c.subscribe(3, "universal", "")
	c.activate()
	due := time.Now().Add(2 * time.Second).UnixMilli()
	value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("future"), EnqueuedAtUnixMs: due - 1000, DelayMs: 1000})
	require.NoError(err)
	bucket, hash := utils.CalculateBucket(due, time.Second), utils.TopicHash("orders")
	cmd, err := raft.MarshalStoreBatchCmd([]raft.StoreBatchItem{
		{ID: 10, Bucket: bucket, TopicHash: hash, Msg: value},
		{ID: 11, Bucket: bucket, TopicHash: hash, Msg: value},
	})
	require.NoError(err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = c.nodes[1].nh.SyncPropose(ctx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(err)
	c.sync()
	for id := uint64(1); id <= 3; id++ {
		d := c.nodes[id].dispatcher
		d.PreloadLookahead, d.PreloadMaxMessages = 10*time.Second, 4
		require.Zero(d.dispatchAll(), "future messages must not prepare or deliver")
		require.NotZero(d.preloadCount, "each replica stages only its local interests")
		for eventID := uint64(10); eventID <= 11; eventID++ {
			state, err := raft.ReadDeliveryState(d.db, utils.EventKey(bucket, hash, eventID))
			require.NoError(err)
			require.Nil(state, "preload does not propose an early manifest")
		}
	}
	require.Empty(first)
	require.Empty(second)
	require.Empty(universal)
	time.Sleep(time.Until(time.UnixMilli(due)))
	for id := uint64(1); id <= 3; id++ {
		c.nodes[id].dispatcher.dispatchAll()
	}
	require.Len(first, 1)
	require.Len(second, 1)
	require.Len(universal, 2)
	firstMessage, secondMessage := <-first, <-second
	require.NotEqual(firstMessage.DeliveryTag, secondMessage.DeliveryTag, "group ownership must remain disjoint")
	universalKeys := [][]byte{(<-universal).DeliveryTag, (<-universal).DeliveryTag}
	require.ElementsMatch([][]byte{firstMessage.DeliveryTag, secondMessage.DeliveryTag}, universalKeys)
}

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

func (s *PreloadSuite) TestFullMessageCacheAvoidsFinalPayloadReadAndOwnsBytes() {
	for _, engine := range []string{"pebble", "bolt"} {
		s.Run(engine, func() {
			require := s.Require()

			d, ledger := newPreloadTest(s.T(), engine)
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			now := time.Now().UnixMilli()
			key := putPipelineEvent(s.T(), d, "orders", 1, now-1000)
			observed := &payloadReadDB{DB: d.db}
			d.db = observed
			ledger.db = observed
			chunk := d.collectScheduledTopic("orders", now)
			require.Len(chunk.items, 1)
			require.Equal([]byte("payload"), chunk.payloads[string(key)].message.Payload)
			require.True(chunk.payloads[string(key)].validity.Valid())
			states, err := d.prepareChunk(context.Background(), chunk)
			require.NoError(err)
			require.EqualValues(1, observed.payloadReads.Load(), "new manifest preparation still verifies existence via Get")
			observed.payloadReads.Store(0)
			require.Equal(1, d.deliverChunk(chunk, states))
			require.Zero(observed.payloadReads.Load(), "valid cached data must avoid final full payload Get/decode")
			require.Equal([]byte("payload"), (<-ch).Payload)
		})
	}
}

func (s *PreloadSuite) TestCachedWorkerCannotBypassDeleteOverwriteOrSnapshot() {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, mutation := range []string{"delete", "overwrite", "snapshot"} {
			s.Run(engine+"/"+mutation, func() {
				require := s.Require()

				d, _ := newPreloadTest(s.T(), engine)
				ch := make(chan *pb.QueueMessage, 1)
				d.hub.Register("local", "orders", "workers", ch)
				key := putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
				chunk := d.collectScheduledTopic("orders", time.Now().UnixMilli())
				states, err := d.prepareChunk(context.Background(), chunk)
				require.NoError(err)
				if mutation == "snapshot" {
					// Real state-machine replacement, including its clearDB phase. Same key,
					// new bytes: old payload must never be authorized by a delayed delete hint.
					var stream bytes.Buffer
					value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("replacement"), EnqueuedAtUnixMs: time.Now().Add(-time.Second).UnixMilli()})
					require.NoError(err)
					for _, record := range []struct{ k, v []byte }{{key, value}} {
						require.NoError(binary.Write(&stream, binary.LittleEndian, uint32(len(record.k))))
						stream.Write(record.k)
						require.NoError(binary.Write(&stream, binary.LittleEndian, uint32(len(record.v))))
						stream.Write(record.v)
					}
					repo, err := repository.NewEventRepository(d.db, zap.NewNop(), time.Millisecond)
					require.NoError(err)
					sm := raft.NewEventStateMachineFactory(d.db, repo, nil, zap.NewNop())(1, 1)
					require.NoError(sm.RecoverFromSnapshot(&stream, make(chan struct{})))
				} else {
					batch := d.db.NewBatch()
					if mutation == "delete" {
						require.NoError(batch.Delete(key))
					} else {
						value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("replacement"), EnqueuedAtUnixMs: time.Now().Add(-time.Second).UnixMilli()})
						require.NoError(err)
						require.NoError(batch.Set(key, value))
					}
					require.NoError(batch.Commit(storage.Sync))
					require.NoError(batch.Close())
				}
				require.False(chunk.payloads[string(key)].validity.Valid())
				sent := d.deliverChunk(chunk, states)
				if mutation == "delete" || mutation == "snapshot" {
					require.Zero(sent)
					require.Empty(ch)
				} else {
					require.Equal(1, sent)
					require.Equal([]byte("replacement"), (<-ch).Payload)
				}
			})
		}
	}
}

func (s *PreloadSuite) TestSlowDiscoveryCannotBlockCachedDeadline() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	d.interval = time.Second
	slow := make(chan *pb.QueueMessage, 1)
	fast := make(chan *pb.QueueMessage, 1)
	d.hub.Register("fast", "b-fast", "workers", fast)
	due := time.Now().Add(120 * time.Millisecond).UnixMilli()
	putPipelineEvent(s.T(), d, "b-fast", 1, due)
	require.Zero(d.dispatchAll())
	require.Equal(1, d.preloadCount)
	d.hub.Register("slow", "a-slow", "workers", slow)
	// Registration changes standalone epoch: refill the cache for that epoch.
	require.Zero(d.dispatchAll())
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
		s.T().Fatal("slow scan did not enter")
	}
	select {
	case msg := <-fast:
		require.Equal("b-fast", msg.Topic)
	case <-time.After(700 * time.Millisecond):
		s.T().Fatal("blocked storage scan delayed cached deadline")
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

func (s *PreloadSuite) TestPoppedHeapSlotsReleasePayloadReferences() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	now := time.Now().UnixMilli()
	putPipelineEvent(s.T(), d, "orders", 1, now+1000)
	require.Empty(d.collectScheduledTopic("orders", now).items)
	p := d.preloads["orders"]
	backing := p.items[:cap(p.items)]
	chunk := d.collectPreloaded("orders", p, d.hub.InterestView("orders"), now+1000)
	require.Len(chunk.items, 1)
	require.NotNil(chunk.payloads[string(chunk.items[0].Key)].message)
	for _, slot := range backing {
		require.Nil(slot.payload.message)
	}
	require.Nil(p.items)
	require.Nil(p.keys)
}

func (s *PreloadSuite) TestZeroPreloadSettingsKeepBoundedDueDelivery() {
	for _, setting := range []string{"lookahead", "count"} {
		s.Run(setting, func() {
			require := s.Require()

			d, _ := newPreloadTest(s.T(), "pebble")
			if setting == "count" {
				d.PreloadMaxMessages = 0
			} else {
				d.PreloadLookahead = 0
			}
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			select {
			case <-ch:
			case <-time.After(time.Second):
				cancel()
				<-done
				s.T().Fatal("zero future settings disabled due delivery")
			}
			cancel()
			<-done
			require.Zero(d.preloadCount)
			require.Zero(d.readyCount)
		})
	}
}

func (s *PreloadSuite) TestCachedWorkerUsesCommittedRecipientACKsAfterPreparation() {
	require := s.Require()

	d, ledger := newPreloadTest(s.T(), "pebble")
	first, second := make(chan *pb.QueueMessage, 2), make(chan *pb.QueueMessage, 2)
	d.hub.Register("a", "orders", "a", first)
	d.hub.Register("b", "orders", "b", second)
	key := putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	chunk := d.collectScheduledTopic("orders", time.Now().UnixMilli())
	states, err := d.prepareChunk(context.Background(), chunk)
	require.NoError(err)
	require.NoError(ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
	require.Equal(1, d.deliverChunk(chunk, states))
	require.Empty(first)
	require.Len(second, 1)
}

func (s *PreloadSuite) TestRemovedGenerationRejectsOldCachedDelivery() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	chunk := d.collectScheduledTopic("orders", time.Now().UnixMilli())
	states, err := d.prepareChunk(context.Background(), chunk)
	require.NoError(err)
	generation := &atomic.Bool{}
	generation.Store(true)
	chunk.generation = generation
	generation.Store(false)
	require.Zero(d.deliverChunk(chunk, states))
	require.Empty(ch)
}

func (s *PreloadSuite) TestAsyncRaftCachePreservesFollowerFanout() {
	require := s.Require()

	c := newReplicaTestCluster(s.T())
	first := c.subscribe(1, "first", "workers")
	second := c.subscribe(2, "second", "workers")
	universal := c.subscribe(3, "universal", "")
	c.activate()
	due := time.Now().Add(500 * time.Millisecond).UnixMilli()
	value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("cached-raft"), EnqueuedAtUnixMs: due - 1000, DelayMs: 1000})
	require.NoError(err)
	cmd, err := raft.MarshalStoreBatchCmd([]raft.StoreBatchItem{{ID: 10, Bucket: utils.CalculateBucket(due, time.Second), TopicHash: utils.TopicHash("orders"), Msg: value}, {ID: 11, Bucket: utils.CalculateBucket(due, time.Second), TopicHash: utils.TopicHash("orders"), Msg: value}})
	require.NoError(err)
	callCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_, err = c.nodes[1].nh.SyncPropose(callCtx, c.nodes[1].nh.GetNoOPSession(replicaTestShard), cmd)
	require.NoError(err)
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
			require.GreaterOrEqual(time.Now().UnixMilli(), due)
			require.Equal([]byte("cached-raft"), msg.Payload)
			return msg
		case <-time.After(3 * time.Second):
			s.T().Fatal("replica did not deliver cached message")
			return nil
		}
	}
	a, b := get(first), get(second)
	u, v := get(universal), get(universal)
	require.NotEqual(a.DeliveryTag, b.DeliveryTag)
	require.ElementsMatch([][]byte{a.DeliveryTag, b.DeliveryTag}, [][]byte{u.DeliveryTag, v.DeliveryTag})
}

func (s *PreloadSuite) TestDiscoveryRejectsResultCapturedBeforeCommittedDelete() {
	require := s.Require()

	d, _ := newPreloadTest(s.T(), "pebble")
	ch := make(chan *pb.QueueMessage, 1)
	d.hub.Register("local", "orders", "workers", ch)
	key := putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
	scanned := d.collectTopic("orders", time.Now().UnixMilli())
	require.Len(scanned.items, 1)
	batch := d.db.NewBatch()
	require.NoError(batch.Delete(key))
	require.NoError(batch.Commit(storage.Sync))
	require.NoError(batch.Close())
	p := &topicPreload{}
	d.admitChunk(p, scanned, nil)
	require.Len(p.items, 1, "uncertain due entries reach worker validation")
	selected := d.collectPreloaded("orders", p, d.hub.InterestView("orders"), time.Now().UnixMilli())
	states, err := d.prepareChunk(context.Background(), selected)
	require.NoError(err)
	require.Empty(states, "the fresh read must reject the deleted message before preparation")
	require.Zero(d.deliverChunk(selected, states))
	require.Empty(ch)
	require.Empty(p.items)
	require.Zero(d.readyCount)
}

func (s *PreloadSuite) TestAsyncOldPreparationCannotReviveReplacedSubscription() {
	require := s.Require()

	d, ledger := newPreloadTest(s.T(), "pebble")
	d.interval = time.Millisecond
	old, newQueue := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
	d.hub.Register("old", "orders", "workers", old)
	putPipelineEvent(s.T(), d, "orders", 1, time.Now().Add(-time.Second).UnixMilli())
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
		s.T().Fatal("preparation did not start")
	}
	d.hub.Unregister("old")
	d.hub.Register("new", "orders", "workers", newQueue)
	close(release)
	select {
	case msg := <-newQueue:
		require.Equal([]byte("payload"), msg.Payload)
	case <-time.After(time.Second):
		s.T().Fatal("replacement subscription was lost behind stale job")
	}
	require.Empty(old, "old job cannot enqueue into an unsubscribed generation")
}

func (s *PreloadSuite) TestFullMessageCacheSizing() {
	if os.Getenv("FUTUREQ_CACHE_MEMORY") == "" {
		s.T().Skip("opt-in retained-cache measurement")
	}
	oldRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = oldRate }()
	for _, size := range []int{1024, 65536, 102400} {
		s.Run(stringSize(size), func() {
			require := s.Require()

			d, _ := newPreloadTest(s.T(), "pebble")
			d.PreloadMaxMessages = 512
			d.PreloadLookahead = time.Minute
			d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
			now := time.Now().UnixMilli()
			due := now + 30000
			value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: make([]byte, size), EnqueuedAtUnixMs: due - 1000, DelayMs: 1000})
			require.NoError(err)
			batch := d.db.NewBatch()
			for i := range d.PreloadMaxMessages {
				require.NoError(batch.Set(utils.EventKey(utils.CalculateBucket(due, d.TimeBucket), utils.TopicHash("orders"), uint64(i)), value))
			}
			require.NoError(batch.Commit(storage.Sync))
			require.NoError(batch.Close())
			require.NoError(d.db.Flush())
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			for i := 0; i < 2000 && d.preloadCount < d.PreloadMaxMessages; i++ {
				require.Empty(d.collectScheduledTopic("orders", now).items)
			}
			require.Equal(512, d.preloadCount)
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			runtime.GC()
			runtime.GC()
			profileBytes := liveCacheAllocationBytes()
			bytes := 0
			for _, p := range d.preloads {
				for _, entry := range p.items {
					bytes += len(entry.payload.message.Payload)
				}
			}
			s.T().Logf("CACHE_MEMORY count=%d payload_size=%d payload_bytes=%d live_heap_delta=%d candidate_struct=%d stored_message_struct=%d cache_profile_inuse_bytes=%d", d.preloadCount, size, bytes, int64(after.HeapAlloc)-int64(before.HeapAlloc), unsafe.Sizeof(preloadCandidate{}), unsafe.Sizeof(storagepb.StoredMessage{}), profileBytes)
			require.Equal(512*size, bytes)
			d.prunePreloads(nil)
			require.Zero(d.preloadCount)
		})
	}
}
func stringSize(n int) string {
	if n == 1024 {
		return "1KiB"
	}
	if n == 65536 {
		return "64KiB"
	}
	return "100KiB"
}

func liveCacheAllocationBytes() int64 {
	n, _ := runtime.MemProfile(nil, true)
	records := make([]runtime.MemProfileRecord, n+100)
	n, _ = runtime.MemProfile(records, true)
	var total int64
	for _, record := range records[:n] {
		frames := runtime.CallersFrames(record.Stack())
		for {
			frame, more := frames.Next()
			if strings.Contains(frame.Function, "dispatcher.cachePayload") {
				total += record.InUseBytes()
				break
			}
			if !more {
				break
			}
		}
	}
	return total
}

// This compares collection at the due instant, including rebuilding the cached
// metadata for each iteration. It excludes preparation, delivery, and the earlier
// speculative scan; it is not a broker throughput benchmark.
func BenchmarkDueCandidateCollection(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "scan"
		if cached {
			name = "preloaded"
		}
		b.Run(name, func(b *testing.B) {
			db, err := storage.NewPebble(config.Pebble{Mode: "memory"}, zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			wake := make(chan struct{}, 1)
			hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), wake)
			hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 64))
			d := NewDispatcher(db, hub, nil, time.Millisecond, time.Second, wake, zap.NewNop())
			d.TimeBucket, d.Limits.WorkTime = time.Millisecond, time.Hour
			now := time.Now().UnixMilli()
			value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: make([]byte, 4096), EnqueuedAtUnixMs: now - 1000})
			if err != nil {
				b.Fatal(err)
			}
			batch := db.NewBatch()
			candidates := make(candidateHeap, 64)
			for i := range candidates {
				key := utils.EventKey(uint64(now-1000), utils.TopicHash("orders"), uint64(i))
				candidates[i] = preloadCandidate{key: [24]byte(key), due: now - 1000}
				if err := batch.Set(key, value); err != nil {
					b.Fatal(err)
				}
			}
			if err := batch.Commit(storage.NoSync); err != nil {
				b.Fatal(err)
			}
			_ = batch.Close()
			view := hub.InterestView("orders")
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var chunk deliveryChunk
				if cached {
					p := &topicPreload{items: append(candidateHeap(nil), candidates...), keys: make(map[[24]byte]bool, len(candidates))}
					for _, candidate := range candidates {
						p.keys[candidate.key] = true
					}
					d.preloadCount = len(candidates)
					chunk = d.collectPreloaded("orders", p, view, now)
				} else {
					chunk = d.collectTopic("orders", now)
				}
				if len(chunk.items) != len(candidates) {
					b.Fatalf("collected %d candidates, expected %d", len(chunk.items), len(candidates))
				}
			}
		})
	}
}
