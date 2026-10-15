package metadata

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTopicDeliverySnapshotIsImmutableAndPendingIsInactive(t *testing.T) {
	s := &MetadataStateMachine{consumers: ConsumerState{Epoch: 7, Groups: map[string]map[string][]ConsumerMember{
		"orders": {"workers": {{ID: "first", NodeID: 1}, {ID: "second", NodeID: 2}}, "": {{ID: "universal", NodeID: 3}}},
	}}}
	view := s.TopicDeliverySnapshot("orders")
	require.True(t, view.Active)
	require.Equal(t, []string{"g:workers", "u:universal"}, view.Recipients)
	s.consumers.Groups["orders"]["workers"][0].ID = "replacement"
	s.consumers.Epoch++
	s.consumers.Pending = true
	require.Equal(t, "first", view.Groups["workers"][0].ID)
	require.Equal(t, uint64(7), view.Epoch)
	require.False(t, s.TopicDeliverySnapshot("orders").Active)
}
