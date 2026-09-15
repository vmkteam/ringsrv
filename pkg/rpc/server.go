package rpc

import (
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/mcp"
	zm "github.com/vmkteam/zenrpc-middleware"
	"github.com/vmkteam/zenrpc/v2"
)

// The zenrpc namespaces are mcpkit's own: MCP methods arrive as `tools/list` and
// are rewritten to `tools.list` by the transport, so the name belongs to
// whoever does the rewriting. Spelling them again here is how the two drift.

//go:generate go tool zenrpc

// MCPDeps bundles dependencies for the MCP zenrpc server.
type MCPDeps struct {
	// Info is what the server tells a client about itself on initialize.
	Info     mcp.ServerInfo
	Docs     *doc.Library
	Targets  *target.Catalog
	Upstream *upstream.Client
	Repos    *git.Store
	Graph    *codegraph.Client
	DB       *dbq.Manager
	MaxBytes int
	// MaxConcurrent is the upstream client's semaphore, shared by every caller:
	// a batch that filled it would stall theirs.
	MaxConcurrent int
	JQTimeout     time.Duration
	Sessions      *ring.Sessions
	Logger        embedlog.Logger
	IsDevel       bool
}

// NewMCP returns a zenrpc.Server dedicated to MCP traffic — served via
// mcpkit at /mcp. Handles initialize, ping, notifications.initialized,
// tools.*, resources.*, prompts.*.
func NewMCP(d MCPDeps) *zenrpc.Server {
	// No AllowCORS: zenrpc reads that option inside its own ServeHTTP and mcpkit
	// calls zsrv.Do() directly, so it switched nothing on. Origins of /mcp are
	// mcpkit.Options.AllowedOrigins — empty in production, and the devel doc box
	// in app.registerMCPHandlers.
	//
	// ExposeSMD is read by that same unused ServeHTTP, so it is a statement of
	// intent rather than a switch: the schema is served in devel by a handler of
	// ours, next to the doc box.
	srv := zenrpc.NewServer(zenrpc.Options{
		ExposeSMD: d.IsDevel, // SMD only in dev — MCP clients don't need it
		// TargetURL ends up in the SMD as "target", which is where SMD Box sends
		// the calls it builds. The default "/" would point the doc box at the
		// root; the endpoint that answers JSON-RPC here is /mcp.
		TargetURL: "/mcp",
	})

	// NB: WithTiming injects Response.extensions, which strict MCP clients
	// reject as unrecognized keys. Keep it out of the MCP server.
	//
	// WithSentry carries a panic inside a tool to Sentry with the method name
	// attached. The call log is our own, because the stock one logs the params.
	srv.Use(
		zm.WithDevel(d.IsDevel),
		zm.WithHeaders(),
		zm.WithSentry(zm.DefaultServerName),
		zm.WithMetrics(zm.DefaultServerName),
		withCallLog(d.Logger, zm.DefaultServerName),
	)

	deps := mcpkit.InitDeps{
		Info: d.Info,
		// Declare exactly what is registered below: a capability announced is a
		// promise to answer.
		//
		// listChanged is false everywhere, and so is subscribe: there is no
		// channel to push a notification down — the transport answers 405 to GET
		// — and a client that believed otherwise waited for notifications that
		// were never coming. The catalogue is embedded at build time, so only a
		// restart changes a list, and a client learns that by reconnecting.
		Capabilities: mcp.Capabilities{
			Tools:     &mcp.ToolsCapability{},
			Resources: &mcp.ResourcesCapability{},
			Prompts:   &mcp.PromptsCapability{},
		},
		Instructions: instructions(d.Targets.Env),
		CacheHint:    catalogueCache,
	}

	srv.RegisterAll(map[string]zenrpc.Invoker{
		// Both eras on one endpoint: initialize for the clients that still open
		// with a handshake, server/discover for the ones that no longer do.
		"":                     mcpkit.NewInitService(deps),
		mcpkit.NamespaceServer: mcpkit.NewDiscoverService(deps),
		mcpkit.NamespaceTools: NewToolsService(ToolsDeps{
			Targets:       d.Targets,
			Upstream:      d.Upstream,
			Repos:         d.Repos,
			Graph:         d.Graph,
			DB:            d.DB,
			Docs:          d.Docs,
			MaxBytes:      d.MaxBytes,
			MaxConcurrent: d.MaxConcurrent,
			JQTimeout:     d.JQTimeout,
			Sessions:      d.Sessions,
			IsDevel:       d.IsDevel,
			Logger:        d.Logger,
		}),
		// The catalogue is cacheable and the same for everyone: the markdown is
		// embedded at build time. tools/list is not — it is filtered by role, and
		// a public cache would hand one caller the tools of another.
		mcpkit.NamespaceResources: mcpkit.NewResourcesService(d.Docs, mcpkit.WithResourceCache(catalogueCache)),
		mcpkit.NamespacePrompts:   mcpkit.NewPromptsService(d.Docs, mcpkit.WithPromptCache(catalogueCache)),
	})

	return srv
}

// catalogueCache is how long a client may hold the catalogue and who may hold
// it. Five minutes: long enough to be worth caching, short enough that what a
// client keeps across a deploy is not stale for long. The library's default —
// keep nothing — is what it says when it cannot know; here the tree ships inside
// the binary.
var catalogueCache = mcp.CacheHint{TTLMs: 300_000, CacheScope: mcp.CacheScopePublic}
