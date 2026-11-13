package files

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/sftpfs/sftptest"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/files/filestest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/webdav"
	"golang.org/x/text/encoding/charmap"
)

// storeTree lays out what both stores serve: a price list in windows-1251, a
// name with a space and Cyrillic, an archive, a folder with a file in it.
func storeTree(t *testing.T, dir string) {
	t.Helper()
	cp1251, err := charmap.Windows1251.NewEncoder().String("code;name\n809000001;Аспирин\n809000002;Бинт\n")
	require.NoError(t, err)
	write := func(name string, data []byte) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, data, 0o644))
	}
	write("prices.csv", []byte(cp1251))
	write("отчёт за ноябрь.csv", []byte("a;b\n1;2\n"))
	write("export.zip", filestest.Zip(t, filestest.Entry{Name: "stock.csv", Body: []byte("code;qty\n1;5\n")}))
	write("2026/11/feed.txt", []byte("first\nsecond\n"))
	write("v1..2.csv", []byte("a\n"))
}

func storeFetch(t *testing.T, prof *target.Profile, route, path string) (*Result, error) {
	t.Helper()
	client := upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20})
	return NewFetcher(client, nil).Store(context.Background(), prof, route, path, 32<<10)
}

func storeProfile(t *testing.T, body string) *target.Profile {
	t.Helper()
	toml := `
Env = "prod"

[Profiles.share]
Description      = "prod · Share"
AllowMethods     = ["GET"]
AllowPaths       = ["^/files/(list|read)$"]
AllowQueryParams = ["path", "offset", "limit<=1000", "grep", "sheet", "entry"]
` + body + `

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["share"]
`
	c, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	p, ok := c.Profile("share")
	require.True(t, ok)
	return p
}

// davServer serves the tree under /dav/shared, behind basic auth, with a file
// beside the root that no path may reach.
func davServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	storeTree(t, filepath.Join(dir, "shared"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("outside\n"), 0o644))
	h := &webdav.Handler{Prefix: "/dav", FileSystem: webdav.Dir(dir), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "reader" || p != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func davProfile(t *testing.T, url, password string) *target.Profile {
	t.Helper()
	auth := base64.StdEncoding.EncodeToString([]byte("reader:" + password))
	return storeProfile(t, fmt.Sprintf(`BaseURL = %q
Headers = ["Authorization: Basic %s"]
Store   = "webdav"
Root    = "/dav/shared"`, url, auth))
}

func TestWebDAV_List(t *testing.T) {
	t.Parallel()
	prof := davProfile(t, davServer(t), "secret")

	res, err := storeFetch(t, prof, target.FilesListPath, "/files/list")
	require.NoError(t, err)
	require.NotNil(t, res.Text)
	assert.Equal(t, contentsHeader, res.Text.Header)
	names := make([]string, 0, len(res.Text.Lines))
	for _, l := range res.Text.Lines {
		names = append(names, l[strings.LastIndex(l, "\t")+1:])
	}
	assert.Equal(t, []string{"2026/", "export.zip", "prices.csv", "v1..2.csv", "отчёт за ноябрь.csv"}, names,
		"a folder ends with /, nothing from beside the root; a name with .. is listed, only not read")

	res, err = storeFetch(t, prof, target.FilesListPath, "/files/list?path=/2026/11/")
	require.NoError(t, err)
	require.Len(t, res.Text.Lines, 1)
	assert.True(t, strings.HasPrefix(res.Text.Lines[0], "1: 13\t"), res.Text.Lines[0])
	assert.True(t, strings.HasSuffix(res.Text.Lines[0], "\tfeed.txt"))
	assert.Equal(t, "/2026/11", res.Meta.ID)
}

func TestWebDAV_Read(t *testing.T) {
	t.Parallel()
	prof := davProfile(t, davServer(t), "secret")

	res, err := storeFetch(t, prof, target.FilesReadPath, "/files/read?path=/prices.csv&grep=%D0%B1%D0%B8%D0%BD%D1%82")
	require.NoError(t, err)
	assert.Equal(t, []string{"3: 809000002;Бинт"}, res.Text.Lines)
	assert.Equal(t, "windows-1251", res.Text.Encoding)
	assert.Equal(t, "code;name", res.Text.Header)
	assert.Equal(t, "/prices.csv", res.Meta.ID)
	assert.NotEmpty(t, res.Meta.Modified)

	res, err = storeFetch(t, prof, target.FilesReadPath, "/files/read?path=%D0%BE%D1%82%D1%87%D1%91%D1%82%20%D0%B7%D0%B0%20%D0%BD%D0%BE%D1%8F%D0%B1%D1%80%D1%8C.csv")
	require.NoError(t, err, "a name with spaces and Cyrillic is escaped on the way out")
	assert.Equal(t, []string{"1: a;b", "2: 1;2"}, res.Text.Lines)

	res, err = storeFetch(t, prof, target.FilesReadPath, "/files/read?path=/export.zip&entry=stock.csv")
	require.NoError(t, err)
	assert.Equal(t, "stock.csv", res.Meta.Entry)
	assert.Equal(t, []string{"1: code;qty", "2: 1;5"}, res.Text.Lines)
}

func TestWebDAV_Refused(t *testing.T) {
	t.Parallel()
	url := davServer(t)
	prof := davProfile(t, url, "secret")

	for _, path := range []string{
		"/files/read?path=/2026",
		"/files/read?path=/",
		"/files/read",
		"/files/read?path=../secret.txt",
		"/files/read?path=/2026/../../secret.txt",
		"/files/read?path=%5Csecret.txt",
		"/files/read?file=F0123456789",
		"/files/list?path=/2026&entry=a",
	} {
		route := target.FilesReadPath
		if strings.HasPrefix(path, "/files/list") {
			route = target.FilesListPath
		}
		_, err := storeFetch(t, prof, route, path)
		require.ErrorIs(t, err, ErrBadParams, path)
	}

	_, err := storeFetch(t, prof, target.FilesReadPath, "/files/read?path=/missing.csv")
	var up *UpstreamError
	require.ErrorAs(t, err, &up)
	assert.Equal(t, http.StatusNotFound, up.Status)

	_, err = storeFetch(t, davProfile(t, url, "wrong"), target.FilesListPath, "/files/list")
	require.ErrorAs(t, err, &up)
	assert.Equal(t, http.StatusUnauthorized, up.Status)
}

// sftpStand serves the tree on SFTP, with a symlink to a file outside it and
// one to a folder outside it.
func sftpStand(t *testing.T) (*sftptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "upload")
	storeTree(t, root)
	outside := filepath.Join(dir, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "passwd"), []byte("root:x:0:0\n"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(outside, "passwd"), filepath.Join(root, "passwd.txt")))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "etc")))
	return sftptest.New(t, root), root
}

