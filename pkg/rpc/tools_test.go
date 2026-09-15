package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/auth"
	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/mcp"
)

func testCatalog(t *testing.T) *target.Catalog {
	t.Helper()
	c, err := target.Load(filepath.Join("testdata", "targets.toml"))
	require.NoError(t, err)
	return c
}

// testDocs is the markdown library that ships with the binary: the sheets
// help serves and the refusals carry.
func testDocs(t *testing.T) *doc.Library {
	t.Helper()
	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)
	return lib
}

func testTools(t *testing.T, isDevel bool) ToolsService {
	t.Helper()
	return NewToolsService(ToolsDeps{
		Targets: testCatalog(t),
		Docs:    testDocs(t),
		IsDevel: isDevel,
		Logger:  embedlog.Logger{},
	})
}

// The shipped contour has carried no write profile since 2026-09-14. What a
// write right does to the surface — the annotations, the intent argument, the
// sheet a write profile borrows — is the code's rule and not the contour's, so
// the tests about it add a writing role to the shipped catalogue instead of
// waiting for prod to grow one back.
const writeRoleTOML = `
[Profiles.youtrack-rw]
Description  = "prod · YouTrack: создание задач и комментариев"
BaseURL      = "https://bugs.example.com"
SkipDefaultHeaders = true
Headers      = ["Authorization: Bearer token"]
AllowMethods = ["GET", "POST"]
AllowPaths   = [
  "^/api/issues$",
  "^/api/issues/[A-Z]+-\\d+/comments$",
]
Write        = true

[Roles.writer]
Description = "Запись в YouTrack"
Groups      = ["ringsrv-writers"]
Tools       = ["api_call"]
Targets     = ["youtrack", "youtrack-rw"]
AllowWrite  = true
MaxWrites   = 10
`

func testToolsWithWrite(t *testing.T) ToolsService {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "targets.toml"))
	require.NoError(t, err)
	c, err := target.Parse(append(raw, writeRoleTOML...))
	require.NoError(t, err)
	return NewToolsService(ToolsDeps{Targets: c, Docs: testDocs(t), Logger: embedlog.Logger{}})
}

func ctxWithGroups(groups ...string) context.Context {
	return ctxWithUser("tester", groups...)
}

// ctxWithUser names the caller: the session key, and with it the trace and the
// write budget, hang off UserID.
func ctxWithUser(userID string, groups ...string) context.Context {
	return auth.NewContext(context.Background(), auth.Principal{UserID: userID, Groups: groups})
}

// The catalogue in the tool description is what keeps the model from guessing
// target names, and it is built per role.
func TestList_CatalogIsPerRole(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)

	viewer, err := s.List(ctxWithGroups("ringsrv-users"), "")
	require.NoError(t, err)
	// The viewer role grants repo_map but no repositories, so the tool that
	// would answer about nothing is not offered. help comes with any tool.
	require.Len(t, viewer.Tools, 2)
	assert.Equal(t, ToolHelp, viewer.Tools[1].Name)

	tool := viewer.Tools[0]
	assert.Equal(t, ToolAPICall, tool.Name)
	assert.Contains(t, tool.Description, "PROD", "the contour comes first (D25)")
	assert.Contains(t, tool.Description, "prom —")
	assert.Contains(t, tool.Description, "help(<name>)", "details are pulled on demand, by the tool that reaches every client")
	assert.NotContains(t, tool.Description, "gitlab —", "a viewer must not read about targets they cannot call")

	// The schema is what the client validates arguments against: one call's
	// fields sit under calls[], and the list itself is required — a model
	// offered both shapes picks the simple one and the batch never runs.
	var schema map[string]any
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "calls")
	assert.Contains(t, props, "max_bytes")
	assert.NotContains(t, props, "target", "the single-call form is gone from the schema")
	assert.ElementsMatch(t, []any{"calls"}, schema["required"])

	call, ok := props["calls"].(map[string]any)
	require.True(t, ok)
	item, ok := call["items"].(map[string]any)
	require.True(t, ok)
	itemProps, ok := item["properties"].(map[string]any)
	require.True(t, ok)
	for _, f := range []string{"target", "method", "path", "jq", "intent"} {
		assert.Contains(t, itemProps, f)
	}
	assert.ElementsMatch(t, []any{"target", "method", "path"}, item["required"])
}

