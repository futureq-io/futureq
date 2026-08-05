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

	// DispatchPassDurationMs measures one full dispatcher scan pass.
	DispatchPassDurationMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "futureq_dispatch_pass_duration_ms",
		Help:    "Duration of each dispatcher scan pass in milliseconds.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 16),
	})

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
		Help:    "Dispatch lateness past scheduled delivery time in milliseconds.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 16), // 0.1ms..~6.5s
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
