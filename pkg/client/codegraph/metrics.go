package codegraph

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Indexing a commit costs seconds — the same case as the first worktree of a
// commit. It is published rather than hidden: an unexplained pause
// on the first question about a release is how a working tool gets a
// reputation for being slow.
var (
	metricsOnce sync.Once

	indexSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "app",
		Subsystem: "codegraph",
		Name:      "index_seconds",
		Help:      "Time spent building an AST index for one worktree.",
		Buckets:   []float64{0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"result"})
)

func observeIndex(d time.Duration, ok bool) {
	metricsOnce.Do(func() { prometheus.MustRegister(indexSeconds) })

	result := "error"
	if ok {
		result = "ok"
	}
	indexSeconds.WithLabelValues(result).Observe(d.Seconds())
}
