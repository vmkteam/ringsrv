package target

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AllowHeaders is the one place a call can influence what an upstream sees, so
// every way of writing it wrong fails at load time rather than at request time.
// A header that is quietly dropped reads to the model as the upstream refusing
// the call, and that is debugged on the wrong side of the wire.
func TestParse_AllowHeadersRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ allow, want string }{
		"identity header of the upstream": {
			allow: `["X-Impersonate"]`,
			want:  "may never be set by a call",
		},
		"forwarded family": {
			allow: `["X-Forwarded-For"]`,
			want:  "says who the caller is",
		},
		"forward-auth family": {
			allow: `["X-Authentik-Username"]`,
			want:  "says who the caller is",
		},
		"transport header": {
			allow: `["Content-Length"]`,
			want:  "may never be set by a call",
		},
		"header the profile already sends": {
			allow: `["Authorization"]`,
			want:  "the profile wins",
		},
		"same header in another case": {
			allow: `["authorization"]`,
			want:  "the profile wins",
		},
		"not a header name": {
			allow: `["Bad Header"]`,
			want:  "is not a valid header name",
		},
		"listed twice": {
			allow: `["Platform", "platform"]`,
			want:  "twice",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			toml := strings.Replace(validTOML,
				`AllowPaths   = ["^/api/v1/query$"]`,
				`AllowPaths   = ["^/api/v1/query$"]`+"\nAllowHeaders = "+tc.allow, 1)
			_, err := Parse([]byte(toml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// The catalogue-wide headers count as the profile's own: a target that does not
// skip them cannot hand the contour's forward-auth to a call.
func TestParse_AllowHeadersAgainstCatalogueDefaults(t *testing.T) {
	t.Parallel()
	toml := strings.Replace(validTOML, "[Profiles.prom]",
		"[Defaults]\nHeaders = [\"X-Contour: dev\"]\n\n[Profiles.prom]", 1)
	toml = strings.Replace(toml,
		`AllowPaths   = ["^/api/v1/query$"]`,
		`AllowPaths   = ["^/api/v1/query$"]`+"\nAllowHeaders = [\"X-Contour\"]", 1)

	_, err := Parse([]byte(toml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the profile wins")

	// The same name is fine once the profile stops receiving the defaults:
	// then nothing of ours is being overridden.
	skipped := strings.Replace(toml, "AllowHeaders =", "SkipDefaultHeaders = true\nAllowHeaders =", 1)
	c, err := Parse([]byte(skipped))
	require.NoError(t, err)
	assert.True(t, c.Profiles["prom"].HeaderAllowed("X-Contour"))
}

func TestProfile_HeaderAllowed(t *testing.T) {
	t.Parallel()
	toml := strings.Replace(validTOML,
		`AllowPaths   = ["^/api/v1/query$"]`,
		`AllowPaths   = ["^/api/v1/query$"]`+"\nAllowHeaders = [\"Authorization2\", \"Platform\"]", 1)
	c, err := Parse([]byte(toml))
	require.NoError(t, err)
	p := c.Profiles["prom"]

	assert.True(t, p.HeaderAllowed("Authorization2"))
	assert.True(t, p.HeaderAllowed("authorization2"), "HTTP header names are case-insensitive")
	assert.True(t, p.AllowsAnyHeader())
	assert.False(t, p.HeaderAllowed("X-Anything"))
}

// A profile that says nothing about headers accepts none. This is the state
// every shipped catalogue is in, and it is what keeps the argument out of the
// api_call schema for roles that have no use for it.
func TestProfile_HeadersClosedByDefault(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validTOML))
	require.NoError(t, err)
	p := c.Profiles["prom"]

	assert.False(t, p.AllowsAnyHeader())
	assert.False(t, p.HeaderAllowed("Authorization2"))
}

// The value rule answers for both sides of the wire: what the catalogue may
// carry and what a call may set are one question about one artifact.
func TestValidHeaderValue(t *testing.T) {
	t.Parallel()
	assert.True(t, ValidHeaderValue("web"))
	assert.False(t, ValidHeaderValue(""))
	assert.False(t, ValidHeaderValue("a\r\nX-Admin: 1"), "CR or LF is how one request becomes two")
	assert.False(t, ValidHeaderValue("a\x00b"))
	assert.False(t, ValidHeaderValue(strings.Repeat("x", MaxHeaderValueLen+1)))
}

func TestValidHeaderName(t *testing.T) {
	t.Parallel()
	assert.True(t, ValidHeaderName("Authorization2"))
	assert.False(t, ValidHeaderName("Bad Header"))
	assert.False(t, ValidHeaderName("Platform\n"))
	assert.False(t, ValidHeaderName(""))
}
