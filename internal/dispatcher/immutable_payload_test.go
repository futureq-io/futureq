package dispatcher

import (
	"bytes"
	"context"
	"encoding/binary"
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

func TestImmutableCachedPayloadSurvivesOtherCommits(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		t.Run(engine, func(t *testing.T) {
			d, ledger := newPreloadTest(t, engine)
			ch := make(chan *pb.QueueMessage, 1)
			d.hub.Register("local", "orders", "workers", ch)
			now := time.Now().UnixMilli()
			key := putPipelineEvent(t, d, "orders", 1, now-1000)
			chunk := d.collectScheduledTopic("orders", now)
			// New publications and identical replay never require reloading an
			// already discovered immutable payload, including through wrappers.
			for id := uint64(2); id < 100; id++ {
				putPipelineEvent(t, d, "unrelated", id, now-1000)
			}
			require.Equal(t, key, putPipelineEvent(t, d, "orders", 1, now-1000))
			observed := &payloadReadDB{DB: d.db}
			d.db, ledger.db = observed, observed
			states, err := d.prepareChunk(context.Background(), chunk)
			require.NoError(t, err)
			require.EqualValues(t, 1, observed.payloadReads.Load(), "only the ledger's initial existence check reads the payload")
			observed.payloadReads.Store(0)
			require.Equal(t, 1, d.deliverChunk(chunk, states))
			require.Zero(t, observed.payloadReads.Load())
			require.Equal(t, []byte("payload"), (<-ch).Payload)
		})
	}
}

func TestImmutableCacheRejectsTerminalDeletion(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, stage := range []string{"before-preparation", "after-preparation"} {
			for _, completion := range []string{"final-ack", "ttl-cleanup"} {
				t.Run(engine+"/"+stage+"/"+completion, func(t *testing.T) {
					d, ledger := newPreloadTest(t, engine)
					ch := make(chan *pb.QueueMessage, 1)
					d.hub.Register("local", "orders", "workers", ch)
					now := time.Now().UnixMilli()
					key := putPipelineEvent(t, d, "orders", 1, now-1000)
					// Commit the receipt before discovery, as publication does.
					_, err := ledger.PrepareBatch(context.Background(), []raft.DeliveryPrepare{{Key: key, Epoch: d.hub.InterestView("orders").Epoch, Recipients: []string{"g:workers"}}})
					require.NoError(t, err)
					chunk := d.collectScheduledTopic("orders", now)
					require.Len(t, chunk.items, 1)
					var states map[string]*raft.DeliveryState
					if stage == "after-preparation" {
						states, err = d.prepareChunk(context.Background(), chunk)
						require.NoError(t, err)
					}
					if completion == "final-ack" {
						require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:workers"}}))
					} else {
						require.NoError(t, ledger.DeleteKeys([][]byte{key}))
					}
					if stage == "before-preparation" {
						states, err = d.prepareChunk(context.Background(), chunk)
						require.NoError(t, err)
						require.Nil(t, states[string(key)])
					}
					require.Zero(t, d.deliverChunk(chunk, states))
					require.Empty(t, ch)
				})
			}
		}
	}
}