// Annotations are the only thing standing between a prompt-injected write and
// an auto-approved one, so they have to follow the caller's actual rights.
func TestList_AnnotationsFollowWriteRights(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)

	viewer, err := s.List(ctxWithGroups("ringsrv-users"), "")
	require.NoError(t, err)
	require.NotNil(t, viewer.Tools[0].Annotations)
	assert.True(t, *viewer.Tools[0].Annotations.ReadOnlyHint)
	assert.False(t, *viewer.Tools[0].Annotations.DestructiveHint)

	writer, err := testToolsWithWrite(t).List(ctxWithGroups("ringsrv-writers"), "")
	require.NoError(t, err)
	require.NotNil(t, writer.Tools[0].Annotations)
	assert.False(t, *writer.Tools[0].Annotations.ReadOnlyHint)
	assert.True(t, *writer.Tools[0].Annotations.DestructiveHint)
	assert.Contains(t, writer.Tools[0].Description, "intent")
}

// The whole path from credential to catalogue for the api-key backend: the key
// carries groups, the store puts them on the principal, the catalogue resolves
// them into a role. It walks the real store rather than building a principal by
// hand, because a key without groups authenticates fine and then 403s.
func TestList_APIKeyCarriesGroups(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)

	sum := sha256.Sum256([]byte("secret-token"))
	store := auth.NewStore([]auth.Key{
		{UserID: "alice", KeyHash: hex.EncodeToString(sum[:]), Groups: []string{"ringsrv-users"}},
		{UserID: "nogroups", KeyHash: func() string { h := sha256.Sum256([]byte("other-token")); return hex.EncodeToString(h[:]) }()},
	})

	p, err := store.Authenticate("secret-token", time.Now())
	require.NoError(t, err)
	got, err := s.List(auth.NewContext(context.Background(), p), "")
	require.NoError(t, err)
	require.Len(t, got.Tools, 2)
	assert.Contains(t, got.Tools[0].Description, "prom —")

	p, err = store.Authenticate("other-token", time.Now())
	require.NoError(t, err)
	_, err = s.List(auth.NewContext(context.Background(), p), "")
	assert.ErrorIs(t, err, ErrNoRole, "a key without groups matches no role — the config is wrong, and it must say so")
}

// A user whose groups match nothing gets a readable 403, not an empty
// catalogue that looks like a broken server.
func TestList_NoRoleIsForbidden(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)

	_, err := s.List(ctxWithGroups("some-other-team"), "")
	require.ErrorIs(t, err, ErrNoRole)

	_, err = s.List(context.Background(), "")
	require.ErrorIs(t, err, ErrNoRole, "no principal on a prod instance is not anonymous access")
}

// A dev instance runs without auth at all; it must still be usable, and still
// never hand out the write bit.
func TestList_DevelWithoutPrincipal(t *testing.T) {
	t.Parallel()
	s := testTools(t, true)

	got, err := s.List(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, got.Tools, 3, "api_call, repo_map and help: the dev catalogue has repositories")
	assert.Equal(t, ToolAPICall, got.Tools[0].Name)
	assert.True(t, *got.Tools[0].Annotations.ReadOnlyHint)
	assert.NotContains(t, got.Tools[0].Description, "youtrack-rw")
}

