package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/rpc"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vmkteam/appkit"
	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/auth"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/zenrpc/v2"
)

// runHTTPServer starts the http listener. The timeouts cap slowloris-style
// attacks: a client that stalls before completing a request hits
// ReadHeaderTimeout or ReadTimeout instead of holding a goroutine. WriteTimeout
// leaves room for a slow upstream, which Limits.UpstreamTimeout caps separately.
func (a *App) runHTTPServer(ctx context.Context, host string, port int) error {
	listenAddress := fmt.Sprintf("%s:%d", host, port)
	addr := "http://" + listenAddress
	// The smdbox line only in devel, because that is the only place the page is
	// registered — printing a link that answers 404 is worse than printing none.
	fields := []any{"url", addr}
	if a.cfg.Server.IsDevel {
		fields = append(fields, "smdbox", addr+"/mcp/doc")
	}
	a.Print(ctx, "starting http listener", fields...)

	a.echo.Server.ReadHeaderTimeout = 5 * time.Second
	a.echo.Server.ReadTimeout = 30 * time.Second
	a.echo.Server.WriteTimeout = 120 * time.Second
	a.echo.Server.IdleTimeout = 120 * time.Second

	return a.echo.Start(listenAddress)
}

// registerMetrics adds HTTP metrics middleware and the /metrics endpoint to echo.
func (a *App) registerMetrics() {
	a.echo.Use(appkit.HTTPMetrics(appkit.DefaultServerName))
	a.echo.Any("/metrics", echo.WrapHandler(promhttp.Handler()))
}

// registerHandlers registers the echo handlers.
//
// CORS skips the MCP surface on purpose: mcpkit validates Origin itself, and in
// production, with an empty Options.AllowedOrigins, refuses any request that
// carries one. Letting echo answer "any origin is fine" for the same path would
// be two settings saying the opposite, and the 403 would be debugged against the
// one that allows it.
//
// The endpoint and everything under it, rather than a list: the doc box already
// adds two paths below /mcp, and a route added later must not become the one
// echo hands a permission mcpkit refuses.
func (a *App) registerHandlers() {
	a.echo.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		Skipper:      func(c echo.Context) bool { return c.Path() == "/mcp" || strings.HasPrefix(c.Path(), "/mcp/") },
		AllowOrigins: []string{"*"},
		AllowMethods: []string{echo.GET, echo.PUT, echo.POST, echo.DELETE},
		AllowHeaders: []string{"Authorization", "Origin", "X-Requested-With", "Content-Type", "Accept"},
	}))
}

// registerDebugHandlers adds /debug/pprof handlers into a.echo instance.
func (a *App) registerDebugHandlers() {
	dbg := a.echo.Group("/debug")

	// add pprof integration
	dbg.Any("/pprof/*", appkit.PprofHandler)

	// /status is the readiness probe: once the catalogue is loaded the instance
	// can serve, and an upstream being down is a per-call error rather than a
	// reason to pull it out of rotation.
	a.echo.GET("/status", func(c echo.Context) error {
		if a.targets == nil {
			return c.String(http.StatusServiceUnavailable, "catalog not loaded")
		}
		return c.String(http.StatusOK, "OK")
	})

	// show all routes in devel mode
	if a.cfg.Server.IsDevel {
		a.echo.GET("/", appkit.RenderRoutes(a.appName, a.echo))
	}
}

// develOrigins lists the origins a browser could be showing the doc box under:
// BaseURL when the devel instance sits behind a proxy, and the loopback names
// when it is started on somebody's machine. Only these are handed to mcpkit,
// and only in devel — see registerMCPHandlers.
func (a *App) develOrigins() []string {
	port := strconv.Itoa(a.cfg.Server.Port)
	origins := []string{
		"http://" + net.JoinHostPort("localhost", port),
		"http://" + net.JoinHostPort("127.0.0.1", port),
	}
	if base := strings.TrimRight(a.cfg.Server.BaseURL, "/"); base != "" {
		origins = append(origins, base)
	}
	return origins
}

