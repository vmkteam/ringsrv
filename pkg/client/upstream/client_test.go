package upstream

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
)

// profileFor builds a catalogue with one profile pointing at the test server.
// Going through Parse rather than a struct literal keeps the compiled patterns
// in place — Allows depends on them.
// targetFor is what the domain would hand this package: a destination and the
// headers to send, nothing about roles or allowlists.
func targetFor(baseURL string) Target {
	return Target{
		Name:    "test",
		BaseURL: baseURL,
		Headers: []string{"Authorization: server-token", "X-Scope-OrgID: prod"},
	}
}

func TestDo_PassesPathAndQueryVerbatim(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	// PromQL lives in the query string: braces, quotes and order have to survive
	// untouched, which is why the query is never re-encoded.
	const q = `query=up{job="apisrv"}&step=60s`
	resp, err := c.Do(context.Background(), targetFor(srv.URL), Request{Method: http.MethodGet, Path: "/api/v1/query?" + q})
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "/api/v1/query", gotPath)
	assert.Equal(t, q, gotQuery)
	assert.JSONEq(t, `{"status":"success"}`, string(resp.Body))
}

// D1 and D5 in one test: the upstream sees the profile's credentials and
// nothing the caller might have attached.
func TestDo_SendsOnlyProfileHeaders(t *testing.T) {
	t.Parallel()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	_, err := c.Do(context.Background(), targetFor(srv.URL), Request{Method: http.MethodPost, Path: "/api/v1/query", Body: `{"a":1}`})
	require.NoError(t, err)

	assert.Equal(t, "server-token", got.Get("Authorization"), "the profile's credential reaches the upstream")
	assert.Equal(t, "prod", got.Get("X-Scope-Orgid"), "and so does the tenant the server decided on")
	assert.Equal(t, "application/json", got.Get("Content-Type"))
}

func TestDo_UpstreamErrorsAndTimeout(t *testing.T) {
	t.Parallel()

	t.Run("error status comes back as a status", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}))
		defer srv.Close()

		resp, err := New(Options{Timeout: 5 * time.Second}).
			Do(context.Background(), targetFor(srv.URL), Request{Method: http.MethodGet, Path: "/api/v1/fail"})
		require.NoError(t, err, "an upstream saying 500 is an answer, not a transport failure")
		assert.Equal(t, http.StatusInternalServerError, resp.Status)
		assert.Contains(t, string(resp.Body), "boom")
	})

	t.Run("slow upstream times out", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
		}))
		defer srv.Close()

		_, err := New(Options{Timeout: 50 * time.Millisecond}).
			Do(context.Background(), targetFor(srv.URL), Request{Method: http.MethodGet, Path: "/api/v1/slow"})
		require.ErrorIs(t, err, ErrTimeout)
	})

	t.Run("unreachable host is a plain error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing is listening any more

		_, err := New(Options{Timeout: time.Second}).
			Do(context.Background(), targetFor(url), Request{Method: http.MethodGet, Path: "/api/v1/query"})
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrTimeout)
	})
}

// A body of unknown size must not be read into memory whole: the limit is what
// stands between one careless query and the instance.
func TestDo_BodyLimit(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 10_000)))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second, MaxBodyBytes: 1000})
	resp, err := c.Do(context.Background(), targetFor(srv.URL), Request{Method: http.MethodGet, Path: "/api/v1/big"})
	require.NoError(t, err)
	assert.True(t, resp.Truncated)
	assert.Len(t, resp.Body, 1000)
}

func TestDo_ConcurrencyLimit(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := New(Options{Timeout: 5 * time.Second, MaxConcurrent: 1})
	p := targetFor(srv.URL)

	go func() {
		_, _ = c.Do(context.Background(), p, Request{Method: http.MethodGet, Path: "/api/v1/slow"})
	}()
	time.Sleep(50 * time.Millisecond) // let the first call take the only slot

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Do(ctx, p, Request{Method: http.MethodGet, Path: "/api/v1/query"})
	require.ErrorIs(t, err, ErrTimeout, "the second call waits for a slot instead of piling on")
}

// The metrics come from appkit's transport, which is why there is no metric of
// our own here. What has to be checked is the label: without the caller name
// every target lands in one series and the numbers stop being useful.
func TestDo_MetricsAreLabelledByTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	c := New(Options{AppName: "ringsrv", Version: "test", Timeout: 5 * time.Second})
	target := targetFor(srv.URL)
	target.Name = "prom-under-test"
	_, err := c.Do(t.Context(), target, Request{Method: http.MethodGet, Path: "/api/v1/query"})
	require.NoError(t, err)

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if f.GetName() != "app_http_client_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "caller" && l.GetValue() == "prom-under-test" {
					found = true
				}
			}
		}
	}
	assert.True(t, found, "the call must be counted against its own target")
}

// A redirect is a request to a host the allowlist never saw, and Go would
// forward every custom header along with it. The 3xx comes back as an answer
// with its Location, and nothing is sent anywhere else.
func TestDo_DoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	leaked := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/login", http.StatusFound)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Do(context.Background(), targetFor(srv.URL), Request{Method: http.MethodGet, Path: "/api/v1/query"})
	require.NoError(t, err)

	assert.Equal(t, http.StatusFound, resp.Status)
	assert.Equal(t, elsewhere.URL+"/login", resp.Location)
	assert.False(t, leaked, "the redirect target must never see the request")
}

// The catalogue refuses a profile that allows a call to set a header it sends
// itself, but the transport does not depend on that being right: the profile's
// headers are written last, so a credential of ours cannot be displaced by an
// argument whatever any layer above believes.
func TestDo_ProfileHeadersBeatCallHeaders(t *testing.T) {
	t.Parallel()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	target := Target{Name: "t", BaseURL: srv.URL, Headers: []string{"Authorization: server-token"}}
	_, err := New(Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}).
		Do(context.Background(), target, Request{
			Method:  http.MethodGet,
			Path:    "/api/v1/query",
			Headers: map[string]string{"Authorization": "caller-token", "Platform": "web"},
		})
	require.NoError(t, err)

	assert.Equal(t, "server-token", got.Get("Authorization"))
	assert.Equal(t, "web", got.Get("Platform"), "a header of its own still travels")
}
