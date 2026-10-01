package dispatcher

import (
	"testing"

	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

func TestLargerConsumerQueueCanFillPastLegacyTrackingLimit(t *testing.T) {
	d, _ := newPipelineTest(t)
	ch := make(chan *pb.QueueMessage, 2048)
	d.hub.Register("local", "orders", "workers", ch)
	for id := uint64(0); id < 2048; id++ {
		require.NotEmpty(t, d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, utils.EventKey(1, 2, id)))
	}
	require.Len(t, ch, 2048)
	require.Len(t, d.hub.deliveryRecords["local"], 2048)
	require.Empty(t, d.hub.DispatchToTopic("orders", &pb.QueueMessage{}, utils.EventKey(1, 2, 2048)))
}
