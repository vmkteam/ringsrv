package target

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An authorized_keys line for a host key, as ssh-keyscan prints it.
const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGvDm1Jt1vVQ4Pc2bW6wR0Y0r9Ry9n0bS1E0w8QvWJ8y"

const sftpTOML = `
Env = "prod"

[Profiles.feeds]
Description      = "prod · Feeds"
BaseURL          = "sftp://files.example.com:2222"
AllowMethods     = ["GET"]
AllowPaths       = ["^/files/(list|read)$"]
AllowQueryParams = ["path", "offset", "limit<=1000", "grep", "sheet", "entry"]
Store            = "sftp"
Root             = "/upload"
SFTPUser         = "reader"
SFTPPassword     = "secret"
SFTPHostKey      = "` + testHostKey + `"

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["feeds"]
`

const davTOML = `
Env = "prod"

[Profiles.share]
Description  = "prod · Share"
BaseURL      = "https://dav.example.com"
Headers      = ["Authorization: Basic cmVhZGVyOnNlY3JldA=="]
AllowMethods = ["GET"]
AllowPaths   = ["^/files/read$"]
Store        = "webdav"
Root         = "/remote.php/dav/files/reader/shared"

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["share"]
`

func TestParse_Stores(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(sftpTOML))
	require.NoError(t, err)
	p, ok := c.Profile("feeds")
	require.True(t, ok)
	assert.Equal(t, FilesListPath, p.StoreRoute("GET", "/files/list?path=/a"))
	assert.Equal(t, FilesReadPath, p.StoreRoute("get", "/files/read"))
	assert.Empty(t, p.StoreRoute("POST", "/files/read"))
	assert.Empty(t, p.StoreRoute("GET", "/files/download"))
	assert.Empty(t, p.StoreRoute("GET", "/files/../files/read"))

	c, err = Parse([]byte(davTOML))
	require.NoError(t, err)
	p, ok = c.Profile("share")
	require.True(t, ok)
	assert.Equal(t, FilesReadPath, p.StoreRoute("GET", "/files/read?path=/a.csv"))
}

func TestParse_RejectsBrokenStores(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		toml, old, new, want string
	}{
		"unknown kind":    {sftpTOML, `Store            = "sftp"`, `Store            = "ftp"`, `field Store "ftp"`},
		"no host key":     {sftpTOML, `SFTPHostKey      = "` + testHostKey + `"`, ``, "needs SFTPHostKey"},
		"broken host key": {sftpTOML, testHostKey, "ssh-ed25519 nonsense", "SFTPHostKey"},
		"no credential":   {sftpTOML, `SFTPPassword     = "secret"`, ``, "one of SFTPPassword and SFTPKey"},
		"two credentials": {sftpTOML, `SFTPPassword     = "secret"`, `SFTPPassword = "secret"` + "\nSFTPKey = \"x\"", "one of SFTPPassword and SFTPKey"},
		"no user":         {sftpTOML, `SFTPUser         = "reader"`, ``, "needs SFTPUser"},
		"user in url":     {sftpTOML, `sftp://files.example.com:2222`, `sftp://reader@files.example.com`, "sftp://host[:port] only"},
		"http for sftp":   {sftpTOML, `sftp://files.example.com:2222`, `https://files.example.com`, "must be sftp://host[:port]"},
		"sftp for webdav": {davTOML, `https://dav.example.com`, `sftp://dav.example.com`, "BaseURL"},
		"headers on sftp": {sftpTOML, `Store            = "sftp"`, `Store            = "sftp"` + "\nHeaders = [\"X-A: b\"]", "sends no headers"},
		"relative root":   {sftpTOML, `Root             = "/upload"`, `Root             = "upload"`, "absolute clean path"},
		"dirty root":      {sftpTOML, `Root             = "/upload"`, `Root             = "/upload/../etc"`, "absolute clean path"},
		"no route":        {sftpTOML, `^/files/(list|read)$`, `^/files/download$`, "lets neither"},
		"another path":    {sftpTOML, `["^/files/(list|read)$"]`, `["^/files/(list|read)$", "^/etc/passwd$"]`, "a store answers nothing else"},
		"no path param":   {sftpTOML, `["path", `, `[`, `AllowQueryParams has no "path"`},
		"writes":          {sftpTOML, `AllowMethods     = ["GET"]`, `AllowMethods     = ["GET", "PUT"]` + "\nWrite = true", "read-only"},
		"with files":      {davTOML, `Store        = "webdav"`, `Store        = "webdav"` + "\nFiles = \"slack\"", "Files, PublicOnly and PathPrefix"},
		"sftp on webdav":  {davTOML, `Store        = "webdav"`, `Store        = "webdav"` + "\nSFTPUser = \"x\"", "belong to Store"},
		"sftp on an api":  {davTOML, `Store        = "webdav"`, `SFTPUser = "x"`, "belong to Store"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Contains(t, tc.toml, tc.old, "the case edits something")
			_, err := Parse([]byte(strings.Replace(tc.toml, tc.old, tc.new, 1)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