func TestCall_Errors(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)
	ctx := ctxWithGroups("ringsrv-users")

	decode := func(t *testing.T, res mcp.ToolCallResult) ToolError {
		t.Helper()
		require.Len(t, res.Content, 1)
		e := decodeErr(t, res)
		assert.Equal(t, "prod", e.Env, "every answer says which contour it is about")
		return e
	}

	t.Run("unknown tool", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(ctx, "run_sql", nil)
		require.NoError(t, err, "tool errors travel in the result, not as JSON-RPC errors")
		assert.Equal(t, ErrCodeBadArgs, decode(t, res).Code)
	})

	t.Run("unknown target lists the ones that exist", func(t *testing.T) {
		t.Parallel()
		res, _ := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "graphana", "method": "GET", "path": "/api/search"}))
		e := decode(t, res)
		assert.Equal(t, ErrCodeTargetUnknown, e.Code)
		assert.Contains(t, e.Targets, "grafana", "the error is the documentation")
	})

	t.Run("target outside the role", func(t *testing.T) {
		t.Parallel()
		res, _ := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "gitlab", "method": "GET", "path": "/api/v4/projects/1"}))
		e := decode(t, res)
		assert.Equal(t, ErrCodeForbiddenRole, e.Code)
		assert.NotContains(t, e.Targets, "gitlab")
	})

	t.Run("missing arguments", func(t *testing.T) {
		t.Parallel()
		res, _ := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "prom"}))
		assert.Equal(t, ErrCodeBadArgs, decode(t, res).Code)
	})

	t.Run("method outside the allowlist", func(t *testing.T) {
		t.Parallel()
		res, _ := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "prom", "method": "PUT", "path": "/api/v1/query"}))
		e := decode(t, res)
		assert.Equal(t, ErrCodeMethodNotAllowed, e.Code)
		assert.Equal(t, []string{"GET"}, e.Methods, "the error is the documentation")
	})

	t.Run("path outside the allowlist", func(t *testing.T) {
		t.Parallel()
		res, _ := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "prom", "method": "GET", "path": "/api/v1/admin/tsdb"}))
		e := decode(t, res)
		assert.Equal(t, ErrCodePathNotAllowed, e.Code)
		assert.NotEmpty(t, e.Paths)
	})
}

// callAgainst wires a ToolsService to a catalogue whose only target is the
// given test server, so a call really goes over HTTP without touching a real
// host.
func callAgainst(t *testing.T, baseURL string, call map[string]any) (mcp.ToolCallResult, error) {
	t.Helper()
	return serviceFor(t, baseURL, "", 0).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(call))
}

// serviceFor builds a ToolsService whose only target is the given test server.
// defaultJQ and maxBytes shape the profile, so the tests exercise the same
// precedence the real catalogue does.
func serviceFor(t *testing.T, baseURL, defaultJQ string, maxBytes int) ToolsService {
	t.Helper()
	toml := `
Env = "dev"

[Profiles.test]
Description  = "dev · test upstream"
BaseURL      = "` + baseURL + `"
Headers      = ["Authorization: server-token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$", "^/api/v1/fail$", "^/api/v1/text$", "^/api/v1/big$"]
`
	if defaultJQ != "" {
		toml += "DefaultJQ    = " + strconv.Quote(defaultJQ) + "\n"
	}
	if maxBytes > 0 {
		toml += "MaxBytes     = " + strconv.Itoa(maxBytes) + "\n"
	}
	toml += `
[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["test"]
`
	cat, err := target.Parse([]byte(toml))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		JQTimeout: time.Second,
		Logger:    embedlog.Logger{},
	})
}

const promEnvelope = `{"status":"success","data":{"result":[{"metric":{"job":"apisrv"},"value":[1,"3"]}]}}`

// apiArgs turns one call into the list api_call takes. Most tests are about
// what happens to a single call, and a batch of one is exactly that — with the
// budget argument lifted to where it now belongs, on the batch.
func apiArgs(call map[string]any) map[string]any {
	one := maps.Clone(call)
	args := map[string]any{}
	if mb, ok := one["max_bytes"]; ok {
		args["max_bytes"] = mb
		delete(one, "max_bytes")
	}
	args["calls"] = []any{one}
	return args
}

