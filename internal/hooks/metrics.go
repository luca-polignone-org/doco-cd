package hooks

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// metricsNamespace matches prometheus.MetricsNamespace in internal/prometheus. It is repeated
// here instead of imported so that hooks does not depend on that package and its imports.
const metricsNamespace = "doco_cd"

// The outbox collectors live in the default registry, the one internal/prometheus serves on
// /metrics. They are registered by NewOutbox and not in init(): without HOOK_OUTCOME_URL there
// is no outbox, and /metrics must stay the upstream one.
var (
	outcomeQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "outcome_queue_depth",
		Help:      "Outcome events waiting in the outbox.",
	})
	outcomeDelivered = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "outcome_delivered_total",
		Help:      "Outcome delivery attempts by result.",
	}, []string{"result"})
	outcomeDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "outcome_dropped_total",
		Help:      "Outcome events dropped by reason.",
	}, []string{"reason"})

	// registerer is where NewOutbox registers the collectors; tests swap it for a fresh
	// registry so that they do not depend on what earlier tests left in the default one.
	registerer prometheus.Registerer = prometheus.DefaultRegisterer
	// registerMetricsOnce makes a second NewOutbox a no-op instead of a MustRegister panic.
	registerMetricsOnce = &sync.Once{}
)

func registerMetrics() {
	registerMetricsOnce.Do(func() {
		registerer.MustRegister(outcomeQueueDepth, outcomeDelivered, outcomeDropped)
	})
}
