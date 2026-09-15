package target

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedNow is a round timestamp so the expected values below are readable:
// 2026-09-01T12:00:00Z.
var fixedNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// timeProfile builds a profile with just the fields ExpandPath looks at.
func timeProfile(format string, params ...string) *Profile {
	return &Profile{name: "test", TimeParams: params, TimeFormat: format}
}

func TestExpandPath_Formats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		format string
		in     string
		want   string
	}{
		// Prometheus wants seconds, Loki nanoseconds, Sentry RFC3339 — the
		// whole point of the feature is that the model does not have to know.
		{"unix", TimeFormatUnix, "/api/v1/query?start=now-1h", "/api/v1/query?start=1788260400"},
		{"unix_ms", TimeFormatUnixMs, "/api/v1/query?start=now-1h", "/api/v1/query?start=1788260400000"},
		{"unix_ns", TimeFormatUnixNs, "/api/v1/query?start=now-1h", "/api/v1/query?start=1788260400000000000"},
		{"rfc3339", TimeFormatRFC3339, "/api/v1/query?start=now-1h", "/api/v1/query?start=2026-09-01T11:00:00Z"},
		{"now without offset", TimeFormatUnix, "/api/v1/query?start=now", "/api/v1/query?start=1788264000"},
		{"days", TimeFormatUnix, "/api/v1/query?start=now-7d", "/api/v1/query?start=1787659200"},
		{"weeks", TimeFormatUnix, "/api/v1/query?start=now-1w", "/api/v1/query?start=1787659200"},
		{"minutes", TimeFormatUnix, "/api/v1/query?start=now-30m", "/api/v1/query?start=1788262200"},
		{"compound", TimeFormatUnix, "/api/v1/query?start=now-1h30m", "/api/v1/query?start=1788258600"},
		// The key is matched decoded, as the allowlist matched it: what passed
		// as start is expanded as start, and the spelling stays the caller's.
		{"encoded key", TimeFormatUnix, "/api/v1/query?%73tart=now-1h", "/api/v1/query?%73tart=1788260400"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := timeProfile(tc.format, "start", "end").ExpandPath(tc.in, fixedNow)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// One grammar for every duration the model writes: after now- and as a value
// under a bound.
func TestParseDuration(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{
		"15s":   15 * time.Second,
		"500ms": 500 * time.Millisecond,
		"5m":    5 * time.Minute,
		"1h30m": 90 * time.Minute,
		"2d":    48 * time.Hour,
		"1w":    7 * 24 * time.Hour,
	} {
		d, ok := parseDuration(in)
		require.True(t, ok, in)
		assert.Equal(t, want, d, in)
	}
	// The last two do not fit a time.Duration: wrapped around they would
	// read as negative, and a negative duration passes any <= bound.
	for _, bad := range []string{"", "15", "s", "15x", "1h30", "-5m", "1.5m", "1y", "9223372036854775807w", "9223372036854775807s1h"} {
		_, ok := parseDuration(bad)
		assert.False(t, ok, bad)
	}
}

// Anything that is not relative time is left alone: the parameter may hold an
// absolute timestamp, and mangling it is worse than doing nothing.
func TestExpandPath_LeavesEverythingElse(t *testing.T) {
	t.Parallel()
	p := timeProfile(TimeFormatUnix, "start", "end")

	untouched := []string{
		"/api/v1/query?start=1725100000",
		"/api/v1/query?start=2026-09-01T00:00:00Z",
		"/api/v1/query?start=now-",
		"/api/v1/query?start=now-1x",
		"/api/v1/query?start=nowhere",
		"/api/v1/query?start=now%2B1h",
		`/api/v1/query?query=up{job="apisrv"}`, // not a time parameter at all
		"/api/v1/query",
	}
	for _, in := range untouched {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := p.ExpandPath(in, fixedNow)
			require.NoError(t, err)
			assert.Equal(t, in, got)
		})
	}
}

// PromQL lives in the same query string: re-encoding would sort the keys and
// escape the braces, so the rewrite has to be surgical.
func TestExpandPath_PreservesOrderAndOtherParams(t *testing.T) {
	t.Parallel()
	p := timeProfile(TimeFormatUnix, "start", "end")

	got, err := p.ExpandPath(`/api/v1/query?query=up{job="apisrv"}&start=now-1h&end=now&step=60s`, fixedNow)
	require.NoError(t, err)
	assert.Equal(t, `/api/v1/query?query=up{job="apisrv"}&start=1788260400&end=1788264000&step=60s`, got)
}

func TestExpandPath_NoTimeParamsIsNoop(t *testing.T) {
	t.Parallel()
	p := &Profile{name: "test"} // no TimeParams at all
	got, err := p.ExpandPath("/api/v1/query?start=now-1h", fixedNow)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/query?start=now-1h", got)
}

// bodyProfile builds a profile with just the fields ExpandBody looks at,
// compiled the way Parse compiles them.
func bodyProfile(format string, paths ...string) *Profile {
	p := &Profile{name: "test", TimeBodyParams: paths, TimeFormat: format}
	p.bodyTimeParams, _ = compileBodyTimeParams(paths, format)
	return p
}

