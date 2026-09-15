package rpc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// captureLog runs fn with a JSON logger writing into a pipe and returns the
// records it produced. embedlog captures os.Stdout when the logger is built,
// so the swap happens around construction only.
func captureLog(t *testing.T, fn func(l embedlog.Logger)) []map[string]any {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	orig := os.Stdout
	os.Stdout = w
	l := embedlog.NewLogger(true, true)
	os.Stdout = orig

	fn(l)
	require.NoError(t, w.Close())

	raw, err := io.ReadAll(r)
	require.NoError(t, err)

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

func auditRecords(recs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r[audit.EventKey] == audit.EventValue {
			out = append(out, r)
		}
	}
	return out
}

// itemRecords keeps the records of the calls a batch actually made, dropping the
// record of the tool call that carried them: the target, the path and the
// decision are properties of an item.
func itemRecords(recs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range auditRecords(recs) {
		if idx, ok := r["batch_index"].(float64); ok && int(idx) == batchCallIndex {
			continue
		}
		out = append(out, r)
	}
	return out
}

// callRecords keeps the other half: one record per tools/call, whatever the
// list inside it held. This is the number "calls per trace" is counted from.
func callRecords(recs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range auditRecords(recs) {
		if idx, ok := r["batch_index"].(float64); ok && int(idx) == batchCallIndex {
			out = append(out, r)
		}
	}
	return out
}

// auditService builds the one-target service every audit test calls. redact is
// the profile's mode, empty for the default; call-set headers are allowed
// because the tests that exercise their refusals need the same record.
func auditService(t *testing.T, baseURL, redact string, l embedlog.Logger) ToolsService {
	t.Helper()
	toml := `
Env = "dev"

[Profiles.test]
Description  = "dev · test upstream"
BaseURL      = "` + baseURL + `"
Headers      = ["Authorization: server-token"]
AllowHeaders = ["Authorization2", "Platform"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["test"]
`
	if redact != "" {
		toml = strings.Replace(toml, "AllowMethods", "Redact       = "+strconv.Quote(redact)+"\nAllowMethods", 1)
	}
	cat, err := target.Parse([]byte(toml))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		JQTimeout: time.Second,
		Sessions:  ring.NewSessions(time.Hour, nil),
		Logger:    l,
	})
}

// Identity does not survive the hop to an upstream — every call carries the
// service's own credential — so this record is the only place where a person
// and a call are connected.
func TestAudit_SuccessfulCall(t *testing.T) {
	const secretPayload = "user@example.com and other personal data"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":"` + secretPayload + `"}`))
	}))
	defer srv.Close()

	var res mcp.ToolCallResult
	recs := captureLog(t, func(l embedlog.Logger) {
		var err error
		res, err = auditService(t, srv.URL, "", l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query",
			"intent": "проверяю всплеск ошибок",
		}))
		require.NoError(t, err)
	})

	records := itemRecords(recs)
	require.Len(t, records, 1, "one call, one record")
	rec := records[0]

	assert.Equal(t, audit.DecisionAllow, rec["decision"])
	assert.Equal(t, "test", rec["source"], "the target is where the answer came from")
	assert.Equal(t, "GET", rec["method"])
	assert.Equal(t, "/api/v1/query", rec["query"])
	assert.Equal(t, "tester", rec["sub"], "who did it")
	assert.Equal(t, "ringsrv-users", rec["groups"])
	assert.Equal(t, "viewer", rec["roles"])
	assert.Equal(t, "проверяю всплеск ошибок", rec["intent"], "and why they say they did it")
	assert.EqualValues(t, http.StatusOK, rec["upstream_status"])
	assert.Positive(t, rec["bytes_out"])
	// Whether the call narrowed the answer and whether it got all of it are the
	// two numbers that say if the model is using the tool well.
	assert.Contains(t, rec, "jq")
	assert.Contains(t, rec, "truncated")

	// The log must not become an unmanaged copy of everything that passed
	// through — response bodies are exactly that.
	line, err := json.Marshal(rec)
	require.NoError(t, err)
	assert.NotContains(t, string(line), secretPayload)
	assert.NotContains(t, string(line), "server-token", "nor the credential the call used")

	// The trace stitches the report a skill writes to the calls behind it.
	var out APICallBatch
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	assert.Equal(t, rec["trace_id"], out.TraceID)
	assert.NotEmpty(t, out.TraceID)
}