func TestImmutableCacheUsesRestoredReceipt(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, restore := range []string{"removed", "partial-ack"} {
			t.Run(engine+"/"+restore, func(t *testing.T) {
				d, ledger := newPreloadTest(t, engine)
				first, second := make(chan *pb.QueueMessage, 1), make(chan *pb.QueueMessage, 1)
				d.hub.Register("a", "orders", "a", first)
				d.hub.Register("b", "orders", "b", second)
				now := time.Now().UnixMilli()
				key := putPipelineEvent(t, d, "orders", 1, now-1000)
				chunk := d.collectScheduledTopic("orders", now)
				states, err := d.prepareChunk(context.Background(), chunk)
				require.NoError(t, err)
				var stream bytes.Buffer
				if restore == "partial-ack" {
					value, closer, err := d.db.Get(key)
					require.NoError(t, err)
					value = bytes.Clone(value)
					require.NoError(t, closer.Close())
					require.NoError(t, ledger.Acknowledge([]raft.DeliveryAck{{Key: key, Recipient: "g:a"}}))
					receipt, closer, err := d.db.Get(raft.DeliveryStateKey(key))
					require.NoError(t, err)
					receipt = bytes.Clone(receipt)
					require.NoError(t, closer.Close())
					for _, record := range []struct{ key, value []byte }{{key, value}, {raft.DeliveryStateKey(key), receipt}} {
						require.NoError(t, binary.Write(&stream, binary.LittleEndian, uint32(len(record.key))))
						stream.Write(record.key)
						require.NoError(t, binary.Write(&stream, binary.LittleEndian, uint32(len(record.value))))
						stream.Write(record.value)
					}
				}
				repo, err := repository.NewEventRepository(d.db, zap.NewNop(), d.TimeBucket)
				require.NoError(t, err)
				sm := raft.NewEventStateMachineFactory(d.db, repo, nil, zap.NewNop())(1, 1)
				require.NoError(t, sm.RecoverFromSnapshot(&stream, make(chan struct{})))
				if restore == "removed" {
					require.Zero(t, d.deliverChunk(chunk, states))
					require.Empty(t, second)
				} else {
					require.Equal(t, 1, d.deliverChunk(chunk, states))
					require.Equal(t, []byte("payload"), (<-second).Payload)
				}
				require.Empty(t, first)
			})
		}
	}
}

func TestImmutableCacheChecksElapsedTTL(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, stage := range []string{"preparation", "delivery"} {
			t.Run(engine+"/"+stage, func(t *testing.T) {
				d, _ := newPreloadTest(t, engine)
				ch := make(chan *pb.QueueMessage, 1)
				d.hub.Register("local", "orders", "workers", ch)
				now := time.Now().UnixMilli()
				key := utils.EventKey(utils.CalculateBucket(now, d.TimeBucket), utils.TopicHash("orders"), 1)
				value, err := proto.Marshal(&storagepb.StoredMessage{Topic: "orders", Payload: []byte("payload"), EnqueuedAtUnixMs: now, TtlMs: 200})
				require.NoError(t, err)
				batch := d.db.NewBatch()
				require.NoError(t, batch.Set(key, value))
				require.NoError(t, batch.Commit(storage.Sync))
				require.NoError(t, batch.Close())
				chunk := d.collectScheduledTopic("orders", now)
				require.Len(t, chunk.items, 1)
				var states map[string]*raft.DeliveryState
				if stage == "delivery" {
					states, err = d.prepareChunk(context.Background(), chunk)
					require.NoError(t, err)
				}
				time.Sleep(time.Until(time.UnixMilli(now + 201)))
				if stage == "preparation" {
					states, err = d.prepareChunk(context.Background(), chunk)
					require.NoError(t, err)
					require.Empty(t, states)
				}
				require.Zero(t, d.deliverChunk(chunk, states))
				require.Empty(t, ch)
			})
		}
	}
}

func TestImmutableCacheLoadsMissingPayload(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, stage := range []string{"preparation", "delivery"} {
			t.Run(engine+"/"+stage, func(t *testing.T) {
				d, _ := newPreloadTest(t, engine)
				ch := make(chan *pb.QueueMessage, 1)
				d.hub.Register("local", "orders", "workers", ch)
				now := time.Now().UnixMilli()
				key := putPipelineEvent(t, d, "orders", 1, now-1000)
				chunk := d.collectScheduledTopic("orders", now)
				if stage == "preparation" {
					delete(chunk.payloads, string(key))
				}
				observed := &payloadReadDB{DB: d.db}
				d.db = observed
				states, err := d.prepareChunk(context.Background(), chunk)
				require.NoError(t, err)
				if stage == "delivery" {
					delete(chunk.payloads, string(key))
				}
				require.Equal(t, 1, d.deliverChunk(chunk, states))
				require.EqualValues(t, 1, observed.payloadReads.Load(), "only the cache miss requires a dispatcher payload read")
				require.Equal(t, []byte("payload"), (<-ch).Payload)
			})
		}
	}
}
