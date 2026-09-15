package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SMD Box is the only browser client this service has, and everything holding
// it together is a URL convention nothing else checks: the box derives the
// schema address from its own ("/mcp/doc" minus "doc", plus ?smd), the schema
// names the endpoint to POST to, and the pages exist in devel alone. Each of
// those fails silently — a page that loads and stays empty.

// docApp assembles /mcp the way Run does, in the mode asked for.
func docApp(t *testing.T, devel bool) *App {
	t.Helper()
	return mcpApp(t, func(c *Config) { c.Server.IsDevel = devel })
}

func serve(a *App, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	a.echo.ServeHTTP(rr, r)
	return rr
}

func TestDocBox(t *testing.T) {
	t.Parallel()

	t.Run("devel serves the box and the schema it asks for", func(t *testing.T) {
		t.Parallel()
		a := docApp(t, true)

		page := serve(a, httptest.NewRequest(http.MethodGet, "/mcp/doc", nil))
		require.Equal(t, http.StatusOK, page.Code)
		assert.Contains(t, page.Body.String(), "smdbox", "the page must load the box, not just say SMD")

		// The address the box computes for itself, spelled out: it strips the
		// trailing "doc/" and asks the parent with ?smd.
		schema := serve(a, httptest.NewRequest(http.MethodGet, "/mcp/?smd", nil))
		require.Equal(t, http.StatusOK, schema.Code)

		var got struct {
			Target   string                     `json:"target"`
			Services map[string]json.RawMessage `json:"services"`
		}
		require.NoError(t, json.Unmarshal(schema.Body.Bytes(), &got))

		// Where the box sends the calls it builds. "/" — the zenrpc default —
		// would point them at the root and fail one request at a time.
		assert.Equal(t, "/mcp", got.Target)

		var namespaces []string
		for method := range got.Services {
			if ns, _, ok := strings.Cut(method, "."); ok {
				namespaces = append(namespaces, ns)
			}
		}
		assert.Subset(t, namespaces, []string{"tools", "resources", "prompts"},
			"the schema must describe the namespaces the endpoint actually answers")
	})

	t.Run("production serves neither", func(t *testing.T) {
		t.Parallel()
		a := docApp(t, false)

		assert.Equal(t, http.StatusNotFound,
			serve(a, httptest.NewRequest(http.MethodGet, "/mcp/doc", nil)).Code)
		assert.Equal(t, http.StatusNotFound,
			serve(a, httptest.NewRequest(http.MethodGet, "/mcp/?smd", nil)).Code,
			"the tool schema outside devel is something tools/list answers, behind auth")
	})
}

// The Origin check is what the doc box runs into on its first call, and the
// symptom — 403 on a page served by this very host — reads like a bug in the
// box rather than a deliberate setting.
func TestMCPOriginPolicy(t *testing.T) {
	t.Parallel()

	ping := func(origin string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		return r
	}

	// appWith sets BaseURL, which is the origin a devel instance behind a proxy
	// is reached under.
	const base = "https://ringsrv.example.com"

	t.Run("devel accepts a call from its own pages", func(t *testing.T) {
		t.Parallel()
		a := docApp(t, true)
		assert.Contains(t, a.develOrigins(), base)
		assert.Equal(t, http.StatusOK, serve(a, ping(base)).Code)
	})

	t.Run("devel still refuses a stranger", func(t *testing.T) {
		t.Parallel()
		a := docApp(t, true)
		assert.Equal(t, http.StatusForbidden, serve(a, ping("https://evil.example.com")).Code)
	})

	t.Run("production refuses every browser", func(t *testing.T) {
		t.Parallel()
		a := docApp(t, false)
		assert.Equal(t, http.StatusForbidden, serve(a, ping(base)).Code)
	})
}
