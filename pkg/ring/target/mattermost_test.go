package target

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Mattermost profile kept to public channels, with the file route.
const mattermostTOML = `
Env = "prod"

[Profiles.mattermost]
Description      = "prod · Mattermost"
BaseURL          = "https://chat.example.com"
Headers          = ["Authorization: Bearer mm-token"]
AllowMethods     = ["GET", "POST"]
ReadOnlyPost     = true
AllowPaths       = ["^/api/v4/channels/[a-z0-9]{26}(/posts)?$", "POST ^/api/v4/teams/[a-z0-9]{26}/posts/search$", "^/files/download$"]
AllowQueryParams = ["page", "per_page", "file", "offset", "limit<=1000", "grep", "sheet", "entry"]
Files            = "mattermost"
PublicOnly       = "mattermost"

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["mattermost"]
`

func TestParse_Mattermost(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(mattermostTOML))
	require.NoError(t, err)
	p, ok := c.Profile("mattermost")
	require.True(t, ok)
	assert.Equal(t, PublicMattermost, p.PublicOnly)
	assert.True(t, p.ServesFiles("GET", "/files/download?file=abcdefghijklmnopqrstuvwxyz"))
}

func TestParse_RejectsBrokenMattermost(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		toml string
		want string
	}{
		"files without the check": {
			toml: strings.Replace(mattermostTOML, `PublicOnly       = "mattermost"`, "", 1),
			want: "PublicOnly is not",
		},
		"unknown check": {
			toml: strings.Replace(mattermostTOML, `PublicOnly       = "mattermost"`, `PublicOnly       = "teams"`, 1),
			want: `field PublicOnly "teams"`,
		},
		"own channels allowed": {
			toml: strings.Replace(mattermostTOML, `"^/files/download$"]`, `"^/files/download$", "^/api/v4/users/me/channels$"]`, 1),
			want: "lets GET /api/v4/users/me/channels through",
		},
		"members allowed": {
			toml: strings.Replace(mattermostTOML, `(/posts)?$"`, `(/posts|/members)?$"`, 1),
			want: "/members through",
		},
		"file info allowed": {
			toml: strings.Replace(mattermostTOML, `"^/files/download$"]`, `"^/files/download$", "^/api/v4/files/.*$"]`, 1),
			want: "lets GET /api/v4/files/abcdefghijklmnopqrstuvwxyz through",
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
