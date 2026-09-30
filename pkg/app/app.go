package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
	"github.com/vmkteam/ringsrv/pkg/rpc"

	"github.com/labstack/echo/v4"
	"github.com/vmkteam/appkit"
	"github.com/vmkteam/cron"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/auth"
	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/ratelimit"
)

func protectedResourceMetadataURL(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/.well-known/oauth-protected-resource"
}

// Config is the instance config — cfg/{local,prod}.toml. The target catalogue
// lives in its own file, reachable through Catalog.Path: different owners, and
// it changes between releases.
type Config struct {
	Server struct {
		Host    string
		Port    int
		IsDevel bool
		// BaseURL is the canonical resource URI (RFC 8707). It must match
		// OIDC.Audience byte for byte, or every valid token gets a 401 —
		// checked in initOIDC, where the failure is fatal anyway.
		BaseURL string
	}
	Sentry struct {
		Environment string
		DSN         string
	}
	// OIDC verifies bearer tokens on /mcp; the verifier needs only discovery and
	// JWKS. An empty Issuer disables JWT validation entirely — dev only.
	OIDC struct {
		Issuer        string
		ClientID      string
		Audience      string
		RequiredRoles []string
		Required      bool
		// GroupsClaim names the token claim carrying the IdP groups the
		// catalogue's roles are keyed by. Another IdP may say "memberOf".
		GroupsClaim string
		// Scopes is what the RFC 9728 document advertises, and what a client asks
		// for; empty takes auth.DefaultScopes.
		//
		// It has to name the scope that carries GroupsClaim, and the two are not
		// the same word — on Authentik the claim sits behind an "entitlements"
		// scope and there is no "groups" scope at all. Advertising a scope the
		// IdP does not have fails silently: it is dropped, the token comes back
		// without the claim, and every call is a 403 with no groups in it.
		Scopes []string
	}
	Catalog struct {
		Path string
	}
	// Storage points at the Nomad volume holding clones, worktrees and AST
	// indexes. An empty ReposDir turns the code tools off.
	Storage struct {
		ReposDir     string
		WorktreesDir string
		MaxDiskBytes int64
		// ASTEngine turns on code_refs and blast_radius. Opt-in because the
		// engine is a separate binary in the image: an instance without it
		// refuses those two by name rather than failing at boot.
		ASTEngine bool
		// ASTBin overrides the binary; empty means code-graph-mcp from PATH.
		ASTBin string
	}
	Limits struct {
		UpstreamTimeout time.Duration
		JQTimeout       time.Duration
		MaxBytes        int
		MaxConcurrent   int
		// DBTimeout and DBMaxRows are what a database target falls back to when
		// it names no limit of its own.
		DBTimeout time.Duration
		DBMaxRows int
	}
	RateLimit ratelimit.Config
	APIKeys   []auth.Key
}

type App struct {
	embedlog.Logger
	appName   string
	cfg       Config
	echo      *echo.Echo
	authStore *auth.Store
	verifier  *auth.Verifier
	rateLimit *ratelimit.Limiter
	docs      *doc.Library
	targets   *target.Catalog
	upstream  *upstream.Client
	repos     *git.Store
	graph     *codegraph.Client
	db        *dbq.Manager
	cron      *cron.Manager
	sessions  *ring.Sessions

	// fetchSlots and fetchWG belong to the webhook-driven mirror fetches: the
	// slots cap how many run at once, the wait group lets Shutdown wait.
	fetchSlots chan struct{}
	fetchWG    sync.WaitGroup
}

func New(appName string, sl embedlog.Logger, cfg Config) *App {
	return &App{
		appName: appName,
		cfg:     cfg,
		echo:    appkit.NewEcho(),
		Logger:  sl,
	}
}

// Run starts the application.
func (a *App) Run(ctx context.Context) error {
	a.initUpstream()
	if err := a.initRepos(ctx); err != nil {
		return err
	}

	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	if err != nil {
		return fmt.Errorf("load docs: %w", err)
	}
	a.docs = lib
	a.Print(ctx, "loaded docs", "resources", len(lib.Resources()), "prompts", len(lib.Prompts()))

	if err := a.loadTargets(ctx); err != nil {
		return err
	}
	if err := a.initDatabases(ctx); err != nil {
		return err
	}

	a.initAPIKeys(ctx)

	if err := a.initOIDC(ctx); err != nil {
		return err
	}
	a.Print(ctx, "MCP auth: "+a.mcpAuthLabel(), "issuer", a.cfg.OIDC.Issuer)

	a.initRateLimit(ctx)
	a.sessions = ring.NewSessions(0, nil)

	a.registerMetrics()
	a.registerHandlers()
	a.registerDebugHandlers()
	a.registerMCPHandlers()
	a.registerMirrorHandlers()
	a.registerCron()
	a.startCron(ctx)
	a.registerMetadata()

	return a.runHTTPServer(ctx, a.cfg.Server.Host, a.cfg.Server.Port)
}

