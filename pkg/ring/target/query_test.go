package target

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForceQuery(t *testing.T) {
	t.Parallel()
	p := &Profile{Query: []string{"environment=devel"}}
	cases := map[string]string{
		"/api/0/organizations/acme/issues/":                                          "/api/0/organizations/acme/issues/?environment=devel",
		"/api/0/organizations/acme/issues/?":                                         "/api/0/organizations/acme/issues/?environment=devel",
		"/api/0/organizations/acme/issues/?query=is:unresolved&limit=3":              "/api/0/organizations/acme/issues/?query=is:unresolved&limit=3&environment=devel",
		"/api/0/organizations/acme/issues/?environment=production&limit=3":           "/api/0/organizations/acme/issues/?limit=3&environment=devel",
		"/api/0/organizations/acme/issues/?environment=production&environment=local": "/api/0/organizations/acme/issues/?environment=devel",
		// The upstream decodes the key before it reads it, so the pin has
		// to recognise the encoded spelling too — or the contour is one
		// percent-escape away from being negotiable.
		"/api/0/organizations/acme/issues/?%65nvironment=production&limit=3": "/api/0/organizations/acme/issues/?limit=3&environment=devel",
	}
	for in, want := range cases {
		assert.Equal(t, want, p.ForceQuery(in), in)
	}
	assert.Equal(t, "/x?a=b", (&Profile{}).ForceQuery("/x?a=b"), "no Query is a no-op")
}

func TestValidateQueryPair(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"environment=devel", "a=", "k=v%20w"} {
		require.NoError(t, validateQueryPair(ok), ok)
	}
	for _, bad := range []string{"environment", "=devel", "a=b&c=d", "a=b c", "a?=b"} {
		assert.Error(t, validateQueryPair(bad), bad)
	}
}

func TestParseQueryRule(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]struct {
		name string
		rule queryRule
	}{
		"query":       {"query", queryRule{spec: "query"}},
		"match[]":     {"match[]", queryRule{spec: "match[]"}},
		"match%5B%5D": {"match[]", queryRule{spec: "match%5B%5D"}},
		"$top<=200":   {"$top", queryRule{spec: "$top<=200", op: "<=", bound: 200}},
		"limit <= 10": {"limit", queryRule{spec: "limit <= 10", op: "<=", bound: 10}},
		"step>=15s":   {"step", queryRule{spec: "step>=15s", op: ">=", bound: 15}},
		"step>=1h30m": {"step", queryRule{spec: "step>=1h30m", op: ">=", bound: 5400}},
	} {
		name, rule, err := parseQueryRule(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, want.name, name, raw)
		assert.Equal(t, want.rule, rule, raw)
	}
	for bad, want := range map[string]string{
		"":           "is not a parameter name",
		"a=b":        "is not a parameter name",
		"a&b":        "is not a parameter name",
		"a b":        "is not a parameter name",
		"<=100":      "is not a parameter name",
		"limit<100":  "a bound is written as name<=N or name>=N",
		"limit<=":    "must be an integer or a duration",
		"limit<=abc": "must be an integer or a duration",
		"limit<=1.5": "must be an integer or a duration",
		"step>=15x":  "must be an integer or a duration",
		"limit<=-1":  "must not be negative",
	} {
		_, _, err := parseQueryRule(bad)
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), want, bad)
	}
}

// CheckQuery on a profile without a list refuses only the denied names; with
// one it refuses what the list does not name and what its bounds do not
// accept, and lets a key Query pins through, because Query replaces it.
func TestCheckQuery(t *testing.T) {
	t.Parallel()

	open := &Profile{}
	for _, ok := range []string{"/x", "/x?", "/x?a=1&b=2", "/x?next_token=abc", "/x?a=1&&=2", "/x?query=up&"} {
		require.NoError(t, open.CheckQuery(ok), ok)
	}
	for _, bad := range []string{
		"/x?token=abc", "/x?a=1&PRIVATE_TOKEN=abc", "/x?%70rivate_token=abc", "/x?sudo=root", "/x?access_token=1",
		"/x?private_token",     // no value: still says who the caller is
		"/x?token[]=abc",       // Rack folds token[] into token
		"/x?a=1;sudo=root",     // Rack 2 splits on ; as well
		"/x?password=hunter22", // the wider family: password, secret, auth, bearer
		// A fragment never reaches the upstream, and neither would the
		// contour pin appended after it.
		"/x?a=1#frag",
		"/x#frag",
	} {
		require.Error(t, open.CheckQuery(bad), bad)
	}

	guarded := &Profile{
		AllowQueryParams: []string{"query", "match[]", "step>=15s", "limit<=100", "start", "end"},
		TimeParams:       []string{"start", "end"},
		Query:            []string{"environment=devel"},
	}
	require.Empty(t, guarded.validateQueryParams())

	for _, ok := range []string{
		"/x",
		"/x?query=up",
		"/x?query=up&start=now-1h&end=now&step=60s",
		"/x?step=15s", "/x?step=15", "/x?step=1h30m", "/x?step=15.5",
		"/x?limit=100", "/x?limit=0", "/x?limit=1e1",
		"/x?query",                       // a listed key without a value
		"/x?match[]=up&match%5B%5D=down", // the same key, written both ways
		"/x?environment=production",      // pinned by Query: dropped, not refused
		"/x?%65nvironment=production",
	} {
		require.NoError(t, guarded.CheckQuery(ok), ok)
	}
	for path, want := range map[string]string{
		"/x?query=up&pretty=true": `"pretty" is not allowed`,
		"/x?query=up;pretty=true": `"pretty" is not allowed`,
		"/x?%71uery=up&Query=up":  `"Query" is not allowed`,
		"/x?step=10s":             "step=10s is outside step>=15s",
		"/x?step=14":              "outside step>=15s",
		"/x?step=abc":             "needs a number or a duration like 15s",
		"/x?step":                 "needs a number or a duration",
		"/x?limit=101":            "limit=101 is outside limit<=100",
		"/x?limit=1&limit=101":    "limit=101 is outside",           // every repeat is read
		"/x?limit=-1":             "limit=-1 is outside limit<=100", // YouTrack's "no limit"
		"/x?limit=1h":             "limit=1h is outside limit<=100", // read as seconds, like any duration
		"/x?limit=NaN":            "needs a number",
		"/x?limit=%31%30%31":      "limit=101 is outside",
		"/x?query=up&token=abc":   `"token" is never accepted`,
	} {
		err := guarded.CheckQuery(path)
		require.Error(t, err, path)
		assert.Contains(t, err.Error(), want, path)
	}
}