// decodeBatch reads a batch answer.
func decodeBatch(t *testing.T, res mcp.ToolCallResult) APICallBatch {
	t.Helper()
	require.False(t, res.IsError, res.Content[0].Text)
	var out APICallBatch
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	return out
}

// decodeOK reads the single answer of a one-call batch.
func decodeOK(t *testing.T, res mcp.ToolCallResult) APICallItem {
	t.Helper()
	out := decodeBatch(t, res)
	require.Len(t, out.Results, 1)
	require.Nil(t, out.Results[0].Error, "expected an answer, got a refusal")
	return out.Results[0]
}

// decodeErr reads the refusal of a one-item batch, whether the whole call was
// refused before anything ran or only its one item was: every batch answer
// carries refusals the same way. The contour is filled in from the batch, since
// an item's error does not repeat it.
func decodeErr(t *testing.T, res mcp.ToolCallResult) ToolError {
	t.Helper()
	if res.IsError {
		var e ToolError
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &e))
		return e
	}
	var out struct {
		Env     string `json:"env"`
		Results []struct {
			Error *ToolError `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out), res.Content[0].Text)
	require.Len(t, out.Results, 1)
	require.NotNil(t, out.Results[0].Error, "expected a refusal, got an answer: "+res.Content[0].Text)
	e := *out.Results[0].Error
	e.Env = out.Env
	return e
}

func TestCall_JQ(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(promEnvelope))
	}))
	t.Cleanup(srv.Close)

	ctx := ctxWithGroups("ringsrv-users")
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"target": "test", "method": "GET", "path": "/api/v1/query"}
		maps.Copy(a, extra)
		return apiArgs(a)
	}

	t.Run("DefaultJQ applies without an argument", func(t *testing.T) {
		t.Parallel()
		res, err := serviceFor(t, srv.URL, ".data.result", 0).Call(ctx, ToolAPICall, args(nil))
		require.NoError(t, err)
		out := decodeOK(t, res)
		arr, ok := out.Data.([]any)
		require.True(t, ok, "the envelope is gone, the series are not")
		assert.Len(t, arr, 1)
		assert.True(t, out.DefaultJQ, "the answer says a filter it did not ask for shaped it")
	})

	t.Run("argument overrides DefaultJQ", func(t *testing.T) {
		t.Parallel()
		res, err := serviceFor(t, srv.URL, ".data.result", 0).Call(ctx, ToolAPICall, args(map[string]any{"jq": ".status"}))
		require.NoError(t, err)
		out := decodeOK(t, res)
		assert.Equal(t, "success", out.Data)
		assert.False(t, out.DefaultJQ, "its own filter ran, nothing to flag")
	})

	// With one tool the error is the documentation: the keys are what turn a
	// wrong filter into a right one on the second try.
	t.Run("broken filter answers with the keys of the body", func(t *testing.T) {
		t.Parallel()
		res, err := serviceFor(t, srv.URL, "", 0).Call(ctx, ToolAPICall, args(map[string]any{"jq": ".data["}))
		require.NoError(t, err)
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeJQFailed, e.Code)
		assert.Equal(t, []string{"data", "status"}, e.Keys)
	})

	t.Run("filter that selects nothing is a valid empty answer", func(t *testing.T) {
		t.Parallel()
		res, err := serviceFor(t, srv.URL, "", 0).Call(ctx, ToolAPICall, args(map[string]any{"jq": `.data.result[] | select(.metric.job == "nope")`}))
		require.NoError(t, err)
		assert.Nil(t, decodeOK(t, res).Data)
	})
}

func TestCall_Truncation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"` + strings.Repeat("я", 500) + `"}`))
	}))
	t.Cleanup(srv.Close)

	ctx := ctxWithGroups("ringsrv-users")
	call := apiArgs(map[string]any{"target": "test", "method": "GET", "path": "/api/v1/query"})

	t.Run("profile limit cuts the answer and says so", func(t *testing.T) {
		t.Parallel()
		res, err := serviceFor(t, srv.URL, "", 200).Call(ctx, ToolAPICall, call)
		require.NoError(t, err)
		out := decodeOK(t, res)

		assert.True(t, out.Truncated)
		assert.Greater(t, out.BytesTotal, 200, "bytes_total is the full size, not the cut one")
		text, ok := out.Data.(string)
		require.True(t, ok, "a cut answer travels as a marked string, not as broken JSON")
		assert.True(t, strings.HasSuffix(text, mcp.TruncateMarker))
		assert.True(t, utf8.ValidString(text), "cut on a rune boundary")
	})

	t.Run("argument overrides the profile limit", func(t *testing.T) {
		t.Parallel()
		res, err := serviceFor(t, srv.URL, "", 200).Call(ctx, ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query", "max_bytes": 100_000,
		}))
		require.NoError(t, err)
		out := decodeOK(t, res)
		assert.False(t, out.Truncated)
		assert.IsType(t, map[string]any{}, out.Data)
	})
}

