package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
)

// batchService is a service over one test upstream that allows the paths a
// fan-out uses, so a batch can be exercised end to end over real HTTP.
func batchService(t *testing.T, baseURL string, l embedlog.Logger) ToolsService {
	t.Helper()
	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.test]
Description  = "dev · test upstream"
BaseURL      = "` + baseURL + `"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/projects/\\d+/commits$", "^/api/v1/slow$", "^/api/v1/fail$"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["test"]
`))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets:       cat,
		Upstream:      upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, MaxConcurrent: 8}),
		JQTimeout:     time.Second,
		MaxBytes:      32768,
		MaxConcurrent: 8,
		Sessions:      ring.NewSessions(time.Hour, nil),
		Logger:        l,
	})
}

func commitCalls(paths ...string) map[string]any {
	calls := make([]any, len(paths))
	for i, p := range paths {
		calls[i] = map[string]any{"target": "test", "method": "GET", "path": p}
	}
	return map[string]any{"calls": calls}
}

func projectPaths(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("/api/v1/projects/%d/commits", i+1)
	}
	return out
}

// The fan-out the batching exists for: the log has 28 calls to
// repository/commits over 23 projects inside one step, each its own round trip.
// One call, one answer per project, in the order asked.
func TestBatch_FanOut(t *testing.T) {
	t.Parallel()
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	s := batchService(t, srv.URL, embedlog.Logger{})
	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, commitCalls(projectPaths(maxAPICallBatch)...))
	require.NoError(t, err)

	out := decodeBatch(t, res)
	require.Len(t, out.Results, maxAPICallBatch)
	assert.EqualValues(t, maxAPICallBatch, served.Load(), "one upstream call per item, no more")
	for i, item := range out.Results {
		require.Nil(t, item.Error, "item %d", i)
		assert.Equal(t, i, item.Index, "answers come back in the order sent")
		assert.Equal(t, http.StatusOK, item.Status)
		data, ok := item.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("/api/v1/projects/%d/commits", i+1), data["path"],
			"answer %d belongs to call %d", i, i)
	}

	// A list longer than the cap is refused rather than trimmed: a trimmed list
	// looks answered, and nobody notices the questions that fell off.
	res, err = s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, commitCalls(projectPaths(maxAPICallBatch+1)...))
	require.NoError(t, err)
	e := decodeErr(t, res)
	assert.Equal(t, ErrCodeBadArgs, e.Code)
	assert.Contains(t, e.Message, "the limit is 20")

	// And so is an empty one, with the hint that this argument is a list: a
	// caller still sending the old scalar arrives here.
	res, _ = s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, map[string]any{"calls": []any{}})
	assert.Contains(t, decodeErr(t, res).Message, "not a single call")
}

// One failure is one item's failure. A batch marked as an error is a batch the
// model sends again in full, including the items that already answered.
func TestBatch_PartialFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/fail" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream is unhappy"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	paths := projectPaths(9)
	// The seventh call fails, the sixth asks for a path the profile does not
	// allow, and the fifth names a target nobody has.
	paths[6] = "/api/v1/fail"
	paths[5] = "/api/v1/admin"
	args := commitCalls(paths...)
	args["calls"].([]any)[4] = map[string]any{"target": "nope", "method": "GET", "path": "/api/v1/projects/5/commits"}

	res, err := batchService(t, srv.URL, embedlog.Logger{}).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, args)
	require.NoError(t, err)
	assert.False(t, res.IsError, "a batch that answered anything is not an error")

	out := decodeBatch(t, res)
	require.Len(t, out.Results, 9)
	require.NotNil(t, out.Results[4].Error)
	assert.Equal(t, ErrCodeTargetUnknown, out.Results[4].Error.Code)
	require.NotNil(t, out.Results[5].Error)
	assert.Equal(t, ErrCodePathNotAllowed, out.Results[5].Error.Code, "the allowlist is checked per item")
	assert.NotEmpty(t, out.Results[5].Error.Paths, "and the refusal still says what is allowed")
	require.NotNil(t, out.Results[6].Error)
	assert.Equal(t, ErrCodeUpstream, out.Results[6].Error.Code)
	assert.Equal(t, http.StatusBadGateway, out.Results[6].Status)

	for _, i := range []int{0, 1, 2, 3, 7, 8} {
		assert.Nilf(t, out.Results[i].Error, "item %d answered and must not be affected", i)
	}
}

// Identity and per-target counts are properties of a call, not of a list: a
// single record for a batch of twenty would hide nineteen.
func TestBatch_AuditsEveryItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	recs := captureLog(t, func(l embedlog.Logger) {
		_, err := batchService(t, srv.URL, l).Call(ctxWithUser("alice", "ringsrv-users"), ToolAPICall,
			commitCalls(projectPaths(3)...))
		require.NoError(t, err)
	})

	items := itemRecords(recs)
	require.Len(t, items, 3, "one record per call the batch made")
	paths := make([]string, len(items))
	for i, rec := range items {
		paths[i], _ = rec["query"].(string)
		assert.Equal(t, "alice", rec["sub"])
		assert.Equal(t, "viewer", rec["roles"], "which role answered is the question the audit exists for")
		assert.EqualValues(t, 3, rec["batch_size"])
		assert.Equal(t, audit.DecisionAllow, rec["decision"])
	}
	assert.ElementsMatch(t, projectPaths(3), paths, "every path is on record, whatever order they finished in")

	calls := callRecords(recs)
	require.Len(t, calls, 1, "and one record for the tool call that carried them")
	assert.EqualValues(t, 3, calls[0]["batch_size"])
	assert.Positive(t, calls[0]["bytes_out"], "the call's record measures the whole answer")
}

// The model repeats itself inside one turn as readily as across turns: 42
// requests in the log were repeated verbatim within a single trace.
func TestBatch_DeduplicatesIdenticalCalls(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var res mcp.ToolCallResult
	recs := captureLog(t, func(l embedlog.Logger) {
		var err error
		res, err = batchService(t, srv.URL, l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, commitCalls(
			"/api/v1/projects/1/commits",
			"/api/v1/projects/2/commits",
			"/api/v1/projects/1/commits",
		))
		require.NoError(t, err)
	})

	assert.EqualValues(t, 2, served.Load(), "the repeat is answered from the first call, not fetched again")
	out := decodeBatch(t, res)
	require.Len(t, out.Results, 3)
	require.NotNil(t, out.Results[2].DedupOf)
	assert.Equal(t, 0, *out.Results[2].DedupOf, "and says which answer it is a copy of")
	assert.Equal(t, 2, out.Results[2].Index, "while keeping its own place in the list")
	assert.Nil(t, out.Results[0].DedupOf)

	// It is still the caller's call, so it is still in the log — marked as
	// having made no request of its own.
	var deduped int
	for _, rec := range itemRecords(recs) {
		if rec["cache"] == cacheDedup {
			deduped++
		}
	}
	assert.Equal(t, 1, deduped)
}

// A write is sent on its own: a list is how one injected instruction spends the
// whole session budget in a single turn.
func TestBatch_RefusesWritesInAList(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)
	s := writeService(t, srv.URL, 5)
	ctx := ctxWithGroups("ringsrv-oncall")

	write := map[string]any{
		"target": "youtrack-rw", "method": "POST", "path": "/api/issues",
		"body": `{"summary":"x"}`, "intent": "создаю задачу по инциденту",
	}
	read := map[string]any{"target": "youtrack", "method": "GET", "path": "/api/issues"}

	res, err := s.Call(ctx, ToolAPICall, map[string]any{"calls": []any{read, write}})
	require.NoError(t, err)
	e := decodeErr(t, res)
	assert.Equal(t, ErrCodeWriteNotBatched, e.Code,
		"its own code: a model told it needs an intent would add one and be refused again")
	assert.Contains(t, e.Message, "calls[1]", "the refusal names the item that has to be sent on its own")
	assert.Zero(t, calls.Load(), "and nothing in the list ran, the read included")

	// Alone it goes through, and a GET on the write profile is not a write.
	res, err = s.Call(ctx, ToolAPICall, map[string]any{"calls": []any{write}})
	require.NoError(t, err)
	require.Nil(t, decodeBatch(t, res).Results[0].Error)
	assert.EqualValues(t, 1, calls.Load())

	res, err = s.Call(ctx, ToolAPICall, map[string]any{"calls": []any{
		read, map[string]any{"target": "youtrack-rw", "method": "GET", "path": "/api/issues"},
	}})
	require.NoError(t, err)
	require.Len(t, decodeBatch(t, res).Results, 2)
	assert.EqualValues(t, 3, calls.Load())
}

// The budget belongs to the answer, not to each call in it: twenty answers at
// the per-target default would be megabytes of context.
func TestBatch_SharesOneByteBudget(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Project 1 answers wide, the others narrowly.
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects/1/") {
			_, _ = fmt.Fprintf(w, `{"text":%q}`, strings.Repeat("x", 4000))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	args := commitCalls(projectPaths(4)...)
	args["max_bytes"] = 1000

	res, err := batchService(t, srv.URL, embedlog.Logger{}).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, args)
	require.NoError(t, err)
	out := decodeBatch(t, res)
	require.Len(t, out.Results, 4)

	assert.True(t, out.Truncated, "the batch says its budget did not carry everything")
	require.NotNil(t, out.Results[0].Error, "the wide answer is the one dropped")
	assert.Equal(t, ErrCodeTruncated, out.Results[0].Error.Code)
	assert.Contains(t, out.Results[0].Error.Message, "narrow this call's jq")
	assert.Nil(t, out.Results[0].Data, "dropped whole: half a JSON document is a parse error, not a smaller answer")
	for _, i := range []int{1, 2, 3} {
		assert.Nilf(t, out.Results[i].Error, "a narrow answer is never cut to make room for one that will not fit anyway (item %d)", i)
	}
}

// Independent calls overlap. The upstream client's semaphore is shared by every
// caller, so a batch takes only a few slots of it at a time.
func TestBatch_RunsInParallelWithinLimit(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var inflight, peak int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	res, err := batchService(t, srv.URL, embedlog.Logger{}).Call(ctxWithGroups("ringsrv-users"), ToolAPICall,
		commitCalls(projectPaths(8)...))
	require.NoError(t, err)
	elapsed := time.Since(start)

	require.Len(t, decodeBatch(t, res).Results, 8)
	mu.Lock()
	defer mu.Unlock()
	assert.Greater(t, peak, 1, "the calls of a batch overlap")
	assert.LessOrEqual(t, peak, defaultBatchWorkers, "but never more than the worker limit at once")
	assert.Less(t, elapsed, 8*30*time.Millisecond, "which is the whole point: eight sequential calls would take longer")
}

// db_query stops at the first failure: a list of statements is usually one line
// of reasoning, and continuing after the premise failed costs work nobody uses.
func TestBatch_DBQueryStopsAtFirstError(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})
	var ran atomic.Int64
	f.pg.SetQuery(func(st dbq.Statement) (*dbq.Result, error) {
		if n := ran.Add(1); n == 2 {
			return nil, errors.New(`ERROR #42703 column "nmae" does not exist`)
		}
		return dbqtest.Rows(1)(st)
	})

	res, err := f.Call(ctxWithGroups("pg-readers"), ToolDBQuery, map[string]any{
		"target": "pg",
		"queries": []any{
			map[string]any{"sql": "SELECT 1"},
			map[string]any{"sql": "SELECT nmae FROM users"},
			map[string]any{"sql": "SELECT 3"},
		},
	})
	require.NoError(t, err)

	out := decodeText[DBQueryBatch](t, res)
	require.Len(t, out.Results, 3)
	assert.Nil(t, out.Results[0].Error, "the statement before the failure answered")
	require.NotNil(t, out.Results[1].Error)
	assert.False(t, out.Results[1].Skipped)
	assert.True(t, out.Results[2].Skipped, "and the rest is not run")
	assert.Nil(t, out.Results[2].Error, "a skipped statement has no error of its own to report")
	assert.EqualValues(t, 2, ran.Load(), "the third never reached the database")

	// A list longer than the cap is refused before anything runs.
	res, err = f.Call(ctxWithGroups("pg-readers"), ToolDBQuery, map[string]any{
		"target":  "pg",
		"queries": []any{map[string]any{"sql": "SELECT 1"}, map[string]any{"sql": "SELECT 2"}, map[string]any{"sql": "SELECT 3"}, map[string]any{"sql": "SELECT 4"}, map[string]any{"sql": "SELECT 5"}, map[string]any{"sql": "SELECT 6"}},
	})
	require.NoError(t, err)
	assert.Equal(t, ErrCodeBadArgs, decodeErr(t, res).Code)
}

