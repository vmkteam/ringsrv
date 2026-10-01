package target

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A catalogue whose chat profile serves the file route.
const filesTOML = `
Env = "prod"

[Profiles.chat]
Description      = "prod · Chat"
BaseURL          = "https://chat.example.com"
Headers          = ["Authorization: Bearer chat-token"]
AllowMethods     = ["GET"]
AllowPaths       = ["^/api/conversations\\.replies$", "^/files/download$"]
AllowQueryParams = ["channel", "ts", "file", "offset", "limit<=1000", "grep"]
Files            = "slack"

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["chat"]
`

func TestParse_Files(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(filesTOML))
	require.NoError(t, err)
	p, ok := c.Profile("chat")
	require.True(t, ok)

	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/files/download?file=F0123456789", true},
		{"get", "/files/download", true},
		{"GET", "/files/download/", false},
		{"GET", "/files/%64ownload?file=F0123456789", false},
		{"POST", "/files/download?file=F0123456789", false},
		{"GET", "/api/conversations.replies?channel=C0123456789", false},
		{"GET", "/files/download/../download", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, p.ServesFiles(tc.method, tc.path), "%s %s", tc.method, tc.path)
	}
}

// A profile without Files never serves the route, whatever its allowlist says.
func TestServesFiles_NeedsTheField(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(strings.Replace(filesTOML, `Files            = "slack"`, "", 1)))
	require.NoError(t, err)
	p, ok := c.Profile("chat")
	require.True(t, ok)
	assert.False(t, p.ServesFiles("GET", "/files/download?file=F0123456789"))
}

func TestParse_RejectsBrokenFiles(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		toml string
		want string
	}{
		"unknown provider": {
			toml: strings.Replace(filesTOML, `Files            = "slack"`, `Files            = "teams"`, 1),
			want: `field Files "teams"`,
		},
		"path not allowed": {
			toml: strings.Replace(filesTOML, `, "^/files/download$"]`, `]`, 1),
			want: "does not let GET /files/download through",
		},
		"no file parameter": {
			toml: strings.Replace(filesTOML, `"ts", "file", `, `"ts", `, 1),
			want: `AllowQueryParams has no "file"`,
		},
		"files.info allowed": {
			toml: strings.Replace(filesTOML, `"^/files/download$"]`, `"^/files/download$", "^/api/files\\.info$"]`, 1),
			want: "lets GET /api/files.info through",
		},
		"everything allowed": {
			toml: strings.Replace(filesTOML, `"^/api/conversations\\.replies$"`, `"^/api/.*$"`, 1),
			want: "lets GET /api/files.list through",
		},
		"no authorization": {
			toml: strings.Replace(filesTOML, `["Authorization: Bearer chat-token"]`, `["X-Chat-Token: chat-token"]`, 1),
			want: "sends no Authorization header",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.toml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// The route reads its parameters through the same splitter the allowlist uses:
// a ; separates pairs for both, and a %3B is a ; inside a value for both.
func TestQueryPairs(t *testing.T) {
	t.Parallel()
	got := QueryPairs("/files/download?file=F0123456789&grep=a%3Bb;offset=10&=x&limit&grep=two+words#frag")
	assert.Equal(t, []QueryPair{
		{Key: "file", Value: "F0123456789"},
		{Key: "grep", Value: "a;b"},
		{Key: "offset", Value: "10"},
		{Key: "limit", Value: ""},
		{Key: "grep", Value: "two words"},
	}, got)
	assert.Empty(t, QueryPairs("/files/download"))
}