func TestCall_ReachesUpstream(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/fail":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream is unhappy"}`))
		case "/api/v1/text":
			_, _ = w.Write([]byte("plain text answer"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[1,2,3]}}`))
		}
	}))
	t.Cleanup(srv.Close)

	t.Run("success carries status, env and data", func(t *testing.T) {
		t.Parallel()
		res, err := callAgainst(t, srv.URL, map[string]any{"target": "test", "method": "GET", "path": "/api/v1/query?query=up"})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)

		batch := decodeBatch(t, res)
		require.Len(t, batch.Results, 1)
		out := batch.Results[0]
		assert.Equal(t, http.StatusOK, out.Status)
		assert.Equal(t, "dev", batch.Env)
		assert.Equal(t, 0, out.Index, "an answer says which call it belongs to")
		assert.False(t, out.Truncated)
		assert.Positive(t, out.BytesTotal)

		data, ok := out.Data.(map[string]any)
		require.True(t, ok, "a JSON body arrives as structured data, not as a string")
		assert.Equal(t, "success", data["status"])
	})

	// A backend answering in its own format is still an answer: failing the
	// call would hide the very message that explains what happened.
	t.Run("non-JSON body arrives as a string", func(t *testing.T) {
		t.Parallel()
		res, err := callAgainst(t, srv.URL, map[string]any{"target": "test", "method": "GET", "path": "/api/v1/text"})
		require.NoError(t, err)
		require.False(t, res.IsError)

		assert.Equal(t, "plain text answer", decodeOK(t, res).Data)
	})

	t.Run("upstream error keeps its status and message", func(t *testing.T) {
		t.Parallel()
		res, err := callAgainst(t, srv.URL, map[string]any{"target": "test", "method": "GET", "path": "/api/v1/fail"})
		require.NoError(t, err)

		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeUpstream, e.Code)
		assert.Equal(t, http.StatusBadGateway, e.Status)
		assert.Contains(t, e.Message, "upstream is unhappy")
	})
}

func TestCall_UpstreamTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.test]
Description  = "dev · slow upstream"
BaseURL      = "` + srv.URL + `"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["test"]
`))
	require.NoError(t, err)

	s := NewToolsService(ToolsDeps{
		Targets:  cat,
		Upstream: upstream.New(upstream.Options{Timeout: 50 * time.Millisecond}),
		Logger:   embedlog.Logger{},
	})
	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{"target": "test", "method": "GET", "path": "/api/v1/query"}))
	require.NoError(t, err)

	assert.Equal(t, ErrCodeTimeout, decodeErr(t, res).Code, "a slow backend is not the same failure as a rejecting one")
}

// Sentry answers carry whatever the user typed, and all of it would otherwise
// land in the model's context.
func TestCall_Redaction(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"title":"TypeError in Create","user":{"email":"buyer@example.com"}}`))
	}))
	t.Cleanup(srv.Close)

	service := func(mode string) ToolsService {
		cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.sentry]