// db_introspect answers several tables at once, and a name that does not
// resolve is that item's error rather than the batch's.
func TestBatch_DBIntrospectTables(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})

	res, err := f.Call(ctxWithGroups("pg-readers"), ToolDBIntrospect, map[string]any{
		"target": "pg", "tables": []any{"users", "userz", "billing.invoices"},
	})
	require.NoError(t, err)

	out := decodeText[DBSchemaBatch](t, res)
	require.Len(t, out.Results, 3)
	assert.Equal(t, "dev", out.Env)
	assert.Equal(t, "postgres", out.Driver)
	require.NotNil(t, out.Results[0].Schema)
	assert.Equal(t, []string{"id"}, out.Results[0].Schema.PrimaryKey)
	require.NotNil(t, out.Results[1].Error, "a table nobody has is that item's refusal")
	assert.Equal(t, ErrCodeBadArgs, out.Results[1].Error.Code)
	require.NotNil(t, out.Results[2].Schema, "and the table after it still answers")
	assert.Equal(t, "billing.invoices", out.Results[2].Table)
}

// shareBudget is the rule the api_call budget follows: smallest first, the
// unspent share carried forward, and never an empty answer.
func TestShareBudget(t *testing.T) {
	t.Parallel()

	t.Run("everything fits", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []bool{true, true, true}, shareBudget([]int{10, 20, 30}, 100))
	})

	t.Run("the wide one starves only itself", func(t *testing.T) {
		t.Parallel()
		// 900 leaves room for the three small ones and not for the big one.
		assert.Equal(t, []bool{false, true, true, true}, shareBudget([]int{800, 100, 100, 100}, 400))
	})

	t.Run("the smallest always fits", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []bool{true}, shareBudget([]int{5000}, 10),
			"each item was already cut to its own limit; a batch of one must not come back empty")
		assert.Equal(t, []bool{false, true}, shareBudget([]int{5000, 4000}, 0))
	})

	t.Run("items with nothing to weigh are skipped", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []bool{false, true}, shareBudget([]int{-1, 10}, 100))
	})
}

