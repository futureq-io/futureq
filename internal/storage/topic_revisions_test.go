package storage

import (
	"testing"

	"github.com/futureq-io/futureq/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestTopicRevisionsBoundRetentionWithoutTrustingEvictedTokens(t *testing.T) {
	var revisions topicRevisions
	quiet := revisions.revision(0)
	for hash := uint64(1); hash <= retainedTopicRevisions; hash++ {
		var writes eventTopicWrites
		writes.add(utils.EventKey(1, hash, 1))
		revisions.commit(writes)
	}
	require.Len(t, revisions.versions, 1, "unobserved writes do not fill the cache")
	require.Equal(t, quiet, revisions.revision(0))
	for hash := uint64(1); hash <= retainedTopicRevisions; hash++ {
		revisions.revision(hash)
	}
	require.Len(t, revisions.versions, retainedTopicRevisions)
	require.NotEqual(t, quiet, revisions.revision(0), "reopening an evicted topic invalidates its old hints")
}
