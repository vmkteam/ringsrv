package git

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// The name is built from the same parts every other metric in this service
// uses, rather than spelled out: a literal is the one that would not follow if
// the namespace ever changed.
const (
	metricNamespace = "app"
	metricSubsystem = "mirror"
)

// staleness is the first metric to look at when a code tool says a commit does
// not exist: either the mirror never caught up, or the webhook stopped
// arriving and the cron is carrying the service alone.
//
// It is a collector rather than a gauge that something updates on a timer:
// the value is "how long ago", which is a function of when it is read.
type stalenessCollector struct {
	store *Store
	desc  *prometheus.Desc
}

func (c stalenessCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c stalenessCollector) Collect(ch chan<- prometheus.Metric) {
	for repo, age := range c.store.Staleness() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, age.Seconds(), repo)
	}
}

var metricsOnce sync.Once

// RegisterMetrics publishes the staleness of this store's mirrors. Called by
// the app once the store exists; safe to call more than once.
func (s *Store) RegisterMetrics() {
	metricsOnce.Do(func() {
		prometheus.MustRegister(stalenessCollector{
			store: s,
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(metricNamespace, metricSubsystem, "last_fetch_seconds"),
				"Seconds since the last successful fetch of a mirror.",
				[]string{"repo"}, nil,
			),
		})
	})
}
