package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/auth"
	"github.com/vmkteam/mcpkit/auth/authtest"
	"github.com/vmkteam/mcpkit/doc"
)

func appWith(t *testing.T, mutate func(c *Config)) *App {
	t.Helper()
	var cfg Config
	cfg.Server.Port = 8075
	cfg.Server.BaseURL = "https://ringsrv.example.com"
	mutate(&cfg)
	return New("ringsrv", embedlog.Logger{}, cfg)
}

// mcpApp assembles /mcp the way Run does: the catalogue, the docs and every init
// the endpoint reads. One copy, because a test that skips an init the real boot
// performs passes against an endpoint nobody runs.
func mcpApp(t *testing.T, mutate func(c *Config)) *App {
	t.Helper()

	a := appWith(t, mutate)

	var err error
	a.targets = testCatalog(t)
	a.docs, err = doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)

	a.initAPIKeys(t.Context())
	a.initRateLimit(t.Context())
	a.initUpstream()
	a.sessions = ring.NewSessions(0, nil)
	a.registerHandlers() // the CORS skipper is part of what /mcp answers with
	a.registerMCPHandlers()

	return a
}

// The ladder decides who gets in. An error here does not fail loudly — it
// admits the wrong caller — so each rung is checked explicitly.
func TestMCPAuthLadder(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t, "ringsrv").URL
	key := auth.Key{UserID: "tester", KeyHash: "abc", Groups: []string{"ringsrv-users"}}

	cases := map[string]struct {
		mutate func(c *Config)
		want   string
	}{
		"required oidc wins over everything": {
			func(c *Config) {
				c.OIDC.Issuer, c.OIDC.ClientID, c.OIDC.Required = issuer, "ringsrv", true
				c.APIKeys = []auth.Key{key}
			},
			authLabelOIDCRequired,
		},
		"api keys come before optional oidc": {
			func(c *Config) {
				c.OIDC.Issuer, c.OIDC.ClientID = issuer, "ringsrv"
				c.APIKeys = []auth.Key{key}
			},
			authLabelAPIKeys,
		},
		"optional oidc when there are no keys": {
			func(c *Config) { c.OIDC.Issuer, c.OIDC.ClientID = issuer, "ringsrv" },
			authLabelOIDCOptional,
		},
		"nothing configured is dev-only no-auth": {
			func(_ *Config) {},
			authLabelDisabled,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := appWith(t, tc.mutate)
			a.initUpstream()
			a.initAPIKeys(context.Background())
			require.NoError(t, a.initOIDC(context.Background()))

			assert.Equal(t, tc.want, a.mcpAuthLabel())

			// The label is what the startup log says; the middleware is what
			// actually runs. They must not drift apart.
			inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			wrapped := a.mcpAuthMiddleware(inner)
			require.NotNil(t, wrapped)

			rr := httptest.NewRecorder()
			wrapped.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", nil))
			if tc.want == authLabelDisabled {
				assert.Equal(t, http.StatusOK, rr.Code, "no auth configured: the call goes through")
			} else {
				assert.Equal(t, http.StatusUnauthorized, rr.Code, "a call without a credential is refused")
			}
		})
	}
}

// The canonical URI mismatch is the failure this stack hits most often, and it
// does not crash anything: the service serves and refuses every valid token.
func TestInitOIDC_AudienceMustMatchBaseURL(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t, "ringsrv").URL

	a := appWith(t, func(c *Config) {
		c.OIDC.Issuer, c.OIDC.ClientID = issuer, "ringsrv"
		c.OIDC.Audience = "https://somewhere.else"
	})
	err := a.initOIDC(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must equal Server.BaseURL")

	a = appWith(t, func(c *Config) {
		c.OIDC.Issuer, c.OIDC.ClientID = issuer, "ringsrv"
		c.OIDC.Audience = "https://ringsrv.example.com"
	})
	assert.NoError(t, a.initOIDC(context.Background()))
}

func TestInitOIDC_RequiredWithoutIssuer(t *testing.T) {
	t.Parallel()
	a := appWith(t, func(c *Config) { c.OIDC.Required = true })
	require.Error(t, a.initOIDC(context.Background()))
}

// With Required=false a transient IdP outage must not take the whole service
// down: it keeps booting on api-keys.
func TestInitOIDC_OptionalSurvivesDiscoveryFailure(t *testing.T) {
	t.Parallel()
	a := appWith(t, func(c *Config) {
		c.OIDC.Issuer, c.OIDC.ClientID = "http://127.0.0.1:1/nope", "ringsrv"
	})
	require.NoError(t, a.initOIDC(context.Background()))
	assert.Nil(t, a.verifier)
}

// The endpoints an orchestrator and an MCP client depend on: readiness, and
// the metadata that tells a client where to authenticate (RFC 9728).
// The subtests here run in order on purpose: the catalogue appears between the
// first two, which is the whole point of the readiness check.
func TestHandlers_StatusAndMetadata(t *testing.T) { //nolint:tparallel // subtests share state by design
	t.Parallel()
	issuer := authtest.NewIssuer(t, "ringsrv").URL
	a := appWith(t, func(c *Config) {
		c.OIDC.Issuer, c.OIDC.ClientID = issuer, "ringsrv"
		c.Server.IsDevel = true
	})
	a.initAPIKeys(context.Background())
	a.registerHandlers()
	a.registerDebugHandlers()

	t.Run("status is unavailable until the catalogue is loaded", func(t *testing.T) {
		rr := httptest.NewRecorder()
		a.echo.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})

	a.targets = testCatalog(t)

	t.Run("and ready once it is", func(t *testing.T) {
		rr := httptest.NewRecorder()
		a.echo.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("protected resource metadata points at the issuer", func(t *testing.T) {
		// registerMCPHandlers wires the limiter, the client and the sessions;
		// all three have to exist by then, as they do in Run.
		a.initRateLimit(context.Background())
		a.initUpstream()
		a.sessions = ring.NewSessions(0, nil)
		a.docs, _ = doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
		a.registerMCPHandlers()

		rr := httptest.NewRecorder()
		a.echo.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
		require.Equal(t, http.StatusOK, rr.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, a.cfg.Server.BaseURL+"/mcp", body["resource"], "the canonical URI, byte for byte")
		assert.Equal(t, []any{issuer}, body["authorization_servers"])
	})

	// Without an issuer the document has nothing to point at. Publishing it
	// anyway says "this resource is protected" and gives no way to
	// authenticate: mcpurl found it, started OAuth and died on a missing
	// client_id against a server that wanted no auth at all.
	t.Run("no issuer means no metadata at all", func(t *testing.T) {
		open := &App{Logger: embedlog.Logger{}, echo: echo.New(), targets: a.targets}
		open.echo.GET("/.well-known/oauth-protected-resource", open.handleOAuthProtectedResource)

		rr := httptest.NewRecorder()
		open.echo.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})
}

// testCatalog is the catalogue these tests serve. It is a fixture and not a
// contour file on purpose: what is under test here is the plumbing around a
// catalogue, so depending on the one a deployment ships buys nothing and costs
// a skipped test in any checkout that does not carry it.
func testCatalog(t *testing.T) *target.Catalog {
	t.Helper()
	c, err := target.Load(filepath.Join("testdata", "targets.toml"))
	require.NoError(t, err)
	return c
}
