package dispatcher

import (
	"time"

	"github.com/futureq-io/futureq/internal/metrics"
)

// Each collection path calls this once, after closing its iterator. Keeping the
// aggregate and path samples together prevents preload work disappearing from
// the existing scan-cost panels. Cache residency time is deliberately excluded.
func observeCollection(path string, started time.Time) {
	elapsed := float64(time.Since(started).Microseconds()) / 1000
	metrics.DeliveryScanDurationMs.Observe(elapsed)
	metrics.DeliveryCollectionDurationMs.WithLabelValues(path).Observe(elapsed)
}

// Only the scheduler accesses preload state. Prometheus scrapes read the gauges,
// never the heap or maps, so collection cannot race with scheduler mutations.
func (d *Dispatcher) observePreload() {
	metrics.DeliveryQueueDepth.WithLabelValues("preload").Set(float64(d.preloadCount))
	capacity := 0
	if d.PreloadLookahead > 0 && d.PreloadMaxMessages > 0 {
		capacity = d.PreloadMaxMessages
	}
	metrics.DeliveryPreloadCapacity.Set(float64(capacity))
}