// parallelMap keeps the order of the input whatever order the work finishes in,
// and never runs more than the worker limit at once.
func TestParallelMap(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var inflight, peak int

	in := make([]int, 12)
	for i := range in {
		in[i] = i
	}
	out := parallelMap(in, 3, func(i, v int) string {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return fmt.Sprintf("%d:%d", i, v)
	})

	require.Len(t, out, 12)
	for i, got := range out {
		assert.Equal(t, fmt.Sprintf("%d:%d", i, i), got)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.LessOrEqual(t, peak, 3)
	assert.Greater(t, peak, 1)

	assert.Empty(t, parallelMap([]int{}, 3, func(int, int) string { return "" }))
}

// The batch histogram is what says whether clients actually batch: its _count
// is the number of tools/call dispatches, which the per-item call counter is
// no longer.
func TestBatch_MetricCountsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	before := batchStats(t)
	_, err := batchService(t, srv.URL, embedlog.Logger{}).Call(ctxWithGroups("ringsrv-users"), ToolAPICall,
		commitCalls(projectPaths(3)...))
	require.NoError(t, err)
	after := batchStats(t)

	assert.Equal(t, before.count+1, after.count, "one observation per tools/call")
	assert.InDelta(t, before.sum+3, after.sum, 0.001, "carrying three items")
}

