package rpc

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/mcp"
)

// writeService builds a catalogue with one read and one write profile, and a
// role that may write maxWrites times.
func writeService(t *testing.T, baseURL string, maxWrites int) ToolsService {
	t.Helper()
	return writeServiceLog(t, baseURL, maxWrites, embedlog.Logger{})
}

// writeServiceLog is writeService with a logger, for the tests that read the
// audit record back.
func writeServiceLog(t *testing.T, baseURL string, maxWrites int, l embedlog.Logger) ToolsService {
	t.Helper()
	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.youtrack]
Description  = "dev · YouTrack (readonly)"
BaseURL      = "` + baseURL + `"
Headers      = ["Authorization: ro-token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/issues$"]

[Profiles.youtrack-rw]
Description  = "dev · YouTrack (write)"
BaseURL      = "` + baseURL + `"
Headers      = ["Authorization: rw-token"]
AllowMethods = ["GET", "POST"]
AllowPaths   = ["^/api/issues$"]
Write        = true

[Roles.viewer]
Description = "read only"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["youtrack"]

[Roles.oncall]
Description = "may write"
Groups      = ["ringsrv-oncall"]
Tools       = ["api_call"]
Targets     = ["youtrack", "youtrack-rw"]
AllowWrite  = true
MaxWrites   = ` + strconv.Itoa(maxWrites) + `
`))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		JQTimeout: time.Second,
		Sessions:  ring.NewSessions(time.Hour, nil),
		Logger:    l,
	})
}

// calls is counted atomically because a batch reaches this handler from several
// goroutines at once: a plain int here is a data race, and the race detector
// finds it only on the runs where the batch tests happen to overlap.
func writeUpstream(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"PLF-1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A write without a stated purpose is refused, and refused before anything
// leaves the process: the upstream must not see a call that was never allowed.
func TestWrite_IntentIsRequired(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)
	s := writeService(t, srv.URL, 5)

	res, err := s.Call(ctxWithGroups("ringsrv-oncall"), ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack-rw", "method": "POST", "path": "/api/issues", "body": `{"summary":"x"}`,
	}))
	require.NoError(t, err)
	assert.Equal(t, ErrCodeIntentRequired, decodeErr(t, res).Code)
	assert.Zero(t, calls.Load(), "a refused write never reaches the upstream")

	// Reading the same target needs no intent: the barrier is about changing
	// things, not about talking.
	res, err = s.Call(ctxWithGroups("ringsrv-oncall"), ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack", "method": "GET", "path": "/api/issues",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	assert.EqualValues(t, 1, calls.Load())
}

func TestWrite_BudgetPerSession(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)
	s := writeService(t, srv.URL, 2)

	write := func(ctxUser string) mcp.ToolCallResult {
		res, err := s.Call(ctxWithUser(ctxUser, "ringsrv-oncall"), ToolAPICall, apiArgs(map[string]any{
			"target": "youtrack-rw", "method": "POST", "path": "/api/issues",
			"body": `{"summary":"x"}`, "intent": "создаю задачу по инциденту",
		}))
		require.NoError(t, err)
		return res
	}

	require.Nil(t, decodeBatch(t, write("alice")).Results[0].Error)
	require.Nil(t, decodeBatch(t, write("alice")).Results[0].Error)

	e := decodeErr(t, write("alice"))
	assert.Equal(t, ErrCodeWriteBudget, e.Code)
	assert.Contains(t, e.Message, "2", "the message says what the limit was")
	assert.EqualValues(t, 2, calls.Load(), "the third write never left the process")

	// One caller cannot spend another's budget: the limit is per session, and
	// a shared counter would make one busy engineer block the on-call rotation.
	assert.Nil(t, decodeBatch(t, write("bob")).Results[0].Error)
	assert.EqualValues(t, 3, calls.Load())

	// Reading still works after the write budget is gone.
	res, err := s.Call(ctxWithUser("alice", "ringsrv-oncall"), ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack", "method": "GET", "path": "/api/issues",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

// A role without AllowWrite does not see the write profile at all, so the
// barrier is never even reached — the catalogue refused it first.
func TestWrite_RoleWithoutAllowWrite(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)
	s := writeService(t, srv.URL, 2)

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack-rw", "method": "POST", "path": "/api/issues",
		"intent": "пробую записать", "body": `{}`,
	}))
	require.NoError(t, err)
	assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)
	assert.Zero(t, calls.Load())
}

// MaxWrites = 0 means "no limit", not "no writes" — the two are easy to
// confuse and the difference is a whole feature.
func TestWrite_ZeroBudgetMeansUnlimited(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)
	s := writeService(t, srv.URL, 0)

	for range 3 {
		res, err := s.Call(ctxWithGroups("ringsrv-oncall"), ToolAPICall, apiArgs(map[string]any{
			"target": "youtrack-rw", "method": "POST", "path": "/api/issues",
			"body": `{}`, "intent": "создаю задачу",
		}))
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
	}
	assert.EqualValues(t, 3, calls.Load())
}

func TestCall_ResultCarriesTrace(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)
	s := writeService(t, srv.URL, 2)

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack", "method": "GET", "path": "/api/issues",
	}))
	require.NoError(t, err)

	assert.Contains(t, decodeBatch(t, res).TraceID, ring.TracePrefix)
}

// A write the allowlist refused never happened, so it must not spend the
// session's budget: with MaxWrites = 1 a typo in the path used to cost the
// on-call engineer their only write.
func TestWrite_DeniedPathSpendsNoBudget(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)
	s := writeService(t, srv.URL, 1)
	ctx := ctxWithUser("alice", "ringsrv-oncall")

	res, err := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack-rw", "method": "POST", "path": "/api/nope",
		"body": `{}`, "intent": "создаю задачу",
	}))
	require.NoError(t, err)
	assert.Equal(t, ErrCodePathNotAllowed, decodeErr(t, res).Code)

	// The allowlist answers before the barrier does: a bad path without an
	// intent is a bad path, not a missing intent.
	res, err = s.Call(ctx, ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack-rw", "method": "POST", "path": "/api/nope", "body": `{}`,
	}))
	require.NoError(t, err)
	assert.Equal(t, ErrCodePathNotAllowed, decodeErr(t, res).Code)

	// The one write the role has is still there.
	res, err = s.Call(ctx, ToolAPICall, apiArgs(map[string]any{
		"target": "youtrack-rw", "method": "POST", "path": "/api/issues",
		"body": `{}`, "intent": "создаю задачу",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	assert.EqualValues(t, 1, calls.Load())
}

// GET on a write profile reads the issue the model is about to comment on and
// changes nothing: no intent, no budget, and the audit record says it was not
// a write.
func TestWrite_GETOnWriteProfileIsARead(t *testing.T) {
	var calls atomic.Int64
	srv := writeUpstream(t, &calls)

	recs := captureLog(t, func(l embedlog.Logger) {
		s := writeServiceLog(t, srv.URL, 1, l)
		ctx := ctxWithUser("alice", "ringsrv-oncall")

		for range 3 {
			res, err := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{
				"target": "youtrack-rw", "method": "GET", "path": "/api/issues",
			}))
			require.NoError(t, err)
			require.False(t, res.IsError, res.Content[0].Text)
		}
		// The budget of one write is untouched by three reads.
		res, err := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{
			"target": "youtrack-rw", "method": "POST", "path": "/api/issues",
			"body": `{}`, "intent": "создаю задачу",
		}))
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
	})
	assert.EqualValues(t, 4, calls.Load())

	records := itemRecords(recs)
	require.Len(t, records, 4)
	for _, rec := range records[:3] {
		assert.Equal(t, false, rec["write"], "a GET is not a write, whatever profile it went through")
	}
	assert.Equal(t, true, records[3]["write"])
}
