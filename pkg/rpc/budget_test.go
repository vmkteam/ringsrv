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
		// zenrpc lower-cases a method before it looks it up, so these reach
		// tools/call all the same; spelt any other way, a call pays.
		{"Tools/Call", ToolHelp},
		{"tools.call", ToolHelp},
		{"", ""},
	}
	for _, c := range paid {
		assert.Falsef(t, ExemptFromBudget(c[0], c[1]), "%s %s", c[0], c[1])
	}
}

// throughLimiter calls tool the way /mcp runs it: inside the limiter's
// middleware, so what the tool charges lands in a budget and in its histogram.
// ctx carries the caller; the budget is an hour, far from spent.
func throughLimiter(t *testing.T, ctx context.Context, s ToolsService, tool string, args map[string]any) mcp.ToolCallResult {
	t.Helper()
	l := ratelimit.New(ratelimit.Config{CostBudgetPerHour: time.Hour})
	t.Cleanup(l.Stop)

	var (
		res mcp.ToolCallResult
		err error
	)
	h := l.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		res, err = s.Call(r.Context(), tool, args)
	}), embedlog.Logger{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
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

// Where the budget goes is answered by target: each call of a batch, each
// statement, each window and each repository searched is charged under the name
// of what it went to. Not parallel: the histogram is shared, and the counts are
// read before and after.
func TestChargesAreLabelledByTarget(t *testing.T) {
	t.Run("api_call", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(upstream.Close)
		svc := batchService(t, upstream.URL, embedlog.Logger{})

		before := charges(t, "test")
		throughLimiter(t, ctxWithGroups("ringsrv-users"), svc, ToolAPICall, commitCalls(projectPaths(2)...))
		assert.Equal(t, before+2, charges(t, "test"), "one charge per call of the batch")
	})

	t.Run("db_query and db_introspect", func(t *testing.T) {
		f := newDBFixture(t, embedlog.Logger{})
		ctx := ctxWithGroups("analysts")
		before := charges(t, "pg")
		throughLimiter(t, ctx, f.ToolsService, ToolDBQuery, map[string]any{
			"target":  "pg",
			"queries": []any{map[string]any{"sql": "SELECT 1"}, map[string]any{"sql": "SELECT 2"}},
		})
		assert.Equal(t, before+2, charges(t, "pg"), "one charge per statement")

		throughLimiter(t, ctx, f.ToolsService, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "users"}))
		assert.Equal(t, before+3, charges(t, "pg"), "and one per table introspected")
	})

	t.Run("code_read", func(t *testing.T) {
		s, sha := codeFixture(t)
		before := charges(t, "apisrv")
		throughLimiter(t, ctxWithGroups("ringsrv-developers"), s, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha,
			"windows": []any{"internal/rpc/order.go:1-3", "internal/rpc/order.go:5-8", "internal/rpc/order.go:1-3"},
		})
		assert.Equal(t, before+2, charges(t, "apisrv"), "one charge per window read, a repeat read once")
	})

	// The repositories are searched at once, so the wall clock is the slowest
	// of them; each pays its own part, the one without the commit included.
	t.Run("code_search", func(t *testing.T) {
		s, sha := searchFixture(t)
		a, b := charges(t, "a"), charges(t, "b")
		throughLimiter(t, ctxWithGroups("ringsrv-developers"), s, ToolCodeSearch, map[string]any{
			"query": "func Needle", "ref": sha, "repos": []any{"a", "b"},
		})
		assert.Equal(t, a+1, charges(t, "a"))
		assert.Equal(t, b+1, charges(t, "b"), "skipped, and still paid: finding that out is work")
	})

	t.Run("why", func(t *testing.T) {
		tracker := youtrack(t)
		t.Cleanup(tracker.Close)
		s, sha := whyFixture(t, tracker, "youtrack", "")
		repo, issues := charges(t, "apisrv"), charges(t, "youtrack")
		throughLimiter(t, ctxWithGroups("ringsrv-developers"), s, ToolWhy, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "line": 4,
		})
		assert.Equal(t, repo+1, charges(t, "apisrv"), "the blame under the repository")
		assert.Equal(t, issues+1, charges(t, "youtrack"), "the task under the tracker it came from")
	})
}