type batchStat struct {
	count uint64
	sum   float64
}

// batchStats reads the api_call row of the histogram: the only tool whose
// counts these tests assert.
func batchStats(t *testing.T) batchStat {
	t.Helper()
	var m dto.Metric
	obs, ok := batchItems().WithLabelValues(ToolAPICall).(prometheus.Metric)
	require.True(t, ok)
	require.NoError(t, obs.Write(&m))
	return batchStat{count: m.GetHistogram().GetSampleCount(), sum: m.GetHistogram().GetSampleSum()}
}

// A batch answer is JSON the model parses; nothing about the nesting may make
// it invalid.
func TestBatch_AnswerIsValidJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	res, err := batchService(t, srv.URL, embedlog.Logger{}).Call(ctxWithGroups("ringsrv-users"), ToolAPICall,
		commitCalls(projectPaths(2)...))
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	assert.True(t, json.Valid([]byte(res.Content[0].Text)))
}

// A refusal repeated in one list must not cost the first one its cheat sheet:
// the sheet is attached once per target, and the items must not be sharing the
// error it hangs on.
func TestBatch_RepeatedRefusalKeepsFirstSheet(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)
	bad := map[string]any{"target": "prom", "method": "GET", "path": "/api/v1/admin/tsdb"}

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, map[string]any{"calls": []any{bad, bad}})
	require.NoError(t, err)

	out := decodeBatch(t, res)
	require.Len(t, out.Results, 2)
	require.NotNil(t, out.Results[0].Error)
	assert.NotEmpty(t, out.Results[0].Error.Help, "the first refusal carries the sheet")
	require.NotNil(t, out.Results[1].Error)
	assert.Empty(t, out.Results[1].Error.Help, "the repeat does not repeat it")
}

