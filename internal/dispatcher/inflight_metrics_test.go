package dispatcher

import (
	"sync"
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func inFlightGaugeValue(t *testing.T, topic, group string) float64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, metrics.MessagesInFlight.WithLabelValues(topic, group).Write(&metric))
	return metric.GetGauge().GetValue()
}

func TestInFlightGaugeTracksPipelineTimeoutAndDisconnect(t *testing.T) {
	for _, cleanup := range []string{"timeout", "disconnect"} {
		t.Run(cleanup, func(t *testing.T) {
			d, _ := newPipelineTest(t)
			topic := t.Name()
			queue := make(chan *pb.QueueMessage, 3)
			d.hub.Register("consumer", topic, "workers", queue)
			t.Cleanup(func() { d.hub.Unregister("consumer") })
			baseline := inFlightGaugeValue(t, topic, "workers")
			for id := uint64(1); id <= 3; id++ {
				putPipelineEvent(t, d, topic, id, time.Now().Add(-time.Second).UnixMilli())
			}
			require.Equal(t, 3, d.dispatchAll())
			require.Equal(t, baseline+3, inFlightGaugeValue(t, topic, "workers"), "count each dispatch only once")
			if cleanup == "timeout" {
				d.hub.ExpireInFlight(time.Now().Add(2 * time.Second))
				require.Empty(t, d.hub.deliveryRecords["consumer"])
			} else {
				d.hub.Unregister("consumer")
				require.False(t, d.hub.HasConsumers())
				require.Zero(t, d.dispatchAll())
			}
			require.Equal(t, baseline, inFlightGaugeValue(t, topic, "workers"))
			// Later cleanup must not subtract the same attempts again.
			d.hub.Unregister("consumer")
			d.hub.ExpireInFlight(time.Now().Add(time.Minute))
			require.Equal(t, baseline, inFlightGaugeValue(t, topic, "workers"))
		})
	}
}

func TestInFlightGaugeCleanupIsIdempotent(t *testing.T) {
	for _, cleanup := range []string{"delete", "nack", "reject", "remove", "ack"} {
		t.Run(cleanup, func(t *testing.T) {
			hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
			topic := t.Name()
			queue := make(chan *pb.QueueMessage, 1)
			hub.Register("consumer", topic, "workers", queue)
			t.Cleanup(func() { hub.Unregister("consumer") })
			baseline := inFlightGaugeValue(t, topic, "workers")
			key := []byte("key")
			require.NotEmpty(t, hub.DispatchToTopic(topic, &pb.QueueMessage{Topic: topic}, key))
			msg := <-queue
			require.Equal(t, baseline+1, inFlightGaugeValue(t, topic, "workers"))
			if cleanup == "nack" || cleanup == "ack" {
				require.True(t, hub.BeginSend("consumer", msg))
			}
			if cleanup == "ack" {
				require.True(t, hub.MarkAcknowledgedSent("consumer", key))
				require.Equal(t, baseline+1, inFlightGaugeValue(t, topic, "workers"), "pending ACKs still hold a delivery record")
			}
			for i := 0; i < 2; i++ {
				switch cleanup {
				case "delete":
					hub.RemoveDeletedBatch([][]byte{key, key})
				case "nack":
					require.Equal(t, i == 0, hub.AcknowledgeSent("consumer", key))
				case "reject":
					hub.RejectQueuedAttempt("consumer", msg)
				case "remove":
					require.Equal(t, i == 0, hub.RemoveInFlightForConsumer("consumer", key))
				case "ack":
					hub.RemoveAcknowledgedBatch([]raft.DeliveryAck{{Key: key, Recipient: metadata.DeliveryRecipient("workers", "consumer")}})
				}
				require.Equal(t, baseline, inFlightGaugeValue(t, topic, "workers"))
			}
		})
	}
}

func TestInFlightGaugeFanoutAndRetry(t *testing.T) {
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	topic := t.Name()
	for _, id := range []string{"a", "b"} {
		hub.Register(id, topic, "", make(chan *pb.QueueMessage, 2))
		t.Cleanup(func() { hub.Unregister(id) })
	}
	baseline := inFlightGaugeValue(t, topic, "")
	key := []byte("key")
	require.Len(t, hub.DispatchToTopic(topic, &pb.QueueMessage{Topic: topic}, key), 2)
	require.Equal(t, baseline+2, inFlightGaugeValue(t, topic, ""))
	require.Empty(t, hub.DispatchToTopic(topic, &pb.QueueMessage{Topic: topic}, key), "duplicate dispatches cannot inflate the gauge")
	hub.ExpireInFlight(time.Now().Add(time.Minute))
	require.Equal(t, baseline, inFlightGaugeValue(t, topic, ""))
	require.Len(t, hub.DispatchToTopic(topic, &pb.QueueMessage{Topic: topic}, key), 2)
	require.Equal(t, baseline+2, inFlightGaugeValue(t, topic, ""))
	hub.Unregister("a")
	require.Equal(t, baseline+1, inFlightGaugeValue(t, topic, ""), "disconnect must preserve other recipients' count")
	hub.RemoveDeletedBatch([][]byte{key})
	require.Equal(t, baseline, inFlightGaugeValue(t, topic, ""))
}

func TestInFlightGaugeConcurrentCleanup(t *testing.T) {
	hub := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	topic := t.Name()
	hub.Register("consumer", topic, "workers", make(chan *pb.QueueMessage, 1))
	baseline := inFlightGaugeValue(t, topic, "workers")
	key := []byte("key")
	require.NotEmpty(t, hub.DispatchToTopic(topic, &pb.QueueMessage{Topic: topic}, key))
	var workers sync.WaitGroup
	for _, cleanup := range []func(){
		func() { hub.Unregister("consumer") },
		func() { hub.ExpireInFlight(time.Now().Add(time.Minute)) },
		func() { hub.RemoveDeletedBatch([][]byte{key}) },
	} {
		workers.Go(cleanup)
	}
	workers.Wait()
	require.Equal(t, baseline, inFlightGaugeValue(t, topic, "workers"))
}
