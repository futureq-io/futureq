package dispatcher

import (
	"context"
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

func TestInFlightGaugeCountsLogicalGroupDeliveriesThroughAckRetry(t *testing.T) {
	d, ledger := newPipelineTest(t)
	h := d.hub
	topic := t.Name()
	queues := make(map[string]chan *pb.QueueMessage)
	for id, group := range map[string]string{"a": "workers", "b": "workers", "archive": "archive", "u": "", "v": ""} {
		queues[id] = make(chan *pb.QueueMessage, 4)
		h.Register(id, topic, group, queues[id])
		t.Cleanup(func() { h.Unregister(id) })
	}
	for id := uint64(1); id <= 3; id++ {
		putPipelineEvent(t, d, topic, id, time.Now().Add(-time.Second).UnixMilli())
	}
	require.Equal(t, 3, d.dispatchAll())
	assertGauges := func(workers, archive, universal float64) {
		t.Helper()
		require.Equal(t, workers, inFlightGaugeValue(t, topic, "workers"))
		require.Equal(t, archive, inFlightGaugeValue(t, topic, "archive"))
		require.Equal(t, universal, inFlightGaugeValue(t, topic, ""))
	}
	assertGauges(3, 3, 6) // One per group/key, plus one per universal recipient/key.
	for range 5 {
		require.Zero(t, d.dispatchAll())
		assertGauges(3, 3, 6)
	}
	// Either member may be first: registration order came from a map.
	owner := "a"
	if len(queues[owner]) == 0 {
		owner = "b"
	}
	msg := <-queues[owner]
	require.True(t, h.BeginSend(owner, msg))
	assertGauges(3, 3, 6) // Queue-to-send transition must not increment again.
	require.True(t, h.MarkAcknowledgedSent(owner, msg.DeliveryTag))
	d.deleter.OnAcknowledged = h.RemoveAcknowledgedBatch
	d.deleter.AcknowledgeBatch = func([]raft.DeliveryAck) error { return context.DeadlineExceeded }
	require.True(t, d.deleter.TryMarkAcknowledged(msg.DeliveryTag, "g:workers"))
	assertGauges(3, 3, 6)
	d.deleter.flush()
	assertGauges(3, 3, 6) // Failed persistence retains the counted attempt.
	d.deleter.AcknowledgeBatch = ledger.Acknowledge
	d.deleter.flush()
	assertGauges(2, 3, 6) // Only the committed recipient decrements.
	d.deleter.flush()
	assertGauges(2, 3, 6)
	for _, queue := range queues {
		for len(queue) > 0 {
			<-queue
		}
	}
	h.ExpireInFlight(time.Now().Add(time.Minute))
	assertGauges(0, 0, 0)
	require.Equal(t, 3, d.dispatchAll())
	assertGauges(2, 3, 6) // Durable ACKs stay completed across timeout/retry.
	for id := range queues {
		h.Unregister(id)
	}
	assertGauges(0, 0, 0)
	h.ExpireInFlight(time.Now().Add(time.Minute))
	assertGauges(0, 0, 0) // Later cleanup cannot drive any series negative.
}

func TestInFlightGaugeConcurrentGroupDispatchAndCleanup(t *testing.T) {
	h := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	topic := t.Name()
	for _, id := range []string{"a", "b", "c"} {
		h.Register(id, topic, "workers", make(chan *pb.QueueMessage, 32))
		t.Cleanup(func() { h.Unregister(id) })
	}
	key := []byte("key")
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() { h.DispatchToTopic(topic, &pb.QueueMessage{Topic: topic}, key) })
	}
	workers.Wait()
	require.Equal(t, float64(1), inFlightGaugeValue(t, topic, "workers"))
	for _, cleanup := range []func(){
		func() { h.Unregister("a"); h.Unregister("b"); h.Unregister("c") },
		func() { h.ExpireInFlight(time.Now().Add(time.Minute)) },
		func() { h.RemoveDeletedBatch([][]byte{key, key}) },
		func() { h.RemoveAcknowledgedBatch([]raft.DeliveryAck{{Key: key, Recipient: "g:workers"}}) },
	} {
		workers.Go(cleanup)
	}
	workers.Wait()
	require.Zero(t, inFlightGaugeValue(t, topic, "workers"))
	require.Empty(t, h.groupInFlight)
}
