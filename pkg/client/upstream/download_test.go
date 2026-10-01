package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The download carries the credential and nothing else of the profile: a
// contour pin or a forward-auth secret is for the API host, not a file host.
func TestDownload_SendsOnlyAuthorization(t *testing.T) {
	t.Parallel()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("a;b\n1;2\n"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20})
	resp, err := c.Download(context.Background(), targetFor(srv.URL), srv.URL+"/files-pri/T1-F1/report.csv", DownloadOptions{})
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "a;b\n1;2\n", string(resp.Body))
	assert.Equal(t, "text/csv", resp.ContentType)
	assert.Equal(t, "server-token", got.Get("Authorization"))
	assert.Empty(t, got.Get("X-Scope-Orgid"), "only the credential travels to a file host")
	assert.NotEmpty(t, got.Get("User-Agent"))
}

// A URL out of an answer is upstream data: it goes nowhere the target or the
// caller's fixed list does not name.
func TestDownload_RefusesForeignURLs(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	opts := DownloadOptions{Hosts: []string{"files.example.com"}}
	for _, raw := range []string{
		"https://evil.example.com/files-pri/T1-F1/a.csv",
		"https://files.example.com@evil.example.com/files-pri/T1-F1/a.csv",
		"https://user@files.example.com/files-pri/T1-F1/a.csv",
		"http://files.example.com/files-pri/T1-F1/a.csv",
		"https://files.example.com:8443/files-pri/T1-F1/a.csv",
		"https://sub.files.example.com/files-pri/T1-F1/a.csv",
		"https:files.example.com/a.csv",
		"//files.example.com/a.csv",
		"/files-pri/T1-F1/a.csv",
		srv.URL + "/files-pri/../admin",
		srv.URL + "/files-pri/%2e%2e/admin",
	} {
		_, err := c.Download(context.Background(), targetFor(srv.URL), raw, opts)
		require.Error(t, err, raw)
	}
	assert.Zero(t, hits, "a refused URL is never requested")
}

// The listed host passes the check; the request itself is what the transport
// would make, so it is only checked to get as far as dialing.
func TestDownloadURL_AllowsListedHost(t *testing.T) {
	t.Parallel()
	got, err := downloadURL("https://api.example.com", "https://files.example.com/files-pri/T1-F1/a.csv#x", []string{"files.example.com"})
	require.NoError(t, err)
	assert.Equal(t, "https://files.example.com/files-pri/T1-F1/a.csv", got, "the fragment does not travel")

	// The path ends in the file's name, and dots in a name are not a step up.
	got, err = downloadURL("https://api.example.com", "https://files.example.com/files-pri/T1-F1/download/report..csv", []string{"files.example.com"})
	require.NoError(t, err)
	assert.Equal(t, "https://files.example.com/files-pri/T1-F1/download/report..csv", got)
}

// A redirect is where a file host sends a request it does not accept — the
// login page. It comes back as an answer and is never followed.
func TestDownload_DoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	leaked := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/signin", http.StatusFound)
	}))
	defer srv.Close()

	resp, err := New(Options{Timeout: 5 * time.Second}).Download(context.Background(), targetFor(srv.URL), srv.URL+"/files-pri/T1-F1/a.csv", DownloadOptions{})
	require.NoError(t, err)
	assert.Equal(t, http.StatusFound, resp.Status)
	assert.False(t, leaked)
}

// The download has its own limit, larger or smaller than the client's: a file
// is not an API answer.
func TestDownload_OwnLimit(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second, MaxBodyBytes: 4})
	resp, err := c.Download(context.Background(), targetFor(srv.URL), srv.URL+"/f", DownloadOptions{MaxBytes: 6})
	require.NoError(t, err)
	assert.Equal(t, "012345", string(resp.Body))
	assert.True(t, resp.Truncated)

	resp, err = c.Download(context.Background(), targetFor(srv.URL), srv.URL+"/f", DownloadOptions{MaxBytes: 64})
	require.NoError(t, err)
	assert.Equal(t, "0123456789", string(resp.Body))
	assert.False(t, resp.Truncated)
}
