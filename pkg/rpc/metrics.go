package rpc

import (
	"sync"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vmkteam/mcpkit/redact"
)

// Metric namespace and subsystem, shared by everything this package exports.
const (
	metricNamespace = "app"
	metricSubsystem = "mcp"
)

// outcome labels on app_mcp_tool_target_calls_total — fixed cardinality, and
// finer than the library's four: a dashboard that cannot tell a refusal from a
// git timeout sends the on-call to look at permissions.
const (
	outcomeOK        = "ok"
	outcomeError     = "error"
	outcomeBadArgs   = "bad-args"
	outcomeForbidden = "forbidden"
	outcomeTimeout   = "timeout"
	// outcomeCached is a call answered from an identical one in the same batch.
	// Its own label, not ok: the two numbers say what deduplication is worth.
	outcomeCached = "cached"
)

var (
	toolCallsVec  *prometheus.CounterVec
	toolCallsOnce sync.Once
)

// toolCalls counts tools/call dispatches with the target they were about. The
// `target` label is a catalogue key or "-", never a value from the request, so
// cardinality stays bounded by the config.
//
// A second series beside mcpkit's app_mcp_tool_calls_total, not a replacement:
// the library counts {tool, outcome}, this adds the dimension only a catalogue
// proxy has. Two names, because one name with two label sets is a registration
// panic.
func toolCalls() *prometheus.CounterVec {
	toolCallsOnce.Do(func() {
		toolCallsVec = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "tool_target_calls_total",
			Help:      "MCP tools/call dispatches by tool, target and outcome.",
		}, []string{"tool", labelTarget, "outcome"})
		prometheus.MustRegister(toolCallsVec)
	})
	return toolCallsVec
}

const labelTarget = "target"

// outcomes a dashboard expects to exist from the start. cached is not among
// them: only api_call can answer from a duplicate, so elsewhere it would be a
// series that can never move.
var knownOutcomes = []string{outcomeOK, outcomeError, outcomeBadArgs, outcomeForbidden, outcomeTimeout}

// outcomeOf maps a tool error code onto the fixed outcome label set. Anything
// unmapped is an error rather than a refusal.
func outcomeOf(code string) string {
	switch code {
	case ErrCodeForbiddenRole, ErrCodePathNotAllowed, ErrCodeMethodNotAllowed, ErrCodeHeaderNotAllowed,
		ErrCodeRPCMethodNotAllowed, ErrCodeQueryParamNotAllowed,
		ErrCodePathDenied, ErrCodeIntentRequired, ErrCodeWriteBudget, ErrCodeWriteNotBatched:
		return outcomeForbidden
	case ErrCodeBadArgs, ErrCodeTargetUnknown, ErrCodeRepoUnknown, ErrCodeRefUnknown,
		ErrCodeAmbiguous, ErrCodeRangeTooBig, ErrCodeNoPrev, ErrCodeJQFailed:
		return outcomeBadArgs
	case ErrCodeTimeout:
		return outcomeTimeout
	default:
		return outcomeError
	}
}

// allTools is every tool this server can dispatch, in one place: initSeries
// pre-creates their series so a dashboard shows zero rather than no data.
var allTools = []string{
	ToolAPICall, ToolRepoMap,
	ToolCodeRead, ToolCodeSearch, ToolCodeHistory, ToolCodeRefs, ToolBlastRadius, ToolWhy,
	ToolDBQuery, ToolDBIntrospect,
	ToolHelp,
}

// initSeries pre-creates the series with zero values: a CounterVec that was
// never observed prints nothing, so a rate() over errors returns no data on a
// healthy service, which an alert cannot tell from a service that stopped.
func initSeries(targets, databases []string) {
	m := toolCalls()
	for _, tool := range allTools {
		for _, o := range knownOutcomes {
			m.WithLabelValues(tool, "-", o)
		}
	}
	m.WithLabelValues(ToolAPICall, "-", outcomeCached)
	for _, t := range targets {
		for _, o := range knownOutcomes {
			m.WithLabelValues(ToolAPICall, t, o)
		}
		m.WithLabelValues(ToolAPICall, t, outcomeCached)
	}
	// The two gauges are materialised here, never set: the manager probed the
	// targets before this service was built, and a zero written now would stand
	// until the next hourly probe — and on the table gauge a zero is the alert.
	for _, d := range databases {
		for _, o := range knownOutcomes {
			m.WithLabelValues(ToolDBQuery, d, o)
			m.WithLabelValues(ToolDBIntrospect, d, o)
			dbQueryDuration().WithLabelValues(d, o)
		}
		dbProven().WithLabelValues(d)
		dbTables().WithLabelValues(d)
	}
}

