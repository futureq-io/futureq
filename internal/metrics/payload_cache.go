package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	DeliveryWorkerOutcomesTotal = promauto.NewCounterVec(prometheus.CounterOpts{Name: "futureq_delivery_worker_outcomes_total", Help: "Bounded delivery job outcomes; failures and unavailable recipients recover through cyclic scans."}, []string{"outcome"})
	DeliveryRepairScansTotal    = promauto.NewCounter(prometheus.CounterOpts{Name: "futureq_delivery_repair_scans_total", Help: "Bounded cyclic due discovery passes, including initial discovery, retry and late-arrival repair."})
	DeliveryPayloadCacheTotal   = promauto.NewCounterVec(prometheus.CounterOpts{Name: "futureq_delivery_payload_cache_total", Help: "Final delivery payload validations by source: cache or fallback. Not successful sends."}, []string{"source"})
	DeliveryPayloadReadsTotal   = promauto.NewCounter(prometheus.CounterOpts{Name: "futureq_delivery_payload_reads_total", Help: "Final-delivery full payload Get calls, including fallbacks. Excludes manifest and preparation reads."})
	DeliveryPayloadDecodesTotal = promauto.NewCounterVec(prometheus.CounterOpts{Name: "futureq_delivery_payload_decodes_total", Help: "Full payload decode attempts by discovery or final delivery."}, []string{"stage"})
	DeliveryDiscoveryBytesTotal = promauto.NewCounter(prometheus.CounterOpts{Name: "futureq_delivery_discovery_bytes_total", Help: "Event key and value bytes examined by due and future discovery, once per scan."})
)
