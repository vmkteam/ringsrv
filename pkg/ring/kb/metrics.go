package kb

import (
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// The subsystem is spelled out: promlint reads "kb" in a metric name as
// kilobytes, and a dashboard reader would too.
const (
	metricNamespace = "app"
	metricSubsystem = "knowledge"
)

// The metrics of the pack. Sections are the first segment of a topic's path:
// a label per file would grow with the pack, a label per section stays a
// handful.
type metrics struct {
	topics    *prometheus.GaugeVec
	reads     *prometheus.CounterVec
	unmatched *prometheus.GaugeVec
	info      *prometheus.GaugeVec
}

var (
	metricsOnce sync.Once
	metricsVal  metrics
)

func pm() metrics {
	metricsOnce.Do(func() {
		metricsVal = metrics{
			topics: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "topics",
				Help: "Topics of the knowledge pack by section.",
			}, []string{"section"}),
			reads: prometheus.NewCounterVec(prometheus.CounterOpts{
				Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "reads_total",
				Help: "Topics of the knowledge pack read through help or resources/read, by section.",
			}, []string{"section"}),
			unmatched: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "annotation_unmatched",
				Help: "Columns described by the table notes of a database target that its tables do not have.",
			}, []string{"target"}),
			info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "info",
				Help: "The knowledge pack the instance serves; the version is what the image build wrote.",
			}, []string{"version"}),
		}
		prometheus.MustRegister(metricsVal.topics, metricsVal.reads, metricsVal.unmatched, metricsVal.info)
	})
	return metricsVal
}

// RecordLoaded publishes what the pack holds. Every section's read counter
// starts at zero here: a section nobody reads is the answer the counter exists
// for, and a series that appears with the first read cannot give it.
func RecordLoaded(p *Pack) {
	m := pm()
	counts := map[string]int{}
	for _, t := range p.topics {
		counts[Section(t.Name)]++
	}
	sections := make([]string, 0, len(counts))
	for s := range counts {
		sections = append(sections, s)
	}
	sort.Strings(sections)
	for _, s := range sections {
		m.topics.WithLabelValues(s).Set(float64(counts[s]))
		m.reads.WithLabelValues(s)
	}
	m.info.WithLabelValues(p.version).Set(1)
	seen := map[string]bool{}
	for _, tgt := range p.opts.Sources {
		if !seen[tgt] {
			seen[tgt] = true
			m.unmatched.WithLabelValues(tgt)
		}
	}
}

// RecordRead counts one topic handed out.
func RecordRead(t Topic) {
	pm().reads.WithLabelValues(Section(t.Name)).Inc()
}

// RecordUnmatched sets how many described columns a target's tables lack.
func RecordUnmatched(target string, n int) {
	pm().unmatched.WithLabelValues(target).Set(float64(n))
}