func (a *App) registerMCPHandlers() {
	zsrv := rpc.NewMCP(rpc.MCPDeps{
		Info:          mcp.ServerInfo{Name: a.appName, Version: appkit.Version()},
		Docs:          a.docs,
		Targets:       a.targets,
		Upstream:      a.upstream,
		Repos:         a.repos,
		Graph:         a.graph,
		DB:            a.db,
		MaxBytes:      a.cfg.Limits.MaxBytes,
		MaxConcurrent: a.cfg.Limits.MaxConcurrent,
		JQTimeout:     a.cfg.Limits.JQTimeout,
		Sessions:      a.sessions,
		Logger:        a.Logger,
		IsDevel:       a.cfg.Server.IsDevel,
	})
	// LogHandshake is the only place that answers what a client actually speaks:
	// neither Claude Code nor Claude Desktop documents its revision, and no
	// header carries the era. One line per connection is affordable here — the
	// clients are a handful of engineers and a stdio bridge. It also records the
	// Origin, which is what turns "no client sends one" into something observed.
	//
	// AllowedHosts stays empty, switching the Host check off: nginx answers that
	// question first through server_name, and a second copy of the host list in
	// a config the proxy does not read goes stale. If ringsrv is ever put in
	// front of a browser, revisit this together with AllowedOrigins.
	opts := mcpkit.Options{LogHandshake: true}
	if a.cfg.Server.IsDevel {
		// SMD Box is the one browser client we have, and a browser attaches an
		// Origin to its POST even when the page came from this very host. With
		// AllowedOrigins empty mcpkit refuses anything carrying one, so the doc
		// box could read the schema and never call a method. Listing our own
		// origins buys that back where it is useful and leaves the rebinding
		// guard untouched everywhere else.
		opts.AllowedOrigins = a.develOrigins()
	}
	srv := mcpkit.NewServerWithOptions(zsrv, a.Logger, opts)

	// Auth wraps rate-limit so the limiter sees Principal.UserID; anonymous dev
	// callers collapse to a single bucket on purpose.
	a.echo.Any("/mcp", echo.WrapHandler(a.mcpAuthMiddleware(a.rateLimit.Middleware(srv, a.Logger))))
	// RFC 9728 protected-resource metadata: compliant clients follow it to the
	// IdP's openid-configuration and run OAuth there directly.
	a.echo.GET("/.well-known/oauth-protected-resource", a.handleOAuthProtectedResource)

	// The MCP surface is JSON-RPC underneath, so the same server describes itself
	// through SMD and SMD Box renders it. The page lives at /mcp/doc because the
	// box derives the schema URL from its own address: it strips the trailing
	// "doc" and asks the parent path with ?smd. Hence the schema hanging off the
	// slashed path while the callable endpoint stays /mcp — a POST goes there,
	// which the box learns from Options.TargetURL in the schema itself.
	//
	// Devel only: in production the tool list is something an authenticated
	// client reads through tools/list, not a page anybody can open.
	if a.cfg.Server.IsDevel {
		a.echo.Any("/mcp/doc", appkit.EchoHandlerFunc(zenrpc.SMDBoxHandler))
		// Built once: SMD() walks every service and method and allocates the
		// whole map each call, and the schema cannot change after boot.
		schema := zsrv.SMD()
		a.echo.GET("/mcp/", func(c echo.Context) error {
			return c.JSON(http.StatusOK, schema)
		})
	}
}

// Labels for the auth ladder, shared by the middleware and the startup log so
// the two cannot say different things.
const (
	authLabelOIDCRequired = "OIDC required"
	authLabelAPIKeys      = "api-keys"
	authLabelOIDCOptional = "OIDC optional"
	authLabelDisabled     = "disabled"
)

// mcpAuthMiddleware picks the auth backend for /mcp. Order:
//  1. OIDC.Required → strict JWT.
//  2. api-keys configured → api-key store.
//  3. OIDC optionally configured → JWT.
//  4. nothing → no-op (dev).
//
// mcpAuthLabel mirrors the same ladder for startup logging — keep them in sync.
func (a *App) mcpAuthMiddleware(next http.Handler) http.Handler {
	if a.cfg.OIDC.Required {
		return a.verifier.Middleware(next, a.Logger)
	}
	if !a.authStore.Empty() {
		return a.authStore.Middleware(next, a.Logger)
	}
	if a.verifier != nil {
		return a.verifier.Middleware(next, a.Logger)
	}
	return next
}

func (a *App) mcpAuthLabel() string {
	switch {
	case a.cfg.OIDC.Required:
		return authLabelOIDCRequired
	case !a.authStore.Empty():
		return authLabelAPIKeys
	case a.verifier != nil:
		return authLabelOIDCOptional
	default:
		return authLabelDisabled
	}
}

// publicBaseURL returns the externally-visible URL of this service with any
// trailing slash trimmed. Falls back to scheme+host of the current request
// when BaseURL is unset (dev mode behind a reverse proxy can vary per call).
func (a *App) publicBaseURL(c echo.Context) string {
	base := a.cfg.Server.BaseURL
	if base == "" {
		base = c.Scheme() + "://" + c.Request().Host
	}
	return strings.TrimRight(base, "/")
}

// handleOAuthProtectedResource serves RFC 9728 — clients hit this after a 401,
// then drive OAuth against the IdP.
//
// auth.ProtectedResource answers 404 without an issuer, which is deliberate:
// publishing the document anyway says "this resource is protected" while giving
// no way to authenticate, and mcpurl then started OAuth against a server that
// wanted none. It stays a method because Resource is per request: without
// BaseURL the canonical URI falls back to the Host the client used.
func (a *App) handleOAuthProtectedResource(c echo.Context) error {
	auth.ProtectedResource{
		Resource: a.publicBaseURL(c) + "/mcp",
		Issuer:   a.cfg.OIDC.Issuer,
		Scopes:   a.cfg.OIDC.Scopes,
	}.Handler().ServeHTTP(c.Response(), c.Request())
	return nil
}
