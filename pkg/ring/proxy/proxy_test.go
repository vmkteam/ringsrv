package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/mcp"
)

// profileFor builds a redacting profile that points at the test server. Going
// through Parse keeps the compiled patterns in place.
func profileFor(t *testing.T, baseURL, redact string) *target.Profile {
	t.Helper()
	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.sentry]
Description  = "dev · Sentry"
BaseURL      = "` + baseURL + `"
Headers      = ["Authorization: token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/x$"]
Redact       = "` + redact + `"
RedactRules  = ["email"]

[Roles.viewer]
Description = "read"
Groups      = ["g"]
Tools       = ["api_call"]
Targets     = ["sentry"]
`))
	require.NoError(t, err)
	p, ok := cat.Profile("sentry")
	require.True(t, ok)
	return p
}

func manager() *Manager {
	return NewManager(upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}), time.Second, 32768)
}

// A body that is not JSON is still text about somebody. It used to bypass the
// profile's redaction on its way to the model.
func TestDo_RedactsNonJSONBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("rate limited; contact user@example.com"))
	}))
	t.Cleanup(srv.Close)

	ans, callErr := manager().Do(context.Background(), profileFor(t, srv.URL, "redact"), Request{Method: http.MethodGet, Path: "/api/x"})
	require.Nil(t, callErr)
	require.True(t, ans.AsText)
	assert.Equal(t, "rate limited; contact u***@example.com", ans.Text)
	assert.Equal(t, []string{"email"}, ans.Redacted.Names())
}

// The first bytes of a failing body travel in the error, and a 4xx from Sentry
// can quote the very event it refused.
func TestDo_RedactsErrorBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"no such user user@example.com"}`))
	}))
	t.Cleanup(srv.Close)

	_, callErr := manager().Do(context.Background(), profileFor(t, srv.URL, "redact"), Request{Method: http.MethodGet, Path: "/api/x"})
	require.NotNil(t, callErr)
	assert.Equal(t, FailUpstream, callErr.Kind)
	assert.Equal(t, http.StatusBadRequest, callErr.Status)
	assert.NotContains(t, callErr.Error(), "user@example.com")
	assert.Contains(t, callErr.Error(), "u***@example.com")

	// In warn mode the text is left alone, as for a document.
	_, callErr = manager().Do(context.Background(), profileFor(t, srv.URL, "warn"), Request{Method: http.MethodGet, Path: "/api/x"})
	require.NotNil(t, callErr)
	assert.Contains(t, callErr.Error(), "user@example.com")
}

// An oversized JSON answer travels as a marked string, cut where the argument
// said and never past the ceiling.
func TestDo_MaxBytesFromArgumentIsCapped(t *testing.T) {
	t.Parallel()
	big := `{"data":"` + strings.Repeat("x", ring.MaxAnswerBytes+4096) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(big))
	}))
	t.Cleanup(srv.Close)

	ans, callErr := manager().Do(context.Background(), profileFor(t, srv.URL, "off"), Request{Method: http.MethodGet, Path: "/api/x", MaxBytes: 10 << 20})
	require.Nil(t, callErr)
	require.True(t, ans.Truncated)
	assert.True(t, strings.HasSuffix(ans.Text, mcp.TruncateMarker))
	assert.LessOrEqual(t, len(ans.Text), ring.MaxAnswerBytes+len(mcp.TruncateMarker))
}

// A shared upstream answers for the contour the catalogue names, whatever the
// call asked for: Sentry serves prod and devel from one host.
func TestDo_ForcesProfileQuery(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.sentry]
Description  = "dev · Sentry"
BaseURL      = "` + srv.URL + `"
Headers      = ["Authorization: token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/x$"]
Query        = ["environment=devel"]

[Roles.viewer]
Description = "read"
Groups      = ["g"]
Tools       = ["api_call"]
Targets     = ["sentry"]
`))
	require.NoError(t, err)
	p, ok := cat.Profile("sentry")
	require.True(t, ok)

	_, callErr := manager().Do(context.Background(), p, Request{Method: http.MethodGet, Path: "/api/x?environment=production&limit=3"})
	require.Nil(t, callErr)
	assert.Equal(t, "limit=3&environment=devel", got)
}

// A window written as now-1h in a JSON body reaches the upstream as the
// integers the profile's TimeBodyParams name — the body's ExpandPath.
func TestDo_ExpandsBodyTime(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	t.Cleanup(srv.Close)

	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.rpc]
Description  = "dev · JSON-RPC"
BaseURL      = "` + srv.URL + `"
Headers      = ["Authorization: token"]
AllowMethods = ["POST"]
AllowPaths   = ["POST ^/rpc$"]
ReadOnlyPost = true
TimeBodyParams = ["params.start"]
TimeFormat     = "unix"

[Roles.viewer]
Description = "read"
Groups      = ["g"]
Tools       = ["api_call"]
Targets     = ["rpc"]
`))
	require.NoError(t, err)
	p, ok := cat.Profile("rpc")
	require.True(t, ok)

	_, callErr := manager().Do(context.Background(), p, Request{Method: http.MethodPost, Path: "/rpc", Body: `{"method":"q","params":{"start":"now","untouched":"now-1h"}}`})
	require.Nil(t, callErr)
	assert.Regexp(t, `^\{"method":"q","params":\{"start":\d{10},"untouched":"now-1h"\}\}$`, got)
}
