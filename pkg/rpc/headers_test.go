package rpc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
)

// echoHeaders answers with the request headers the test cares about, so a test
// asserts on what the upstream actually received rather than on our intent.
func echoHeaders(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := make(map[string]string, len(names))
		for _, n := range names {
			got[n] = r.Header.Get(n)
		}
		w.Header().Set("Content-Type", "application/json")
		// No require here: a failed assertion inside a handler runs on another
		// goroutine, and testify cannot fail the test from there.
		_ = json.NewEncoder(w).Encode(got)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCall_AllowedHeaderReachesTheUpstream(t *testing.T) {
	t.Parallel()
	srv := echoHeaders(t, "Authorization2", "Platform", "Authorization")

	res, err := auditService(t, srv.URL, "", embedlog.Logger{}).Call(
		ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query",
			"headers": map[string]any{"Authorization2": "caller-key", "Platform": "web"},
		}))
	require.NoError(t, err)

	out := decodeOK(t, res)
	got, ok := out.Data.(map[string]any)
	require.True(t, ok, "the echo answers an object")
	assert.Equal(t, "caller-key", got["Authorization2"])
	assert.Equal(t, "web", got["Platform"])
	assert.Equal(t, "server-token", got["Authorization"],
		"the profile's own credential travels unchanged alongside the call's")
}

// The refusal carries the names that would have worked. With a single tool the
// error is the documentation, and a model that has to guess spends a turn per
// guess.
func TestCall_UnlistedHeaderIsRefused(t *testing.T) {
	t.Parallel()
	srv := echoHeaders(t, "X-Impersonate")

	res, err := auditService(t, srv.URL, "", embedlog.Logger{}).Call(
		ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query",
			"headers": map[string]any{"X-Impersonate": "1"},
		}))
	require.NoError(t, err)

	e := decodeErr(t, res)
	assert.Equal(t, ErrCodeHeaderNotAllowed, e.Code)
	assert.Contains(t, e.Message, "X-Impersonate")
	assert.Equal(t, []string{"Authorization2", "Platform"}, e.Headers)
}

func TestCall_HeaderValuesAreChecked(t *testing.T) {
	t.Parallel()
	srv := echoHeaders(t, "Platform")
	svc := auditService(t, srv.URL, "", embedlog.Logger{})

	call := func(headers map[string]any) ToolError {
		t.Helper()
		res, err := svc.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query", "headers": headers,
		}))
		require.NoError(t, err)
		return decodeErr(t, res)
	}

	// A value carrying CR or LF is how one request becomes two.
	assert.Equal(t, ErrCodeBadArgs, call(map[string]any{"Platform": "web\r\nX-Admin: 1"}).Code)
	assert.Equal(t, ErrCodeBadArgs, call(map[string]any{"Platform": ""}).Code)

	many := map[string]any{}
	for i := range MaxCallHeaders + 1 {
		many[string(rune('a'+i))] = "v"
	}
	assert.Equal(t, ErrCodeBadArgs, call(many).Code, "the cap is checked before the names")
}

// The audit answers "who set what header", never "what was in it": the reason
// to set a header from a call is usually that it carries a credential.
func TestAudit_RecordsHeaderNamesNotValues(t *testing.T) {
	srv := echoHeaders(t, "Authorization2")

	recs := captureLog(t, func(l embedlog.Logger) {
		_, err := auditService(t, srv.URL, "", l).Call(
			ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
				"target": "test", "method": "GET", "path": "/api/v1/query",
				"headers": map[string]any{"Platform": "web", "Authorization2": "s3cret-key"},
			}))
		require.NoError(t, err)
	})

	audit := itemRecords(recs)
	require.Len(t, audit, 1)
	assert.Equal(t, "Authorization2,Platform", audit[0]["headers"], "sorted, so one call is one line")

	line, err := json.Marshal(recs)
	require.NoError(t, err)
	assert.NotContains(t, string(line), "s3cret-key", "a header value never reaches the log")
}

// The argument is paid for on every tools/list, so a role whose targets
// all refuse headers is not offered it.
func TestList_HeadersArgumentIsOfferedOnlyWhereItWorks(t *testing.T) {
	t.Parallel()
	srv := echoHeaders(t, "Platform")

	withHeaders, err := auditService(t, srv.URL, "", embedlog.Logger{}).List(ctxWithGroups("ringsrv-users"), "")
	require.NoError(t, err)
	assert.Contains(t, string(withHeaders.Tools[0].InputSchema), `"headers"`)

	without, err := serviceFor(t, srv.URL, "", 0).List(ctxWithGroups("ringsrv-users"), "")
	require.NoError(t, err)
	assert.NotContains(t, string(without.Tools[0].InputSchema), `"headers"`)
	assert.Less(t, len(without.Tools[0].InputSchema), len(withHeaders.Tools[0].InputSchema))
}

// A name that cannot travel is refused as an argument, not handed to net/http
// to fail as a transport error. The whole feature exists so that a header
// problem is reported where the header was written.
func TestCall_HeaderNamesAreChecked(t *testing.T) {
	t.Parallel()
	srv := echoHeaders(t, "Platform")
	svc := auditService(t, srv.URL, "", embedlog.Logger{})

	call := func(headers map[string]any) ToolError {
		t.Helper()
		res, err := svc.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query", "headers": headers,
		}))
		require.NoError(t, err)
		return decodeErr(t, res)
	}

	// Surrounding whitespace is trimmed, the way the catalogue trims its own
	// names; anything else in the middle of a name cannot travel.
	bad := call(map[string]any{"Plat form": "web"})
	assert.Equal(t, ErrCodeBadArgs, bad.Code)
	assert.Contains(t, bad.Message, "valid header name")

	assert.Equal(t, ErrCodeBadArgs, call(map[string]any{"Plat\nform": "web"}).Code)

	// Two spellings of one header are a map with room for both, and which of
	// them reached the upstream would otherwise be map iteration order.
	dup := call(map[string]any{"Platform": "web", "platform": "ios"})
	assert.Equal(t, ErrCodeBadArgs, dup.Code)
	assert.Contains(t, dup.Message, "twice")
}

// The name reaches the upstream canonicalised, whatever case the model wrote.
func TestCall_HeaderNameIsCanonicalised(t *testing.T) {
	t.Parallel()
	srv := echoHeaders(t, "Authorization2")

	res, err := auditService(t, srv.URL, "", embedlog.Logger{}).Call(
		ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(map[string]any{
			"target": "test", "method": "GET", "path": "/api/v1/query",
			"headers": map[string]any{"authorization2": "caller-key"},
		}))
	require.NoError(t, err)

	got, ok := decodeOK(t, res).Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "caller-key", got["Authorization2"])
}