func sftpProfile(t *testing.T, srv *sftptest.Server, root, hostKey string) *target.Profile {
	t.Helper()
	return storeProfile(t, fmt.Sprintf(`BaseURL      = "sftp://%s"
Store        = "sftp"
Root         = %q
SFTPUser     = %q
SFTPPassword = %q
SFTPHostKey  = %q`, srv.Addr, root, srv.User, srv.Password, hostKey))
}

func TestSFTP_ListAndRead(t *testing.T) {
	t.Parallel()
	srv, root := sftpStand(t)
	prof := sftpProfile(t, srv, root, srv.HostKey)

	res, err := storeFetch(t, prof, target.FilesListPath, "/files/list")
	require.NoError(t, err)
	joined := strings.Join(res.Text.Lines, "\n")
	assert.Contains(t, joined, "\t2026/")
	assert.Contains(t, joined, "\tetc@", "a symlink is shown as one")
	assert.Contains(t, joined, "\tpasswd.txt@")

	res, err = storeFetch(t, prof, target.FilesReadPath, "/files/read?path=/2026/11/feed.txt")
	require.NoError(t, err)
	assert.Equal(t, []string{"1: first", "2: second"}, res.Text.Lines)

	res, err = storeFetch(t, prof, target.FilesReadPath, "/files/read?path=/prices.csv&offset=1")
	require.NoError(t, err)
	assert.Equal(t, []string{"2: 809000001;Аспирин", "3: 809000002;Бинт"}, res.Text.Lines)
}

// A symlink is not followed, wherever on the path it is.
func TestSFTP_LinksNotFollowed(t *testing.T) {
	t.Parallel()
	srv, root := sftpStand(t)
	prof := sftpProfile(t, srv, root, srv.HostKey)

	for _, tc := range []struct{ route, path string }{
		{target.FilesReadPath, "/files/read?path=/passwd.txt"},
		{target.FilesReadPath, "/files/read?path=/etc/passwd"},
		{target.FilesListPath, "/files/list?path=/etc"},
	} {
		_, err := storeFetch(t, prof, tc.route, tc.path)
		require.Error(t, err, tc.path)
		assert.Equal(t, CodeUnsupported, refusalCode(err), "%s: %v", tc.path, err)
		assert.NotContains(t, err.Error(), "root:x", tc.path)
	}
}

func TestSFTP_Refused(t *testing.T) {
	t.Parallel()
	srv, root := sftpStand(t)

	_, err := storeFetch(t, sftpProfile(t, srv, root, srv.HostKey), target.FilesReadPath, "/files/read?path=/missing.csv")
	var up *UpstreamError
	require.ErrorAs(t, err, &up)
	assert.Equal(t, http.StatusNotFound, up.Status)

	other, _ := sftpStand(t)
	_, err = storeFetch(t, sftpProfile(t, srv, root, other.HostKey), target.FilesListPath, "/files/list")
	require.Error(t, err, "a server whose key is not the pinned one")
	assert.Contains(t, err.Error(), "handshake")
}

// A name with .. in it is refused as a name WebDAV cannot carry here, not
// failed as a broken path in the HTTP layer.
func TestWebDAV_DottedName(t *testing.T) {
	t.Parallel()
	prof := davProfile(t, davServer(t), "secret")
	_, err := storeFetch(t, prof, target.FilesReadPath, "/files/read?path=/v1..2.csv")
	require.Error(t, err)
	assert.Equal(t, CodeUnsupported, refusalCode(err), err.Error())
}

// A server that takes the connection and then says nothing does not hold the
// call: the session has the client's timeout.
func TestSFTP_StalledServer(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	srv, root := sftpStand(t)
	prof := storeProfile(t, fmt.Sprintf(`BaseURL      = "sftp://%s"
Store        = "sftp"
Root         = %q
SFTPUser     = %q
SFTPPassword = %q
SFTPHostKey  = %q`, ln.Addr().String(), root, srv.User, srv.Password, srv.HostKey))

	client := upstream.New(upstream.Options{Timeout: 300 * time.Millisecond, MaxBodyBytes: 1 << 20, MaxConcurrent: 2})
	start := time.Now()
	_, err = NewFetcher(client, nil).Store(context.Background(), prof, target.FilesListPath, "/files/list", 32<<10)
	require.Error(t, err)
	require.ErrorIs(t, err, upstream.ErrTimeout)
	assert.Less(t, time.Since(start), 5*time.Second)
}
