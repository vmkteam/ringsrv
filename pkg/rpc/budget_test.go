package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/ratelimit"
)

// The budget spares what costs nothing and what a caller needs to learn to
// spend less; every tool that reaches an upstream still pays.
func TestExemptFromBudget(t *testing.T) {
	t.Parallel()
	spared := [][2]string{
		{"tools/call", ToolHelp},
		{"tools/call", ToolRepoMap},
		{"resources/read", "ringsrv://tools/db.md"},
		{"prompts/get", "triage"},
		{"initialize", ""},
		{"ping", ""},
		{"server/discover", ""},
		{"tools/list", ""},
		{"resources/list", ""},
		{"resources/templates/list", ""},
		{"prompts/list", ""},
		{"notifications/initialized", ""},
	}
	for _, c := range spared {
		assert.Truef(t, ExemptFromBudget(c[0], c[1]), "%s %s", c[0], c[1])
	}

	paid := [][2]string{
		{"tools/call", ToolAPICall},
		{"tools/call", ToolDBQuery},
		{"tools/call", ToolCodeSearch},
		{"tools/call", ToolCodeRead},
		{"tools/call", ""},
		{"tools/call", "Help"},
		{"", ""},
	}
	for _, c := range paid {
		assert.Falsef(t, ExemptFromBudget(c[0], c[1]), "%s %s", c[0], c[1])
	}
}

// throughLimiter runs fn the way /mcp runs a tool: inside the limiter's
// middleware, so what the tool charges lands in a budget and in its histogram.
// ctx carries the caller; the budget is an hour, far from spent.
func throughLimiter(t *testing.T, ctx context.Context, fn func(ctx context.Context) mcp.ToolCallResult) mcp.ToolCallResult {
	t.Helper()
	l := ratelimit.New(ratelimit.Config{CostBudgetPerHour: time.Hour})
	t.Cleanup(l.Stop)

	var res mcp.ToolCallResult
	h := l.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		res = fn(r.Context())
	}), embedlog.Logger{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	return res
}

// charges is how many charges app_mcp_ratelimit_charge_seconds holds under a
// label. The series is the process's, so a test reads the difference.
func charges(t *testing.T, label string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "app_mcp_ratelimit_charge_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "label" && l.GetValue() == label {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// Where the budget goes is answered by target: each call of a batch and each
// statement is charged under the name of what it went to. Not parallel: the
// histogram is shared, and the counts are read before and after.
func TestChargesAreLabelledByTarget(t *testing.T) {
	t.Run("api_call", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(upstream.Close)
		svc := batchService(t, upstream.URL, embedlog.Logger{})

		before := charges(t, "test")
		res := throughLimiter(t, ctxWithGroups("ringsrv-users"), func(ctx context.Context) mcp.ToolCallResult {
			r, err := svc.Call(ctx, ToolAPICall, commitCalls(projectPaths(2)...))
			require.NoError(t, err)
			return r
		})
		require.False(t, res.IsError, res.Content[0].Text)
		assert.Equal(t, before+2, charges(t, "test"), "one charge per call of the batch")
	})

	t.Run("db_query and db_introspect", func(t *testing.T) {
		f := newDBFixture(t, embedlog.Logger{})
		before := charges(t, "pg")
		throughLimiter(t, ctxWithGroups("analysts"), func(ctx context.Context) mcp.ToolCallResult {
			r, err := f.Call(ctx, ToolDBQuery, map[string]any{
				"target":  "pg",
				"queries": []any{map[string]any{"sql": "SELECT 1"}, map[string]any{"sql": "SELECT 2"}},
			})
			require.NoError(t, err)
			require.False(t, r.IsError, r.Content[0].Text)
			return r
		})
		assert.Equal(t, before+2, charges(t, "pg"), "one charge per statement")

		throughLimiter(t, ctxWithGroups("analysts"), func(ctx context.Context) mcp.ToolCallResult {
			r, err := f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "users"}))
			require.NoError(t, err)
			require.False(t, r.IsError, r.Content[0].Text)
			return r
		})
		assert.Equal(t, before+3, charges(t, "pg"), "and one per table introspected")
	})
}