Description  = "dev · Sentry"
BaseURL      = "` + srv.URL + `"
Headers      = ["Authorization: ro-token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/0/issues/$"]
Redact       = "` + mode + `"
RedactRules  = ["email", "ip"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["sentry"]
`))
		require.NoError(t, err)
		return NewToolsService(ToolsDeps{
			Targets:   cat,
			Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
			JQTimeout: time.Second,
			Sessions:  ring.NewSessions(time.Hour, nil),
			Logger:    embedlog.Logger{},
		})
	}
	args := apiArgs(map[string]any{"target": "sentry", "method": "GET", "path": "/api/0/issues/"})

	t.Run("redact replaces and says what it replaced", func(t *testing.T) {
		t.Parallel()
		res, err := service("redact").Call(ctxWithGroups("ringsrv-users"), ToolAPICall, args)
		require.NoError(t, err)
		out := decodeOK(t, res)

		body, err := json.Marshal(out.Data)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "buyer@example.com")
		assert.Contains(t, string(body), "TypeError in Create", "the diagnosis survives")
		assert.Equal(t, []string{"email"}, out.Redacted, "an absent field must not read as absent data")
	})

	// warn is the mode that decides the rule set: it counts without changing
	// anything, so the cost of a wrong rule is a number, not a lost answer.
	t.Run("warn keeps the body and still reports", func(t *testing.T) {
		t.Parallel()
		res, err := service("warn").Call(ctxWithGroups("ringsrv-users"), ToolAPICall, args)
		require.NoError(t, err)
		out := decodeOK(t, res)

		body, err := json.Marshal(out.Data)
		require.NoError(t, err)
		assert.Contains(t, string(body), "buyer@example.com")
		assert.Equal(t, []string{"email"}, out.Redacted)
	})

	t.Run("off is the default and does nothing", func(t *testing.T) {
		t.Parallel()
		res, err := service("off").Call(ctxWithGroups("ringsrv-users"), ToolAPICall, args)
		require.NoError(t, err)
		out := decodeOK(t, res)
		assert.Empty(t, out.Redacted)
	})
}

// A service is called one thing in git and another in Sentry; repo_map is the
// one place that mapping lives, and it comes from the catalogue alone — no
// upstream, no cache.
func TestCall_RepoMap(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)
	ctx := ctxWithGroups("ringsrv-developers")

	t.Run("developer sees the tool with its repositories named", func(t *testing.T) {
		t.Parallel()
		got, err := s.List(ctx, "")
		require.NoError(t, err)
		require.Len(t, got.Tools, 3, "api_call, repo_map and help")

		tool := got.Tools[1]
		assert.Equal(t, ToolRepoMap, tool.Name)
		assert.Contains(t, tool.Description, "apisrv")
		assert.True(t, *tool.Annotations.ReadOnlyHint)
		assert.False(t, *tool.Annotations.OpenWorldHint, "it reads the catalogue and nothing else")
	})

	t.Run("table without an argument", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(ctx, ToolRepoMap, nil)
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)

		var out RepoMapResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		require.NotEmpty(t, out.Repos)
		assert.Equal(t, "prod", out.Env)

		e := out.Repos[0]
		assert.Equal(t, "apisrv", e.Repo)
		assert.Equal(t, "apisrv", e.SentrySlug)
		assert.Equal(t, "master", e.DefaultBranch)
		assert.Empty(t, e.Layers, "layers cost more than they explain in a table")
	})

	t.Run("one repository comes with its layers", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(ctx, ToolRepoMap, map[string]any{"repos": []any{"apisrv"}})
		require.NoError(t, err)

		var out RepoMapResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		require.Len(t, out.Repos, 1)
		assert.Equal(t, "pkg/rpc", out.Repos[0].Layers["rpc"])
		assert.NotEmpty(t, out.Repos[0].Exclude)
		assert.NotEmpty(t, out.Repos[0].Description, "the hand-written architecture note")
	})

	t.Run("unknown repository lists the ones that exist", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(ctx, ToolRepoMap, map[string]any{"repos": []any{"nope"}})
		require.NoError(t, err)
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeTargetUnknown, e.Code)
		assert.Contains(t, e.Repos, "apisrv", "repositories travel in their own field, not among targets")
	})

	// Symmetry with tools/list: a role that is not offered the tool does not
	// get an answer by calling it directly either.
	t.Run("a role without repositories is refused", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolRepoMap, nil)
		require.NoError(t, err)
		assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)
	})
}

// The contour header has to arrive at the upstream, not merely sit in the
// catalogue: the targets of a contour sit behind an Authentik forward-auth, and one
// missing header turns every answer into a redirect to the login page.
func TestCall_DefaultHeadersReachUpstream(t *testing.T) {
	t.Parallel()

	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	cat, err := target.Parse([]byte(`
Env = "dev"

[Defaults]
Headers = ["X-Authentik-Token: outpost-secret"]

[Profiles.test]
Description  = "dev · test upstream"
BaseURL      = "` + srv.URL + `"
Headers      = ["Authorization: server-token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["test"]
`))
	require.NoError(t, err)

	s := NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		MaxBytes:  1 << 20,
		JQTimeout: time.Second,
		Sessions:  ring.NewSessions(time.Hour, nil),
		Logger:    embedlog.Logger{},
	})

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
		"target": "test", "method": "GET", "path": "/api/v1/query",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)

	h := <-got
	assert.Equal(t, "outpost-secret", h.Get("X-Authentik-Token"), "the contour header travels with every call")
	assert.Equal(t, "server-token", h.Get("Authorization"), "and does not displace the target's own credentials")
}

// rpcServiceFor builds a ToolsService with two targets on one fake upstream: a
// JSON-RPC API behind one path (POST only, method and window in the body) and a
// Prometheus-shaped GET API with its query parameters listed.
func rpcServiceFor(t *testing.T, baseURL string) ToolsService {
	t.Helper()
	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.rpc]
Description  = "dev · JSON-RPC upstream"
BaseURL      = "` + baseURL + `"
Headers      = ["Authorization: Bearer server-token"]
AllowMethods = ["POST"]
AllowPaths   = ["POST ^/api/v1/rpc/$"]
ReadOnlyPost = true
AllowRPCMethods = ["host.list", "metric.queryRange"]
TimeBodyParams  = ["params.start", "params.end"]
TimeFormat      = "unix"

[Profiles.prom]
Description  = "dev · Prometheus"
BaseURL      = "` + baseURL + `"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query(_range)?$"]
AllowQueryParams = ["query", "time", "start", "end", "step>=15s", "limit<=100"]
TimeParams   = ["start", "end", "time"]
TimeFormat   = "unix"

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["rpc", "prom"]
`))
	require.NoError(t, err)
	return NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		JQTimeout: time.Second,
		Logger:    embedlog.Logger{},
	})
}