// loadTargets reads and validates the target catalogue. A broken catalogue is a
// boot failure: half the profiles missing looks to the user like a permissions
// problem and gets debugged on the wrong side.
func (a *App) loadTargets(ctx context.Context) error {
	cat, err := target.Load(a.cfg.Catalog.Path)
	if err != nil {
		return fmt.Errorf("load targets: %w", err)
	}
	a.targets = cat
	a.Print(ctx, "loaded targets",
		"path", a.cfg.Catalog.Path,
		"profiles", len(cat.Profiles),
		"databases", len(cat.Databases),
		"roles", len(cat.Roles),
		"repos", len(cat.Repos))
	return nil
}

// initAPIKeys builds the api-key store and says out loud when a key cannot work:
// access is granted by IdP group, so a key without Groups authenticates and then
// gets 403 on every call.
func (a *App) initAPIKeys(ctx context.Context) {
	a.authStore = auth.NewStoreWithOptions(a.cfg.APIKeys, auth.StoreOptions{Realm: a.appName})
	for _, k := range a.cfg.APIKeys {
		if len(k.Groups) == 0 {
			a.Error(ctx, "api key has no groups and will be denied by every role", "user", k.UserID)
		}
	}
}

// initOIDC builds the JWT verifier. With Required=false a discovery failure is
// logged and the service keeps booting on api-keys, so a transient IdP outage
// does not take the whole thing down.
func (a *App) initOIDC(ctx context.Context) error {
	if a.cfg.OIDC.Issuer == "" {
		if a.cfg.OIDC.Required {
			return errors.New("oidc required but issuer not configured")
		}
		return nil
	}

	// Config.Validate already refused a mismatch at boot; checked again here
	// because a test that builds an App by hand skips main.
	if err := a.cfg.audienceMismatch(); err != nil {
		return err
	}

	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{
		HTTPClient:          a.upstream.HTTP(),
		Issuer:              a.cfg.OIDC.Issuer,
		ClientID:            a.cfg.OIDC.ClientID,
		Audience:            a.cfg.OIDC.Audience,
		RequiredRoles:       a.cfg.OIDC.RequiredRoles,
		GroupsClaim:         a.cfg.OIDC.GroupsClaim,
		ResourceMetadataURL: protectedResourceMetadataURL(a.cfg.Server.BaseURL),
	})
	if err != nil {
		if a.cfg.OIDC.Required {
			return fmt.Errorf("oidc verifier: %w", err)
		}
		a.Error(ctx, "oidc discovery failed, continuing without it", "err", err.Error())
		return nil
	}
	a.verifier = v
	return nil
}

// initUpstream builds the HTTP client every api_call goes through. MaxBodyBytes
// is larger than the answer limit: cutting before jq would throw away exactly
// what the filter was there to find.
func (a *App) initUpstream() {
	maxBody := int64(defaultMaxBodyBytes)
	if a.cfg.Limits.MaxBytes > 0 {
		maxBody = int64(a.cfg.Limits.MaxBytes) * bodyToAnswerRatio
	}
	a.upstream = upstream.New(upstream.Options{
		AppName:       a.appName,
		Version:       appkit.Version(),
		Timeout:       a.cfg.Limits.UpstreamTimeout,
		MaxConcurrent: a.cfg.Limits.MaxConcurrent,
		MaxBodyBytes:  maxBody,
	})
}

const (
	defaultMaxBodyBytes = 8 << 20 // 8 MiB
	bodyToAnswerRatio   = 16
)

