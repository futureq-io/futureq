package storage

import (
	"testing"

	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestTopicRevisionsRemainStableWhileWatched(t *testing.T) {
	var revisions topicRevisions
	const topics = 4097
	watches := make([]*TopicRevision, topics)
	tokens := make([]uint64, topics)
	for i := range watches {
		watches[i] = revisions.watch(uint64(i))
		tokens[i] = watches[i].Read(nil)
	}
	for range 3 {
		for i, watch := range watches {
			require.Equal(t, tokens[i], watch.Read(nil))
		}
	}
	for hash := uint64(topics); hash < 2*topics; hash++ {
		var writes eventTopicWrites
		writes.add(utils.EventKey(1, hash, 1))
		revisions.commit(writes)
	}
	require.Len(t, revisions.active, topics, "unobserved writes do not add retained topics")
	secondWatch := revisions.watch(0)
	for i, watch := range watches {
		require.Equal(t, tokens[i], watch.Read(nil))
		watch.Close()
	}
	require.Len(t, revisions.active, 1)
	require.Equal(t, tokens[0], secondWatch.Read(nil))
	secondWatch.Close()
	require.Empty(t, revisions.active)
	reopened := revisions.watch(0)
	defer reopened.Close()
	require.NotEqual(t, tokens[0], reopened.Read(nil))
}
