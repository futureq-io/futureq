package metrics

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// All histograms use millisecond units.
var deliveryBuckets = []float64{0.1, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 15000, 30000, 60000, 120000}

var (
	// ─── Producer ────────────────────────────────────────────────────────────

	// PublishRequestsTotal counts every PublishBatch frame processed,
	// tagged by outcome so success/error rates can be computed.
	PublishRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "futureq_publish_requests_total",
		Help: "Total publish batch requests, partitioned by outcome.",
	}, []string{"topic", "ack_level", "result"})

	// MessagesPublishedTotal counts individual messages (not batches).
	MessagesPublishedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "futureq_messages_published_total",
		Help: "Total number of messages successfully published.",
	}, []string{"topic", "ack_level"})

	// PublishBatchSize records the distribution of batch sizes.
	PublishBatchSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "futureq_publish_batch_size",
		Help:    "Number of messages in each publish batch.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1..4096
	}, []string{"topic"})

	// PublishLatencyMs measures the full per-batch processing time on the
	// broker, from receiving the frame to just before sending the ack.
	PublishLatencyMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "futureq_publish_latency_ms",
		Help:    "Broker-side publish latency per batch in milliseconds.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 16), // 0.1ms..~6.5s
	}, []string{"topic", "ack_level"})

	// RaftProposeDurationMs measures just the Raft SyncPropose / Propose call.
	RaftProposeDurationMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "futureq_raft_propose_duration_ms",
		Help:    "Latency of Raft SyncPropose calls in milliseconds.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 16), // 0.1ms..~6.5s
	}, []string{"ack_level"})

	// ─── Consumer ────────────────────────────────────────────────────────────

	// ActiveConsumers tracks currently connected consumers.
	ActiveConsumers = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "futureq_active_consumers",
		Help: "Current number of connected consumers.",
	}, []string{"topic", "group_id"})

	// ConsumerAckTotal counts ACK (success=true) and NACK (success=false) frames.
	ConsumerAckTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "futureq_consumer_ack_total",
		Help: "Total number of consumer acknowledgements received.",
	}, []string{"topic", "group_id", "success"})

	// MessagesInFlight tracks messages dispatched but not yet ACKed/NACKed.
	MessagesInFlight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "futureq_messages_in_flight",
		Help: "Current number of dispatched but unacknowledged messages.",
	}, []string{"topic", "group_id"})

	// ─── Dispatcher / delivery ───────────────────────────────────────────────

	// MessagesDispatchedTotal counts each successful send to a consumer channel.
	// For fan-out topics the same message is counted once per recipient.
	MessagesDispatchedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "futureq_messages_dispatched_total",
		Help: "Total number of messages dispatched to consumers.",
	}, []string{"topic", "group_id"})

	// DispatchPassDurationMs measures a bounded chunk, replacing the old full
	// topic pass. Separate scan/preparation/read metrics attribute its cost.
	DispatchPassDurationMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "futureq_dispatch_pass_duration_ms",
		Help:    "Bounded delivery chunk duration from scan start through preparation and queueing in milliseconds (formerly a full scan pass).",
		Buckets: deliveryBuckets,
	})

	ReadBarrierDurationMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "futureq_delivery_read_barrier_duration_ms", Help: "Duration of quorum delivery read barriers in milliseconds.", Buckets: deliveryBuckets,
	})
	DeliveryScanDurationMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "futureq_delivery_scan_duration_ms", Help: "Local scan work per bounded chunk, excluding preparation and read barriers.", Buckets: deliveryBuckets,
	})
	DeliveryPrepareDurationMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "futureq_delivery_prepare_duration_ms", Help: "Preparation wait in milliseconds per batch or candidate (including existing manifests).", Buckets: deliveryBuckets,
	}, []string{"unit"})
	DeliveryPrepareProposalsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "futureq_delivery_prepare_proposals_total", Help: "Number of durable delivery preparation proposals, including retries.",
	})
	DeliveryPrepareKeys = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "futureq_delivery_prepare_keys", Help: "Keys in each durable preparation proposal.", Buckets: []float64{1, 2, 4, 8, 16, 32, 64},
	})
	DeliveryManifestHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "futureq_delivery_manifest_hits_total", Help: "Preparation candidates with an existing manifest in the requested epoch.",
	})
	DeliveryScannedKeysTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "futureq_delivery_scanned_keys_total", Help: "Event keys examined by bounded delivery scans.",
	})
	DeliveryOwnershipFilteredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "futureq_delivery_ownership_filtered_total", Help: "Event keys without a locally owned interest.",
	})
	DeliveryRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "futureq_delivery_rejected_total", Help: "Delivery attempts rejected by permission, queue capacity, or fencing.",
	}, []string{"reason"})
	DeliveryQueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "futureq_delivery_queue_depth", Help: "Current bounded queue depth, summed across local subscriptions or preparation jobs.",
	}, []string{"queue"})
	DeliveryOldestQueuedLatenessMs = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "futureq_delivery_oldest_queued_lateness_ms", Help: "Lateness of the oldest pending local delivery attempt in milliseconds.",
	})
	DeliveryOldestPreparationLatenessMs = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "futureq_delivery_oldest_preparation_lateness_ms", Help: "Oldest due-message lateness in bounded preparation jobs in milliseconds.",
	})
	DeliverySenderEnqueueLatenessMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "futureq_delivery_sender_enqueue_lateness_ms", Help: "Lateness immediately before successfully queueing a delivery attempt for the sender goroutine, relative to its individual due time, in milliseconds; includes retries.", Buckets: deliveryBuckets,
	}, []string{"topic"})
	DeliverySendLatenessMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "futureq_delivery_send_lateness_ms", Help: "Lateness immediately before each gRPC Send attempt, relative to the individual due time, in milliseconds; includes failed send attempts.", Buckets: deliveryBuckets,
	}, []string{"topic"})
	DeliveryGRPCSendsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "futureq_delivery_grpc_sends_total", Help: "Successful gRPC delivery sends.",
	}, []string{"topic"})

	// DeliveryLatencyMs measures the total time from when the producer enqueued
	// the message to when the dispatcher handed it to a consumer. Includes any
	// intentional DelayMs the producer requested.
	DeliveryLatencyMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "futureq_delivery_latency_ms",
		Help:    "Enqueue-to-dispatch latency in milliseconds (includes producer-requested delay).",
		Buckets: prometheus.ExponentialBuckets(1, 2, 16), // 1ms..~65s
	}, []string{"topic"})

	// DeliveryOverheadMs measures how late the dispatch was relative to the
	// message's scheduled delivery time (EnqueuedAt + DelayMs). For messages
	// with no delay this equals DeliveryLatencyMs. A rising p99 here means the
	// dispatcher is falling behind.
	DeliveryOverheadMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "futureq_delivery_overhead_ms",
		Help:    "Scan-start lateness past scheduled delivery time in milliseconds; excludes preparation and sender queue time.",
		Buckets: deliveryBuckets,
	}, []string{"topic"})

	// MessagesExpiredTotal counts messages discarded because their TTL elapsed.
	// source = "dispatcher" or "janitor".
	MessagesExpiredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "futureq_messages_expired_total",
		Help: "Total number of messages discarded due to TTL expiry.",
	}, []string{"topic", "source"})

	// ─── Deleter ─────────────────────────────────────────────────────────────

	// DeleteBatchSize records the distribution of batched deletes.
	DeleteBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "futureq_delete_batch_size",
		Help:    "Number of keys in each batched delete flush.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12),
	})

	// DeleteFailuresTotal counts failed batched delete attempts (will be retried).
	DeleteFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "futureq_delete_failures_total",
		Help: "Total failed batched delete flushes.",
	})
)

// Server wraps the Prometheus HTTP metrics server.
type Server struct {
	addr   string
	logger *zap.Logger
}

// NewServer creates a metrics HTTP server that will expose /metrics.
// addr should be in the form "host:port" (e.g. "0.0.0.0:9090").
func NewServer(addr string, logger *zap.Logger) *Server {
	return &Server{
		addr:   addr,
		logger: logger.Named("metrics"),
	}
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	if s.addr == "" {
		s.logger.Info("metrics server disabled (no listen address configured)")
		<-ctx.Done()
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	go func() {
		s.logger.Info("metrics server listening", zap.String("address", s.addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("metrics server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	s.logger.Info("metrics server: shutting down")
	_ = srv.Shutdown(context.Background()) //nolint:contextcheck
}