// Denials are recorded too: without them the ratio of allowlist refusals to
// real calls — the thing that says a skill has drifted from the API — cannot
// be recovered from the log.
func TestAudit_Denials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"unknown target": {map[string]any{"target": "nope", "method": "GET", "path": "/api/v1/query"}, ErrCodeTargetUnknown},
		"method":         {map[string]any{"target": "test", "method": "PUT", "path": "/api/v1/query"}, ErrCodeMethodNotAllowed},
		"path":           {map[string]any{"target": "test", "method": "GET", "path": "/api/v1/admin"}, ErrCodePathNotAllowed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			recs := captureLog(t, func(l embedlog.Logger) {
				_, err := auditService(t, srv.URL, "", l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(tc.args))
				require.NoError(t, err)
			})
			records := itemRecords(recs)
			require.Len(t, records, 1)
			assert.Equal(t, audit.DecisionDeny, records[0]["decision"])
			assert.Equal(t, tc.want, records[0]["deny_reason"])
		})
	}
}

// A crafted intent must not be able to forge a line in the log it is written
// into (log forging).
func TestAudit_IntentIsSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	recs := captureLog(t, func(l embedlog.Logger) {
		_, err := auditService(t, srv.URL, "", l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query",
			"intent": "ok\n\x1b[31mFAKE decision=allow sub=admin\x1b[0m " + strings.Repeat("x", 300),
		}))
		require.NoError(t, err)
	})

	records := itemRecords(recs)
	require.Len(t, records, 1)
	intent, ok := records[0]["intent"].(string)
	require.True(t, ok)

	assert.NotContains(t, intent, "\n")
	assert.NotContains(t, intent, "\x1b")
	assert.LessOrEqual(t, len([]rune(intent)), audit.MaxIntentLen)
}

// The path is the model's too, and with a query in its query string it can
// be as long as a query. It goes into the record under the intent's rule,
// only longer.
func TestAudit_PathIsSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	path := "/api/v1/query?query=" + strings.Repeat("a", 2500) + "\n" + strings.Repeat("b", 2500)
	recs := captureLog(t, func(l embedlog.Logger) {
		_, err := auditService(t, srv.URL, "", l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": path,
		}))
		require.NoError(t, err)
	})

	records := itemRecords(recs)
	require.Len(t, records, 1, "however the call ended, it is one record")
	got, ok := records[0]["query"].(string)
	require.True(t, ok)

	assert.NotContains(t, got, "\n", "one record, one line")
	assert.Len(t, []rune(got), audit.MaxTextLen)
	assert.True(t, strings.HasPrefix(got, "/api/v1/query?query=aaa"), "the head is kept, the tail is cut")
}

// A profile with redaction masks what the model wrote into the path as well as
// what the upstream answered — and in the log it does so even in warn mode:
// warn is a decision about the model's answer, and the log is read by everyone
// with Loki.
func TestAudit_PathIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	recs := captureLog(t, func(l embedlog.Logger) {
		_, err := auditService(t, srv.URL, "warn", l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query?query=user_email%20%3D%20'a@b.co'",
		}))
		require.NoError(t, err)
	})

	records := itemRecords(recs)
	require.Len(t, records, 1)
	got, ok := records[0]["query"].(string)
	require.True(t, ok)
	assert.Contains(t, got, "a***@b.co", "the local part goes, the domain stays: it groups rows by employer and hiding it protects nothing")
	assert.NotContains(t, got, "a@b.co")
}

// Which rules fired on the answer, by name, in the record of the call they fired
// on: the counter says how often across the service and cannot say which call.
//
// warn on purpose — the answer went to the model untouched and the record still
// says a rule matched. That is the mode's point: count first, mask next release.
func TestAudit_RecordNamesTheRulesThatFired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owner":"ivan@example.com"}`))
	}))
	defer srv.Close()

	recs := captureLog(t, func(l embedlog.Logger) {
		_, err := auditService(t, srv.URL, "warn", l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query",
		}))
		require.NoError(t, err)
	})

	records := itemRecords(recs)
	require.Len(t, records, 1)
	assert.Equal(t, redact.RuleEmail, records[0]["masked"], "the rule is named, what it matched is not")
}