// Query parameters are read against the profile's list once the path has
// passed: a name outside it, a value outside a bound and a credential in the
// query string are refused with the list and never reach the upstream. A call
// inside the list goes through as written, the time parameters expanded.
func TestCall_QueryParams(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	s := rpcServiceFor(t, srv.URL)
	ctx := ctxWithGroups("ringsrv-users")
	call := func(target, method, path string) mcp.ToolCallResult {
		res, err := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": target, "method": method, "path": path}))
		require.NoError(t, err)
		return res
	}

	list := []string{"query", "time", "start", "end", "step>=15s", "limit<=100"}
	for path, want := range map[string]string{
		"/api/v1/query?query=up&pretty=1":                           `"pretty" is not allowed`,
		"/api/v1/query_range?query=up&start=now-1h&end=now&step=5s": "outside step>=15s",
	} {
		e := decodeErr(t, call("prom", http.MethodGet, path))
		assert.Equal(t, ErrCodeQueryParamNotAllowed, e.Code, path)
		assert.Contains(t, e.Message, want, path)
		assert.Equal(t, list, e.QueryParams, "the error is the documentation")
	}
	// The denied names hold on a profile without a list of its own, and
	// they are read before the body is.
	e := decodeErr(t, call("rpc", http.MethodPost, "/api/v1/rpc/?private_token=abc"))
	assert.Equal(t, ErrCodeQueryParamNotAllowed, e.Code)
	assert.Empty(t, e.QueryParams, "nothing to list: the profile takes any name but this one")
	// And in the body: a framework that folds the body into params would
	// read sudo out of it just the same.
	res, err := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{
		"target": "rpc", "method": http.MethodPost, "path": "/api/v1/rpc/",
		"body": `{"jsonrpc":"2.0","id":1,"method":"host.list","sudo":"root"}`,
	}))
	require.NoError(t, err)
	e = decodeErr(t, res)
	assert.Equal(t, ErrCodeQueryParamNotAllowed, e.Code)
	assert.Contains(t, e.Message, `body key "sudo"`)
	mu.Lock()
	assert.Empty(t, seen, "a refused parameter never reaches the upstream")
	mu.Unlock()

	out := decodeOK(t, call("prom", http.MethodGet, "/api/v1/query_range?query=up{job=\"api\"}&start=now-1h&end=now&step=1m&limit=10"))
	assert.Equal(t, http.StatusOK, out.Status)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 1)
	assert.Regexp(t, `^/api/v1/query_range\?query=up\{job="api"\}&start=\d{10}&end=\d{10}&step=1m&limit=10$`, seen[0])
}