// A refused db_query still writes one record per statement, and they have to
// look like items of a batch: batch_size = 0 reads as "a tool without a list"
// and drops them out of every batch query in the log.
func TestBatch_RefusedDBQueryRecordsCarryBatchSize(t *testing.T) {
	recs := captureLog(t, func(l embedlog.Logger) {
		f := newDBFixture(t, l)
		_, err := f.Call(ctxWithGroups("pg-readers"), ToolDBQuery, map[string]any{
			"target":  "ch", // granted to analysts, not to pg-readers
			"queries": []any{map[string]any{"sql": "SELECT 1"}, map[string]any{"sql": "SELECT 2"}},
		})
		require.NoError(t, err)
	})

	items := itemRecords(recs)
	require.Len(t, items, 2, "both statements are on record, refused or not")
	for i, rec := range items {
		assert.EqualValues(t, 2, rec["batch_size"], "record %d", i)
		assert.EqualValues(t, i, rec["batch_index"])
		assert.Equal(t, ErrCodeForbiddenRole, rec["deny_reason"])
		assert.NotEmpty(t, rec["query"], "the SQL of a refusal is only written here")
	}
}

// A panic in one item is that item's failure, not the process's: the request
// middleware recovers the goroutine it serves on, and the workers are not it.
func TestParallelMap_RecoversPanic(t *testing.T) {
	t.Parallel()
	assert.NotPanics(t, func() {
		out := parallelMap([]int{1, 2, 3}, 2, func(_, v int) string {
			if v == 2 {
				panic("upstream shaped something unexpected")
			}
			return "ok"
		})
		require.Len(t, out, 3)
		assert.Equal(t, "ok", out[0])
		assert.Empty(t, out[1], "the item that panicked is left empty")
		assert.Equal(t, "ok", out[2], "and the others still answer")
	})
}

// The per-item cap: a batch shapes each answer to its share of the budget, and
// a batch of one is shaped exactly as a single call was.
func TestBatch_ItemBytes(t *testing.T) {
	t.Parallel()
	s := ToolsService{maxBytes: 32768}
	one := APICallArgs{Calls: make([]APICall, 1), MaxBytes: 262144}
	assert.Equal(t, 262144, s.itemBytes(one), "a batch of one keeps the whole budget, as a single call had")

	four := APICallArgs{Calls: make([]APICall, 4), MaxBytes: 262144}
	assert.Equal(t, 262144/4, s.itemBytes(four), "four items share the budget instead of each shaping at all of it")

	twenty := APICallArgs{Calls: make([]APICall, 20), MaxBytes: 262144}
	assert.Equal(t, 32768, s.itemBytes(twenty),
		"the instance default is the floor — still 8x less work than shaping every item at 256 KB")

	small := APICallArgs{Calls: make([]APICall, 20), MaxBytes: 1000}
	assert.Equal(t, 32768, s.itemBytes(small), "a budget below the floor is enforced by the batch, not by the shaping")

	assert.Zero(t, s.itemBytes(APICallArgs{Calls: make([]APICall, 3)}),
		"without an argument the profile and the instance default decide, as before")
}

// The histogram is what "calls per trace" is read from, so its _count has to be
// the number of tools/call — the ones refused for the shape of the list
// included, since those are what a client gets wrong while learning it.
func TestBatch_MetricCountsRefusedCalls(t *testing.T) {
	s := testTools(t, false)
	ctx := ctxWithGroups("ringsrv-users")

	before := batchStats(t)
	_, err := s.Call(ctx, ToolAPICall, map[string]any{"calls": []any{}})
	require.NoError(t, err)
	_, err = s.Call(ctx, ToolAPICall, commitCalls(projectPaths(maxAPICallBatch+1)...))
	require.NoError(t, err)
	after := batchStats(t)

	assert.Equal(t, before.count+2, after.count, "an empty list and an oversized one are still tool calls")
	assert.InDelta(t, before.sum+float64(maxAPICallBatch+1), after.sum, 0.001)
}