// SQL reaches the record whole, masked by the target's rules whatever mode the
// answer gets, with the hash of the full text alongside. It travels as the
// record's query, the field for model-authored request text — an HTTP path
// travels there for the same reason.
func TestAudit_SQLRedacted(t *testing.T) {
	const sql = "SELECT id FROM users WHERE email = 'ivan@example.com'\nORDER BY id"
	recs := captureLog(t, func(l embedlog.Logger) {
		rec := auditRecord{Target: "pg", SQL: sql, RowsReturned: 3, Redact: redact.ModeWarn}
		rec.Tool, rec.Env, rec.Decision = "db_query", "dev", audit.DecisionAllow
		newAuditWriter(l).Write(t.Context(), rec.core())
	})

	records := itemRecords(recs)
	require.Len(t, records, 1)
	rec := records[0]

	got, ok := rec["query"].(string)
	require.True(t, ok)
	assert.Equal(t, "SELECT id FROM users WHERE email = 'i***@example.com' ORDER BY id", got)
	assert.Equal(t, audit.Hash(sql), rec["query_sha256"], "hashed before masking and before the cut")
	assert.EqualValues(t, 3, rec["rows"])
}

// repo_map reads who may ask what about which service. Who asked belongs in
// the log for the same reason every api_call does.
func TestAudit_RepoMap(t *testing.T) {
	recs := captureLog(t, func(l embedlog.Logger) {
		s := NewToolsService(ToolsDeps{
			Targets:  testCatalog(t),
			Sessions: ring.NewSessions(time.Hour, nil),
			Logger:   l,
		})
		res, err := s.Call(ctxWithGroups("ringsrv-developers"), ToolRepoMap, map[string]any{"repos": []any{"apisrv"}})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
	})

	// repo_map answers from the catalogue in memory, so the call is the whole
	// of the work: one record for it, none per item.
	records := callRecords(recs)
	require.Len(t, records, 1)
	assert.Empty(t, itemRecords(recs))
	assert.Equal(t, ToolRepoMap, records[0]["tool"], "and under its own name, not api_call's")
	assert.Equal(t, audit.DecisionAllow, records[0]["decision"])
	assert.Equal(t, "developer", records[0]["roles"])
	assert.NotEmpty(t, records[0]["trace_id"])
	assert.Positive(t, records[0]["bytes_out"])
}

// The code tools share the record with api_call and used to fill only half of
// it: bytes_out stayed 0 and truncated false whatever the answer, so a search
// that found nothing and one cut at max_matches looked the same in the log.
func TestAudit_CodeTools(t *testing.T) {
	var (
		s   ToolsService
		sha string
	)
	recs := captureLog(t, func(l embedlog.Logger) {
		s, sha = codeFixtureLog(t, l)
		ctx := ctxWithGroups("ringsrv-developers")
		call := func(tool string, args map[string]any) {
			args["ref"] = sha
			res, err := s.Call(ctx, tool, args)
			require.NoError(t, err)
			require.False(t, res.IsError, res.Content[0].Text)
		}
		call(ToolCodeSearch, map[string]any{"query": "func", "max_matches": 1})
		call(ToolCodeSearch, map[string]any{"query": "ThisStringDoesNotExistAnywhere"})
		call(ToolCodeRead, map[string]any{"repo": "apisrv", "path": "internal/rpc/order.go", "max_lines": 2})
		call(ToolCodeRead, map[string]any{"repo": "apisrv", "path": "internal/rpc/order.go"})
	})

	records := itemRecords(recs)
	require.Len(t, records, 4, "one call, one record")
	capped, empty, cut, whole := records[0], records[1], records[2], records[3]

	assert.Equal(t, sha, capped["method"], "which commit was searched")
	assert.Equal(t, true, capped["truncated"])

	assert.Equal(t, false, empty["truncated"])
	assert.Less(t, empty["bytes_out"], capped["bytes_out"], "nothing found is smaller than one hit")

	assert.Equal(t, true, cut["truncated"])
	assert.Equal(t, false, whole["truncated"])
	assert.Less(t, cut["bytes_out"], whole["bytes_out"])
}