// The window of a JSON-RPC range query travels in the body as integers;
// the model writes now-1h there and the server does the arithmetic.
func TestExpandBody(t *testing.T) {
	t.Parallel()
	p := bodyProfile(TimeFormatUnix, "params.start", "params.end")

	cases := []struct{ name, in, want string }{
		{"integers for unix",
			`{"jsonrpc":"2.0","id":1,"method":"metric.queryRange","params":{"query":"up","start":"now-1h","end":"now","step":"5m"}}`,
			`{"id":1,"jsonrpc":"2.0","method":"metric.queryRange","params":{"end":1788264000,"query":"up","start":1788260400,"step":"5m"}}`},
		{"a batch gets every element",
			`[{"id":1,"method":"metric.queryRange","params":{"start":"now-1h"}},{"id":2,"method":"metric.queryRange","params":{"end":"now"}}]`,
			`[{"id":1,"method":"metric.queryRange","params":{"start":1788260400}},{"id":2,"method":"metric.queryRange","params":{"end":1788264000}}]`},
		{"big ids and PromQL comparisons survive the round trip",
			`{"id":9007199254740993,"method":"metric.queryRange","params":{"query":"up < 1 and x > 0","start":"now-7d"}}`,
			`{"id":9007199254740993,"method":"metric.queryRange","params":{"query":"up < 1 and x > 0","start":1787659200}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := p.ExpandBody(tc.in, fixedNow)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("rfc3339 stays a string", func(t *testing.T) {
		t.Parallel()
		got, err := bodyProfile(TimeFormatRFC3339, "from").ExpandBody(`{"from":"now-1h"}`, fixedNow)
		require.NoError(t, err)
		want := `{"from":"2026-09-01T11:00:00Z"}`
		assert.Equal(t, want, got)
	})
}

// One upstream, two windows: topsrv reads the metric range as unix integers
// and the weblog filter as RFC3339 strings, both in the body of the same
// path. A profile has one TimeFormat, so the entry that disagrees says so.
func TestExpandBody_FormatPerParam(t *testing.T) {
	t.Parallel()
	p := bodyProfile(TimeFormatUnix, "params.start", "params.end",
		"params.filter.from:rfc3339", "params.filter.to:rfc3339")

	got, err := p.ExpandBody(`{"id":1,"method":"metric.queryRange","params":{"start":"now-1h","end":"now"}}`, fixedNow)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":1,"method":"metric.queryRange","params":{"end":1788264000,"start":1788260400}}`, got)

	got, err = p.ExpandBody(`{"id":1,"method":"weblog.top","params":{"groupBy":"asn","filter":{"from":"now-6h","to":"now"}}}`, fixedNow)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":1,"method":"weblog.top","params":{"filter":{"from":"2026-09-01T06:00:00Z","to":"2026-09-01T12:00:00Z"},"groupBy":"asn"}}`, got)

	// A profile whose every entry names a format needs no TimeFormat of its
	// own — the field exists for the parameters that stay silent.
	only := bodyProfile("", "params.filter.from:rfc3339")
	got, err = only.ExpandBody(`{"params":{"filter":{"from":"now-1d"}}}`, fixedNow)
	require.NoError(t, err)
	assert.JSONEq(t, `{"params":{"filter":{"from":"2026-08-31T12:00:00Z"}}}`, got)
}

// What compiles and what does not. A format nobody implements and a path
// listed twice are caught at load, where the author is, and not on the call
// that silently sent the window in the wrong shape.
func TestCompileBodyTimeParams(t *testing.T) {
	t.Parallel()

	params, errs := compileBodyTimeParams([]string{"params.start", "params.filter.from:rfc3339"}, TimeFormatUnix)
	require.Empty(t, errs)
	assert.Equal(t, []bodyTimeParam{
		{path: []string{"params", "start"}, format: TimeFormatUnix},
		{path: []string{"params", "filter", "from"}, format: TimeFormatRFC3339},
	}, params)

	broken := map[string]string{
		"params.start:iso8601":               "want one of",
		"params.start:":                      "want one of",
		"params.filter.from:rfc3339:rfc3339": "want one of", // everything after the first colon is the format
		"params..start":                      "dotted path",
	}
	for entry, want := range broken {
		_, got := compileBodyTimeParams([]string{entry}, TimeFormatUnix)
		require.Len(t, got, 1, entry)
		assert.Contains(t, got[0].Error(), want, entry)
	}

	// The same path in two formats: the last one would win silently.
	_, errs = compileBodyTimeParams([]string{"params.start", "params.start:rfc3339"}, TimeFormatUnix)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), `lists "params.start" twice`)

	// Without a default and without a suffix there is nothing to expand
	// into; validateTimeFormat is what refuses it, and it does so first.
	_, errs = compileBodyTimeParams([]string{"params.start"}, "")
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "want one of")
}

// Everything that is not relative time at a named path goes through byte for
// byte — an absolute timestamp, a missing path, a body that is not JSON:
// re-encoding an untouched request would only reorder what the caller wrote.
func TestExpandBody_LeavesEverythingElse(t *testing.T) {
	t.Parallel()
	p := bodyProfile(TimeFormatUnix, "params.start", "params.end")

	untouched := []string{
		`{"method":"metric.queryRange","params":{"start":1725100000,"end":"2026-09-01T00:00:00Z"}}`,
		`{"method":"metric.queryRange","params":{"start":"nowhere","end":"now-1x"}}`,
		`{"method":"host.list"}`,
		`{"method":"x","params":"start=now-1h"}`,
		`{"method":"x","params":{"start":{"nested":"now-1h"}}}`,
		`{"z":1, "a":2}`,
		`not json at all`,
		`{"method":"x"} trailing`,
		`"now-1h"`,
		``,
		`  `,
	}
	for _, body := range untouched {
		got, err := p.ExpandBody(body, fixedNow)
		require.NoError(t, err, body)
		assert.Equal(t, body, got)
	}

	in := `{"params":{"start":"now-1h"}}`
	got, err := bodyProfile("").ExpandBody(in, fixedNow)
	require.NoError(t, err)
	assert.Equal(t, in, got, "no paths, no expansion")
}
