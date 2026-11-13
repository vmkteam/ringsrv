package rpc

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"golang.org/x/net/webdav"
)

// A WebDAV store through api_call: a listing and a file in one batch, and
// any other path refused, since a store passes nothing on.
func TestStore_ThroughAPICall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "shared"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "shared", "feed.csv"), []byte("code;qty\n1;5\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "beside.txt"), []byte("outside\n"), 0o644))
	h := &webdav.Handler{FileSystem: webdav.Dir(dir), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "reader" || p != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.share]
Description      = "dev · Share"
BaseURL          = "` + srv.URL + `"
Headers          = ["Authorization: Basic ` + base64.StdEncoding.EncodeToString([]byte("reader:secret")) + `"]
AllowMethods     = ["GET"]
AllowPaths       = ["^/(files/(list|read)|beside\\.txt)$"]
AllowQueryParams = ["path", "offset", "limit<=1000", "grep"]
Store            = "webdav"
Root             = "/shared"

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["share"]
`))
	require.NoError(t, err)
	s := NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		MaxBytes:  32 << 10,
		JQTimeout: time.Second,
		Sessions:  ring.NewSessions(time.Hour, nil),
		Logger:    embedlog.Logger{},
		Docs:      testDocs(t),
	})
	call := func(path string) map[string]any {
		return map[string]any{"target": "share", "method": "GET", "path": path}
	}

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, map[string]any{"calls": []any{
		call("/files/list"),
		call("/files/read?path=/feed.csv"),
		call("/beside.txt"),
	}})
	require.NoError(t, err)
	out := decodeBatch(t, res)
	require.Len(t, out.Results, 3)
	require.Nil(t, out.Results[0].Error)
	assert.Contains(t, res.Content[0].Text, `feed.csv`)
	require.Nil(t, out.Results[1].Error)
	data, ok := out.Results[1].Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"1: code;qty", "2: 1;5"}, data["lines"])
	require.NotNil(t, out.Results[2].Error, "a path the allowlist lets through still does not reach the server")
	assert.Equal(t, ErrCodeBadArgs, out.Results[2].Error.Code)
	assert.NotContains(t, res.Content[0].Text, "outside")
}