// The denied names hold for the top-level keys of a JSON body too: GitLab
// reads the body into the same params as the query string. Nested keys are
// the upstream's data, and a body that is not JSON is the upstream's problem.
func TestCheckBody(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{
		"", "  ", "not json", "42", `"sudo"`, `[1, 2]`,
		`{"title":"x","source_branch":"a"}`,
		`{"params":{"token":"nested is data"}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"host.list"},{"jsonrpc":"2.0","id":2,"method":"host.get","params":{"hostname":"a"}}]`,
	} {
		require.NoError(t, CheckBody(ok), ok)
	}
	for body, want := range map[string]string{
		`{"title":"x","sudo":"root"}`:                                          `"sudo"`,
		`{"Private_Token":"glpat-x"}`:                                          `"Private_Token"`,
		`[{"jsonrpc":"2.0","id":1,"method":"host.list"},{"access_token":"x"}]`: `"access_token"`,
	} {
		err := CheckBody(body)
		require.Error(t, err, body)
		assert.Contains(t, err.Error(), want, body)
	}
}

// The list is validated with the rest of the profile: a denied name, a name
// Query pins, a bound on a time parameter and a time parameter left out are
// all mistakes the author has to hear about at load, not from the model.
func TestParse_AllowQueryParams(t *testing.T) {
	t.Parallel()
	with := func(fields string) string {
		return replaceLine(validTOML, "AllowPaths", `AllowPaths   = ["^/api/v1/query$"]`+"\n"+fields)
	}

	c, err := Parse([]byte(with(`AllowQueryParams = ["query", "step>=15s", "start", "end", "time"]
TimeParams = ["start", "end", "time"]
TimeFormat = "unix"
Query = ["environment=devel"]`)))
	require.NoError(t, err)
	p, _ := c.Profile("prom")
	assert.True(t, p.QueryGuarded())
	require.NoError(t, p.CheckQuery("/api/v1/query?query=up&time=now-1h&environment=x"))
	require.Error(t, p.CheckQuery("/api/v1/query?query=up&timeout=1m"))

	// A time parameter the profile pins itself is not one the list has to
	// carry: the call never writes it.
	_, err = Parse([]byte(with(`AllowQueryParams = ["query"]
TimeParams = ["start"]
TimeFormat = "unix"
Query = ["start=now-1h"]`)))
	require.NoError(t, err)

	for name, tc := range map[string]struct{ fields, want string }{
		"denied name":        {`AllowQueryParams = ["query", "private_token"]`, "may never be set by a call"},
		"denied by case":     {`AllowQueryParams = ["Sudo"]`, "may never be set by a call"},
		"pinned by Query":    {`AllowQueryParams = ["environment"]` + "\nQuery = [\"environment=devel\"]", "pinned by Query"},
		"bound on time":      {`AllowQueryParams = ["start<=5", "end"]` + "\nTimeParams = [\"start\", \"end\"]\nTimeFormat = \"unix\"", "a time parameter takes now-1h"},
		"time param missing": {`AllowQueryParams = ["query"]` + "\nTimeParams = [\"time\"]\nTimeFormat = \"unix\"", `TimeParams "time" is not in AllowQueryParams`},
		"twice":              {`AllowQueryParams = ["limit", "limit<=5"]`, `lists "limit" twice`},
		"twice by encoding":  {`AllowQueryParams = ["match[]", "match%5B%5D"]`, `lists "match[]" twice`},
		"bad entry":          {`AllowQueryParams = ["limit<5"]`, `AllowQueryParams "limit<5": a bound is written as`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(with(tc.fields)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