// assertCarriesBudget calls tool through a limiter and then without one: the
// first answer says where the budget stands, the second says nothing of it. The
// field is read from the JSON the model gets, whatever the answer's type.
func assertCarriesBudget(t *testing.T, s ToolsService, group, tool string, args map[string]any) {
	t.Helper()
	ctx := ctxWithGroups(group)
	res := throughLimiter(t, ctx, s, tool, args)
	b := decodeText[struct {
		Budget *Budget `json:"budget"`
	}](t, res).Budget
	require.NotNil(t, b, res.Content[0].Text)
	assert.Equal(t, "1h", b.Limit)
	assert.NotEmpty(t, b.Used)
	assert.NotEmpty(t, b.Left)
	assert.NotEmpty(t, b.ResetIn)

	res, err := s.Call(ctx, tool, args)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	assert.NotContains(t, res.Content[0].Text, `"budget"`, "no limiter, no budget to report")
}

// Every answer that spends the budget says where it stands, and so does help,
// the call a caller makes to find out. Without a budget the field is not there
// at all. Not parallel: the subtests share the fixtures, one fake database
// among them.
func TestAnswersCarryTheBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	tracker := youtrack(t)
	t.Cleanup(tracker.Close)

	api := batchService(t, upstream.URL, embedlog.Logger{})
	db := newDBFixture(t, embedlog.Logger{})
	src, sha := codeFixture(t)
	hist, _, _, head := historyFixture(t)
	why, whySHA := whyFixture(t, tracker, "youtrack", "")
	const dev = "ringsrv-developers"

	calls := map[string]struct {
		svc   ToolsService
		group string
		tool  string
		args  map[string]any
	}{
		ToolAPICall:      {api, "ringsrv-users", ToolAPICall, commitCalls(projectPaths(1)...)},
		ToolDBQuery:      {db.ToolsService, "analysts", ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT 1"})},
		ToolDBIntrospect: {db.ToolsService, "analysts", ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "users"})},
		ToolHelp:         {db.ToolsService, "analysts", ToolHelp, map[string]any{}},
		ToolCodeRead:     {src, dev, ToolCodeRead, map[string]any{"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go"}},
		"code_read windows": {src, dev, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "windows": []any{"internal/rpc/order.go:1-3"},
		}},
		ToolCodeSearch: {src, dev, ToolCodeSearch, map[string]any{"query": "Create", "ref": sha}},
		ToolCodeHistory: {hist, dev, ToolCodeHistory, map[string]any{
			"repo": "apisrv", "mode": "lines", "ref": head, "path": "internal/rpc/order.go", "line_from": 3, "line_to": 5,
		}},
		ToolWhy:              {why, dev, ToolWhy, map[string]any{"repo": "apisrv", "ref": whySHA, "path": "internal/rpc/order.go", "line": 4}},
		"why without a task": {why, dev, ToolWhy, map[string]any{"repo": "apisrv", "ref": whySHA, "path": "internal/rpc/plain.go", "line": 3}},
	}
	for name, c := range calls {
		t.Run(name, func(t *testing.T) {
			assertCarriesBudget(t, c.svc, c.group, c.tool, c.args)
		})
	}
}

// The tools behind the AST engine answer the same way. Apart, so that a machine
// without the engine skips them alone.
func TestEngineAnswersCarryTheBudget(t *testing.T) {
	t.Parallel()
	t.Run(ToolCodeRefs, func(t *testing.T) {
		t.Parallel()
		s, sha := refsFixture(t)
		assertCarriesBudget(t, s, "ringsrv-developers", ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID", "path": "internal/db/order.go",
		})
	})
	t.Run(ToolBlastRadius, func(t *testing.T) {
		t.Parallel()
		s, prev, release := blastFixture(t)
		assertCarriesBudget(t, s, "ringsrv-developers", ToolBlastRadius, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
		})
	})
}

// The budget in an answer has that answer's own work in it, summed over what
// ran at once: two calls of 750ms each run side by side, the wall clock says
// under a second, and used says two. Taken before the calls were charged it
// would say 0s, and priced by the wall clock 1s.
func TestBudgetCountsTheCallsOwnWork(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(750 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	svc := batchService(t, upstream.URL, embedlog.Logger{})

	res := throughLimiter(t, ctxWithGroups("ringsrv-users"), svc, ToolAPICall, commitCalls(projectPaths(2)...))
	b := decodeBatch(t, res).Budget
	require.NotNil(t, b, res.Content[0].Text)
	assert.Equal(t, "2s", b.Used)
}

func TestRoughly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"},
		{1400 * time.Millisecond, "1s"},
		{20 * time.Minute, "20m"},
		{12*time.Minute + 30*time.Second, "12m30s"},
		{time.Hour, "1h"},
		{time.Hour + 5*time.Minute, "1h5m"},
		{59*time.Minute + 59*time.Second + 600, "59m59s"},
	}
	for _, tc := range tests {
		assert.Equalf(t, tc.want, roughly(tc.d), "%d", tc.d)
	}
}