// An API behind one path is allow-listed by the method in the body: a refused
// method is answered like a refused path and never reaches the upstream, an
// allowed one goes through with its window expanded — the model writes now-1h
// and the upstream reads an integer.
func TestCall_RPCMethods(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
	}))
	t.Cleanup(srv.Close)
	s := rpcServiceFor(t, srv.URL)
	ctx := ctxWithGroups("ringsrv-users")
	call := func(body string) mcp.ToolCallResult {
		res, err := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "rpc", "method": "POST", "path": "/api/v1/rpc/", "body": body}))
		require.NoError(t, err)
		return res
	}

	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"alert.silence","params":{}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"host.list"},{"jsonrpc":"2.0","id":2,"method":"alert.silence"}]`,
		``,
		`{"jsonrpc":"2.0","id":1}`,
	} {
		e := decodeErr(t, call(body))
		assert.Equal(t, ErrCodeRPCMethodNotAllowed, e.Code, body)
		assert.Equal(t, []string{"host.list", "metric.queryRange"}, e.RPCMethods, "the error is the documentation")
	}
	mu.Lock()
	assert.Empty(t, bodies, "a refused method never reaches the upstream")
	mu.Unlock()

	out := decodeOK(t, call(`{"jsonrpc":"2.0","id":1,"method":"Metric.QueryRange","params":{"query":"up","start":"now-1h","end":"now","step":"5m"}}`))
	assert.Equal(t, http.StatusOK, out.Status)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1)
	var sent struct {
		Method string `json:"method"`
		Params struct {
			Start int64  `json:"start"`
			End   int64  `json:"end"`
			Step  string `json:"step"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal([]byte(bodies[0]), &sent), bodies[0])
	assert.Equal(t, "Metric.QueryRange", sent.Method, "the method goes as written; the upstream does not care about case either")
	assert.Equal(t, "5m", sent.Params.Step)
	assert.InDelta(t, time.Now().Unix(), sent.Params.End, 5)
	assert.Equal(t, int64(3600), sent.Params.End-sent.Params.Start)
}
