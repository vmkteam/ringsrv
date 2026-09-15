package upstream

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The host comes from the profile and only from there. These are the
// shapes that would send a request somewhere else if the path were trusted.
func TestBuildURL(t *testing.T) {
	t.Parallel()
	const base = "https://prom.example.com"

	ok := map[string]string{
		"/api/v1/query":                    "https://prom.example.com/api/v1/query",
		"/api/v1/query?query=up":           "https://prom.example.com/api/v1/query?query=up",
		`/api/v1/query?q=up{job="apisrv"}`: `https://prom.example.com/api/v1/query?q=up{job="apisrv"}`,
		"/api/v1/query?a=1&b=2&a=3":        "https://prom.example.com/api/v1/query?a=1&b=2&a=3",
		// A space would end the request line; encoded it means the same.
		"/api/v1/query?query=up offset 5m": "https://prom.example.com/api/v1/query?query=up%20offset%205m",
	}
	for path, want := range ok {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			got, err := buildURL(base, path)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}

	bad := []string{
		"https://evil.example.com/api/v1/query",
		"//evil.example.com/api/v1/query",
		"/api/v1/../../admin",
		"api/v1/query",
		"",
	}
	for _, path := range bad {
		t.Run("rejected: "+path, func(t *testing.T) {
			t.Parallel()
			_, err := buildURL(base, path)
			assert.Error(t, err)
		})
	}
}

func TestBuildURL_BadBase(t *testing.T) {
	t.Parallel()
	_, err := buildURL("not-a-url", "/api/v1/query")
	require.Error(t, err)
}

// The upstream decodes before routing, so the decoded form has to be as plain
// as the written one. The written form is what travels — some APIs need the
// escapes — which is why CleanPath returns it unchanged.
func TestCleanPath_DecodedForm(t *testing.T) {
	t.Parallel()
	bad := []string{
		"/a/%2e%2e/b",
		"/a/%2E%2E/b",
		"/a%2f..%2fb",
		"/a/%00",
		"/a/%5c..",
		"/a//b",
		"/a/./b",
		"/a/.",
		"/a/%zz", // not a valid escape: undefined on the other side
		"/a\tb",
	}
	for _, p := range bad {
		_, ok := CleanPath(p)
		assert.False(t, ok, p)
	}
	ok := map[string]string{
		"/a/b%2Fc/raw?ref=x": "/a/b%2Fc/raw",
		"/a/b%20c":           "/a/b%20c",
		"/a/b#frag":          "/a/b",
	}
	for p, want := range ok {
		got, isOK := CleanPath(p)
		require.True(t, isOK, p)
		assert.Equal(t, want, got)
	}
}
