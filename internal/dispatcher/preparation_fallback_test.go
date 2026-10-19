package dispatcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/repository"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func TestPreparationRereadsUncertainPayloadWithoutRediscovery(t *testing.T) {
	for _, engine := range []string{"pebble", "bolt"} {
		for _, mutation := range []string{"collision", "overwrite-due", "overwrite-future", "expired", "deleted", "unsupported", "snapshot-due", "snapshot-future"} {
			t.Run(engine+"/"+mutation, func(t *testing.T) {
				d, _ := newPreloadTest(t, engine)
				ch := make(chan *pb.QueueMessage, 1)
				d.hub.Register("local", "orders", "workers", ch)
				now := time.Now().UnixMilli()
				key := putPipelineEvent(t, d, "orders", 1, now-1000)
				chunk := d.collectScheduledTopic("orders", now)
				require.Len(t, chunk.items, 1)
				require.True(t, chunk.payloads[string(key)].validity.Valid())
				if mutation == "unsupported" {
					payload := chunk.payloads[string(key)]
					payload.validity = storage.CacheToken{}
					chunk.payloads[string(key)] = payload
				} else if strings.HasPrefix(mutation, "snapshot-") {
					msg := &storagepb.StoredMessage{Topic: "orders", Payload: []byte("replacement"), EnqueuedAtUnixMs: now - 1000}
					if mutation == "snapshot-future" {
						msg.DelayMs = 60000
					}
					value, err := proto.Marshal(msg)
					require.NoError(t, err)
					var stream bytes.Buffer
					require.NoError(t, binary.Write(&stream, binary.LittleEndian, uint32(len(key))))
					stream.Write(key)
					require.NoError(t, binary.Write(&stream, binary.LittleEndian, uint32(len(value))))
					stream.Write(value)
					repo, err := repository.NewEventRepository(d.db, zap.NewNop(), d.TimeBucket)
					require.NoError(t, err)
					sm := raft.NewEventStateMachineFactory(d.db, repo, nil, zap.NewNop())(1, 1)
					require.NoError(t, sm.RecoverFromSnapshot(&stream, make(chan struct{})))
				} else {
					batch := d.db.NewBatch()
					if mutation == "deleted" {
						require.NoError(t, batch.Delete(key))
					} else if mutation == "collision" {
						// Mutate a different event sharing the default tracker slot.
						slot := func(k []byte) uint64 {
							h := uint64(14695981039346656037)
							for _, b := range k {
								h = (h ^ uint64(b)) * 1099511628211
							}
							return h & uint64(config.DefaultPayloadCacheSlots-1)
						}
						var other []byte
						for id := uint64(2); id < 100000; id++ {
							candidate := utils.EventKey(1, utils.TopicHash("unrelated"), id)
							if slot(candidate) == slot(key) {
								other = candidate
								break
							}
						}
						require.NotEmpty(t, other)
						require.NotEqual(t, key, other)
						require.NoError(t, batch.Set(other, []byte("unrelated")))
					} else {
						msg := &storagepb.StoredMessage{Topic: "orders", Payload: []byte("replacement"), EnqueuedAtUnixMs: now - 1000}
						if mutation == "overwrite-future" {
							msg.DelayMs = 60000
						}
						if mutation == "expired" {
							msg.TtlMs = 1
						}
						value, err := proto.Marshal(msg)
						require.NoError(t, err)
						require.NoError(t, batch.Set(key, value))
					}
					require.NoError(t, batch.Commit(storage.Sync))
					require.NoError(t, batch.Close())
				}
				require.False(t, chunk.payloads[string(key)].validity.Valid())
				observed := &payloadReadDB{DB: d.db}
				d.db = scanStartupDB{DB: observed, beforeNewIter: func() { t.Fatal("fallback must not require rediscovery") }}
				fallbacks := metrics.DeliveryPreparePayloadCacheTotal.WithLabelValues("fallback")
				var beforeFallbacks dto.Metric
				require.NoError(t, fallbacks.Write(&beforeFallbacks))
				states, err := d.prepareChunk(context.Background(), chunk)
				require.NoError(t, err)
				var afterFallbacks dto.Metric
				require.NoError(t, fallbacks.Write(&afterFallbacks))
				require.Equal(t, beforeFallbacks.GetCounter().GetValue()+1, afterFallbacks.GetCounter().GetValue(), "preparation reads must be visible even when final delivery is skipped")
				require.EqualValues(t, 1, observed.payloadReads.Load(), "invalid or unsupported proofs must trigger a preparation read")
				if mutation == "deleted" || mutation == "expired" || mutation == "overwrite-future" || mutation == "snapshot-future" {
					require.Empty(t, states)
					state, err := raft.ReadDeliveryState(d.db, key)
					require.NoError(t, err)
					require.Nil(t, state, "ineligible replacements must not freeze recipients")
					require.Zero(t, d.deliverChunk(chunk, states))
					require.Empty(t, ch)
				} else {
					require.NotNil(t, states[string(key)], "eligible payload must prepare on this attempt")
					require.Equal(t, 1, d.deliverChunk(chunk, states))
					want := "payload"
					if mutation == "overwrite-due" || mutation == "snapshot-due" {
						want = "replacement"
					}
					require.Equal(t, want, string((<-ch).Payload))
				}
			})
		}
	}
}

func TestDueAdmissionRetainsUncertainProofForWorkerRead(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
	now := time.Now().UnixMilli()
	key := putPipelineEvent(t, d, "orders", 1, now-1000)
	chunk := d.collectTopic("orders", now)
	// An identical overwrite invalidates the proof without changing eligibility.
	require.Equal(t, key, putPipelineEvent(t, d, "orders", 1, now-1000))
	require.True(t, chunk.payloads[string(key)].validity.Invalidated())
	p := &topicPreload{epoch: d.hub.InterestView("orders").Epoch}
	require.Equal(t, 1, d.admitChunk(p, chunk, nil), "due work must reach worker validation instead of waiting for a rescan")
	selected := d.collectPreloaded("orders", p, d.hub.InterestView("orders"), now)
	states, err := d.prepareChunk(context.Background(), selected)
	require.NoError(t, err)
	require.NotNil(t, states[string(key)])
}
