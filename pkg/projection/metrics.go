package projection

import "github.com/prometheus/client_golang/prometheus"

// Metrics follow the same default-registry pattern as
// pkg/dockerregistry/server/metrics: package-level collectors registered once
// in init.
var (
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "projection",
			Name:      "reconcile_total",
			Help:      "Total ImageStream reconciles by result (created, updated, noop, error).",
		},
		[]string{"result"},
	)

	reconcileLagSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "projection",
			Name:      "lag_seconds",
			Help:      "Latency from event receipt to the ImageStream status write that reflects it.",
			Buckets:   prometheus.DefBuckets,
		},
	)

	eventsDroppedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "projection",
			Name:      "events_dropped_total",
			Help:      "Notification events dropped before enqueue (chaos hook).",
		},
	)
)

func init() {
	prometheus.MustRegister(reconcileTotal, reconcileLagSeconds, eventsDroppedTotal)
}