// initRepos prepares the on-disk mirrors. Cloning is not done here: the first
// question about a repository pays for it, rather than every boot paying minutes
// for repositories nobody asks about. What is done here is checking that git
// exists at all, so a missing tool fails clearly.
func (a *App) initRepos(ctx context.Context) error {
	a.repos = git.New(git.Options{
		ReposDir:     a.cfg.Storage.ReposDir,
		WorktreesDir: a.cfg.Storage.WorktreesDir,
		MaxDiskBytes: a.cfg.Storage.MaxDiskBytes,
	})
	if a.cfg.Storage.ReposDir == "" {
		a.Print(ctx, "code tools disabled: Storage.ReposDir is empty")
		a.repos = nil
		return nil
	}
	if err := a.repos.Check(ctx); err != nil {
		return err
	}
	a.Print(ctx, "git ready", "version", a.repos.Version(), "repos", a.cfg.Storage.ReposDir)
	return a.initGraph(ctx)
}

// initGraph checks the AST engine the way git is checked: saying at boot that it
// is missing beats a puzzle on the first question about callers. An instance
// without it keeps every code tool but code_refs and blast_radius.
func (a *App) initGraph(ctx context.Context) error {
	if !a.cfg.Storage.ASTEngine {
		a.Print(ctx, "AST engine disabled by config: code_refs and blast_radius will refuse")
		return nil
	}

	graph := codegraph.New(codegraph.Options{
		Bin:     a.cfg.Storage.ASTBin,
		Timeout: a.cfg.Limits.UpstreamTimeout,
	})
	version, err := graph.Version(ctx)
	if err != nil {
		return err
	}
	a.graph = graph
	a.Print(ctx, "AST engine ready", "version", version)
	return nil
}

// initRateLimit builds the MCP limiter and logs what it will actually enforce
// — each limit is disabled independently when its config value is ≤0.
func (a *App) initRateLimit(ctx context.Context) {
	// Which calls the budget spares is a fact about the tools, not about a
	// contour, so it is set here rather than read from the config.
	cfg := a.cfg.RateLimit
	cfg.Exempt = rpc.ExemptFromBudget
	a.rateLimit = ratelimit.New(cfg)
	if a.rateLimit.Disabled() {
		a.Print(ctx, "MCP rate limit: disabled")
		return
	}
	a.Print(ctx, "MCP rate limit: enabled",
		"rpm", a.cfg.RateLimit.PerUserRPM,
		"user_concurrent", a.cfg.RateLimit.PerUserConcurrent,
		"global_concurrent", a.cfg.RateLimit.GlobalConcurrent,
		"cost_budget_per_hour", a.cfg.RateLimit.CostBudgetPerHour)
}

// Shutdown stops everything this process started, in the order that makes the
// rest safe: the listener first, so no new work arrives; then the cron, whose
// running jobs are waited for; then the rate limiter's eviction loop; and last
// the webhook fetches in flight. The timeout is shared, and Nomad's
// kill_timeout is what really bounds it.
func (a *App) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := a.echo.Shutdown(ctx)

	if a.cron != nil {
		// Stop returns a context that is done once the jobs that were running
		// have finished.
		select {
		case <-a.cron.Stop().Done():
		case <-ctx.Done():
			a.Error(context.WithoutCancel(ctx), "shutdown: cron jobs still running, giving up on them")
		}
	}
	if a.rateLimit != nil {
		a.rateLimit.Stop()
	}
	a.waitFetches(ctx)
	a.closeDatabases(ctx)
	return err
}

// registerMetadata registers meta info for monitoring.
func (a *App) registerMetadata() {
	// No databases of its own: everything ringsrv reads lives behind an upstream
	// HTTP API or on the git volume.
	md := appkit.NewMetadataManager(appkit.MetadataOpts{
		HasPublicAPI:  true,
		HasPrivateAPI: false,
		HasCronJobs:   a.cron != nil,
		// The upstreams are what this service is: a proxy that reaches seven
		// systems must not show up in the C4 diagram standing alone.
		Services: a.upstreamServices(),
	})
	md.RegisterMetrics()

	a.echo.GET("/debug/metadata", md.Handler)
}

// upstreamServices names the systems this instance is configured to reach.
func (a *App) upstreamServices() []appkit.ServiceMetadata {
	if a.targets == nil {
		return nil
	}
	names := make([]string, 0, len(a.targets.Profiles))
	for name := range a.targets.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]appkit.ServiceMetadata, 0, len(names))
	for _, name := range names {
		out = append(out, appkit.NewServiceMetadata(name, appkit.MetadataServiceTypeExternal))
	}
	return out
}