// DBProven and DBTables read the gauges back, for the test that guards the order
// above.
func DBProven(name string) float64 { return gaugeValue(dbProven(), name) }

func DBTables(name string) float64 { return gaugeValue(dbTables(), name) }

func gaugeValue(vec *prometheus.GaugeVec, name string) float64 {
	var m dto.Metric
	if err := vec.WithLabelValues(name).Write(&m); err != nil {
		return -1
	}
	return m.GetGauge().GetValue()
}

var (
	dbQueryDurationVec *prometheus.HistogramVec
	dbProvenVec        *prometheus.GaugeVec
	dbTablesVec        *prometheus.GaugeVec
	dbMetricsOnce      sync.Once
)

func dbMetrics() {
	dbMetricsOnce.Do(func() {
		dbQueryDurationVec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: "db",
			Name:      "query_duration_seconds",
			Help:      "db_query round trips by target and outcome, the domain's cap included.",
			Buckets:   []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 15, 30},
		}, []string{labelTarget, "outcome"})
		dbProvenVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "db",
			Name:      "target_proven",
			Help:      "1 when the database target passed the read-only proof, 0 while it is unproven.",
		}, []string{labelTarget})
		dbTablesVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "db",
			Name:      "tables",
			Help:      "Tables the target's role can see, as of the last probe; -1 when the probe could not ask.",
		}, []string{labelTarget})
		prometheus.MustRegister(dbQueryDurationVec, dbProvenVec, dbTablesVec)
	})
}

// dbQueryDuration is the histogram behind app_db_query_duration_seconds.
func dbQueryDuration() *prometheus.HistogramVec {
	dbMetrics()
	return dbQueryDurationVec
}

// dbProven is the gauge behind app_db_target_proven.
func dbProven() *prometheus.GaugeVec {
	dbMetrics()
	return dbProvenVec
}

// dbTables is the gauge behind app_db_tables.
func dbTables() *prometheus.GaugeVec {
	dbMetrics()
	return dbTablesVec
}

// SetDBProven records a target's proof state; pkg/app hangs it on the
// manager's OnState.
func SetDBProven(name string, proven bool) {
	v := 0.0
	if proven {
		v = 1
	}
	dbProven().WithLabelValues(name).Set(v)
}

// SetDBTables records how many tables the target's role could see at the last
// probe. A proven target sitting at zero used to be invisible: nothing failed,
// nothing alerted, and every call answered an empty list. It rides the same
// OnState as the proof, so the two agree about which probe they came from.
func SetDBTables(name string, tables int) {
	dbTables().WithLabelValues(name).Set(float64(tables))
}

// RecordDBState is the whole of what one probe publishes. One function rather
// than two calls at each site, so a third gauge cannot be added in one place and
// forgotten in the other.
func RecordDBState(name string, st dbq.State) {
	SetDBProven(name, st.Proven)
	SetDBTables(name, st.Tables)
}

// observeDBQuery records one db_query round trip under the same outcome label
// the call counter got, so a timeout does not share a bucket with an answer.
func observeDBQuery(target string, elapsed time.Duration, outcome string) {
	dbQueryDuration().WithLabelValues(target, outcome).Observe(elapsed.Seconds())
}

// observe records one tools/call dispatch.
func observe(tool, target, outcome string) {
	toolCalls().WithLabelValues(tool, target, outcome).Inc()
}

var (
	redactionsVec  *prometheus.CounterVec
	redactionsOnce sync.Once
)

// redactions counts PII matches by target and rule — the input for choosing the
// final rule set: a rule that never fires is noise, and one that fires on every
// answer is probably matching something else.
func redactions() *prometheus.CounterVec {
	redactionsOnce.Do(func() {
		redactionsVec = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "redactions_total",
			Help:      "PII redaction matches by target and rule.",
		}, []string{labelTarget, "rule"})
		prometheus.MustRegister(redactionsVec)
	})
	return redactionsVec
}

func observeRedactions(target string, r redact.Result) {
	if r.Total() == 0 {
		return
	}
	m := redactions()
	for rule, n := range r.Hits {
		m.WithLabelValues(target, rule).Add(float64(n))
	}
}
