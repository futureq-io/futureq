package dispatcher

import (
	"fmt"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPreloadPartialDrainBoundsRetainedCapacityAcrossTopics(t *testing.T) {
	d, _ := newPreloadTest(t, "pebble")
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
			require.NoError(t, err)
			require.NoError(t, batch.Set(key, value))
		}
		require.NoError(t, batch.Commit(storage.Sync))
		require.NoError(t, batch.Close())
		d.preloadTopic(topic, p, view, now)
		require.Equal(t, d.PreloadMaxMessages, d.preloadCount)
		require.Len(t, p.items, available)
		oldKeys := p.keys
		seen := make(map[[24]byte]bool)
		for len(p.items) > 1 {
			chunk := d.collectPreloaded(topic, p, view, now+1000)
			require.NotEmpty(t, chunk.items)
			for _, item := range chunk.items {
				key := [24]byte(item.Key)
				require.False(t, seen[key], "compaction must preserve heap contents without duplicates")
				seen[key] = true
			}
		}
		require.Len(t, seen, available-1)
		require.Equal(t, retainedKey, p.items[0].key)
		require.Len(t, p.keys, 1)
		// Go maps do not expose capacity. Prove the oversized map was replaced,
		// rather than merely deleting entries from its existing allocation.
		oldKeys[retainedKey] = false
		require.True(t, p.keys[retainedKey], "partial drain must rebuild the sparse deduplication map")
		retainedSlots := 0
		for _, cached := range d.preloads {
			retainedSlots += cap(cached.items)
		}
		require.LessOrEqual(t, retainedSlots, 4*d.preloadCount, "retained storage must track live candidates across topics")
	}
	require.Equal(t, topics, d.preloadCount)
	for topic, p := range d.preloads {
		// Repeated scanning must still deduplicate the retained future key.
		d.preloadTopic(topic, p, d.hub.InterestView(topic), now+1000)
		require.Len(t, p.items, 1)
		chunk := d.collectPreloaded(topic, p, d.hub.InterestView(topic), now+50000)
		require.Len(t, chunk.items, 1)
		require.Nil(t, p.items)
		require.Nil(t, p.keys)
	}
	require.Zero(t, d.preloadCount)
}
