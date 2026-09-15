package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/mcptool"
)

// The tools as mcptool.Tool: a name, the answer, and whether this caller may
// see the tool at all. The registry asks Describe on every tools/list, so role
// filtering is a property of each tool rather than of a list somebody has to
// remember to filter. Each type is a view of ToolsService, not a second home for
// logic.
//
// Each also implements mcptool.Visibility — Describe's condition without its
// answer, for the two callers that want only the bool: the registry dispatching
// tools/call, and help. Describing costs most of a kilobyte. The two halves
// agree by construction: every Describe here opens by asking Visible.
type visibleTool interface {
	mcptool.Tool
	mcptool.Visibility
}

// apiCallTool is the one HTTP call to one allow-listed upstream.
type apiCallTool struct{ s ToolsService }

func (t apiCallTool) Name() string { return ToolAPICall }

func (t apiCallTool) allow(ctx context.Context) (target.Access, bool) {
	acc, err := t.s.access(ctx)
	return acc, err == nil && acc.HasTool(ToolAPICall)
}

func (t apiCallTool) Visible(ctx context.Context) bool {
	_, ok := t.allow(ctx)
	return ok
}

func (t apiCallTool) Describe(ctx context.Context) (mcp.Tool, bool) {
	acc, ok := t.allow(ctx)
	if !ok {
		return mcp.Tool{}, false
	}
	// The headers argument is only offered when one of this role's targets can
	// accept it: it costs bytes in every tools/list, and an argument no target
	// honours invites a call that will be refused.
	writable, headed := t.s.targetTraits(acc)
	schema := apiCallInputSchema
	if headed {
		schema = apiCallInputSchemaHeaders
	}
	return mcp.Tool{
		Name:        ToolAPICall,
		Description: t.s.apiCallDescription(acc),
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{
			Title:        "HTTP call to an allow-listed target (" + t.s.env + ")",
			ReadOnlyHint: new(!writable),
			// Write targets create issues, comments and MRs; nothing merges,
			// deletes or deploys. The client must still ask a human.
			DestructiveHint: new(writable),
			OpenWorldHint:   new(true),
		},
	}, true
}

func (t apiCallTool) Call(ctx context.Context, args map[string]any) mcp.ToolCallResult {
	return t.s.callAPI(ctx, args)
}

// codeTool is one of the six tools that read code. They differ by name, title,
// schema and description; the conditions are the same for all six — the instance
// has the code, the role has repositories, and the AST engine is here when the
// tool needs one.
//
// desc is the rendered text, not a function: all six are built from the contour
// alone, so they are the same string for the life of the process.
type codeTool struct {
	s      ToolsService
	name   string
	title  string
	desc   string
	schema json.RawMessage
}

func (t codeTool) Name() string { return t.name }

func (t codeTool) Visible(ctx context.Context) bool {
	acc, err := t.s.access(ctx)
	switch {
	case err != nil, !t.s.code.Available(), len(acc.Repos) == 0:
		return false
	case !acc.HasTool(t.name), needsEngine(t.name) && !t.s.code.HasEngine():
		return false
	}
	return true
}

func (t codeTool) Describe(ctx context.Context) (mcp.Tool, bool) {
	if !t.Visible(ctx) {
		return mcp.Tool{}, false
	}
	return mcp.Tool{
		Name:        t.name,
		Description: t.desc,
		InputSchema: t.schema,
		Annotations: &mcp.ToolAnnotations{
			// No env suffix: it is the first word of the description already,
			// and eight titles are paid for on every request.
			Title:        t.title,
			ReadOnlyHint: new(true),
			// Spelt out because the spec default for an absent destructiveHint
			// is true, and saying nothing would claim a reader is destructive.
			DestructiveHint: new(false),
			OpenWorldHint:   new(false),
		},
	}, true
}

func (t codeTool) Call(ctx context.Context, args map[string]any) mcp.ToolCallResult {
	return t.s.callCode(ctx, t.name, args)
}

// codeTools are the six, in the order they appear in the list; the descriptions
// are rendered here once.
func codeTools(s ToolsService) []visibleTool {
	return []visibleTool{
		codeTool{s, ToolCodeRead, "Read code at a commit", s.codeReadDescription(), codeReadInputSchema},
		codeTool{s, ToolCodeSearch, "Search code at a commit", s.codeSearchDescription(), codeSearchInputSchema},
		codeTool{s, ToolCodeHistory, "History of lines, not of a file", s.codeHistoryDescription(), codeHistoryInputSchema},
		codeTool{s, ToolCodeRefs, "Who calls this symbol", s.codeRefsDescription(), codeRefsInputSchema},
		codeTool{s, ToolBlastRadius, "What a release changed, against the stack trace", s.blastRadiusDescription(), blastRadiusInputSchema},
		codeTool{s, ToolWhy, "Why this code is like this", s.whyDescription(), whyInputSchema},
	}
}

// dbQueryTool is read-only SQL against a database from the catalogue.
type dbQueryTool struct{ s ToolsService }

func (t dbQueryTool) Name() string { return ToolDBQuery }

func (t dbQueryTool) Visible(ctx context.Context) bool {
	_, ok := t.s.dbAccess(ctx, ToolDBQuery)
	return ok
}

func (t dbQueryTool) Describe(ctx context.Context) (mcp.Tool, bool) {
	acc, ok := t.s.dbAccess(ctx, ToolDBQuery)
	if !ok {
		return mcp.Tool{}, false
	}
	return mcp.Tool{
		Name:        ToolDBQuery,
		Description: t.s.dbQueryDescription(acc),
		InputSchema: dbQueryInputSchema,
		Annotations: &mcp.ToolAnnotations{
			Title: "Read-only SQL", ReadOnlyHint: new(true), DestructiveHint: new(false), OpenWorldHint: new(true),
		},
	}, true
}

func (t dbQueryTool) Call(ctx context.Context, args map[string]any) mcp.ToolCallResult {
	return t.s.callDBQuery(ctx, args)
}

// dbIntrospectTool is the schema of the tables a role may read.
type dbIntrospectTool struct{ s ToolsService }

func (t dbIntrospectTool) Name() string { return ToolDBIntrospect }

func (t dbIntrospectTool) Visible(ctx context.Context) bool {
	_, ok := t.s.dbAccess(ctx, ToolDBIntrospect)
	return ok
}

func (t dbIntrospectTool) Describe(ctx context.Context) (mcp.Tool, bool) {
	if !t.Visible(ctx) {
		return mcp.Tool{}, false
	}
	return mcp.Tool{
		Name:        ToolDBIntrospect,
		Description: t.s.dbIntrospectDescription(),
		InputSchema: dbIntrospectInputSchema,
		Annotations: &mcp.ToolAnnotations{
			Title: "Database schema", ReadOnlyHint: new(true), DestructiveHint: new(false), OpenWorldHint: new(true),
		},
	}, true
}

func (t dbIntrospectTool) Call(ctx context.Context, args map[string]any) mcp.ToolCallResult {
	return t.s.callDBIntrospect(ctx, args)
}

// dbAccess is the visibility rule of both database tools, with the access the
// description needs.
func (s ToolsService) dbAccess(ctx context.Context, tool string) (target.Access, bool) {
	acc, err := s.access(ctx)
	if err != nil {
		return acc, false
	}
	return acc, s.dbToolsVisible(acc, tool)
}

// repoMapTool answers what a service is called in the other targets.
type repoMapTool struct{ s ToolsService }

func (t repoMapTool) Name() string { return ToolRepoMap }

func (t repoMapTool) allow(ctx context.Context) (target.Access, bool) {
	acc, err := t.s.access(ctx)
	return acc, err == nil && acc.HasTool(ToolRepoMap) && len(acc.Repos) > 0
}

func (t repoMapTool) Visible(ctx context.Context) bool {
	_, ok := t.allow(ctx)
	return ok
}

func (t repoMapTool) Describe(ctx context.Context) (mcp.Tool, bool) {
	acc, ok := t.allow(ctx)
	if !ok {
		return mcp.Tool{}, false
	}
	return mcp.Tool{
		Name:        ToolRepoMap,
		Description: t.s.repoMapDescription(acc),
		InputSchema: repoMapInputSchema,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Repository map (" + t.s.env + ")",
			ReadOnlyHint:    new(true),
			DestructiveHint: new(false),
			// It reads only the catalogue this instance was started with.
			OpenWorldHint: new(false),
		},
	}, true
}

func (t repoMapTool) Call(ctx context.Context, args map[string]any) mcp.ToolCallResult {
	return t.s.callRepoMap(ctx, args)
}

// helpTool hands out the cheat sheets. It is last in the list and only there
// when there is something to read about, which is why it holds the other tools:
// asking them is the only way to know that cannot drift from what the list
// shows. Visible rather than Describe, which would render every sibling's text.
type helpTool struct {
	s      ToolsService
	others []visibleTool
}

func (t helpTool) Name() string { return ToolHelp }

func (t helpTool) Visible(ctx context.Context) bool {
	return slices.ContainsFunc(t.others, func(o visibleTool) bool { return o.Visible(ctx) })
}

func (t helpTool) Describe(ctx context.Context) (mcp.Tool, bool) {
	if !t.Visible(ctx) {
		return mcp.Tool{}, false
	}
	return t.s.describeHelp(), true
}

func (t helpTool) Call(ctx context.Context, args map[string]any) mcp.ToolCallResult {
	return t.s.callHelp(ctx, args)
}

// newRegistry builds the dispatcher. The order is the order of tools/list:
// api_call, the code tools, the database tools, repo_map, and help last.
//
// No cache hint: this list is computed per caller, so a cache between us and
// them could hand one user the tools of another. The catalogue, which is the
// same for everyone, does declare one (server.go).
func newRegistry(s ToolsService) *mcptool.Registry {
	tools := make([]visibleTool, 0, maxTools)
	tools = append(tools, apiCallTool{s})
	tools = append(tools, codeTools(s)...)
	tools = append(tools, dbQueryTool{s}, dbIntrospectTool{s}, repoMapTool{s})
	// Cloned, so help holds the list it was given rather than a window onto the
	// array the next append writes into.
	tools = append(tools, helpTool{s: s, others: slices.Clone(tools)})

	byName := make(map[string]visibleTool, len(tools))
	for _, t := range tools {
		byName[t.Name()] = t
	}

	return mcptool.NewRegistry(mcp.Map(tools, func(t visibleTool) mcptool.Tool { return t })...).
		With(
			// The arguments, so a call the registry will not dispatch is still
			// recorded for what it asked.
			mcptool.WithCallHook(withCallArgs, nil),
			mcptool.WithUnknownTool(refusal{s: s, byName: byName}.answer),
		)
}

// callArgs carries the arguments of one tools/call past the registry, which
// hands them to the tool it dispatches but not to the hook that answers when it
// dispatches none — and a refused call's arguments are why its record is kept.
type callArgsKey struct{}

func withCallArgs(ctx context.Context, _ string, args map[string]any) context.Context {
	return context.WithValue(ctx, callArgsKey{}, args)
}

func callArgs(ctx context.Context) map[string]any {
	args, _ := ctx.Value(callArgsKey{}).(map[string]any)
	return args
}

// refusal answers for a call the registry will not dispatch. It holds the tools
// by name rather than asking the service, because the hook is wired before the
// service holds the registry.
type refusal struct {
	s      ToolsService
	byName map[string]visibleTool
}

// answer refuses in this service's own codes.
//
// The library's default hides "there is no such tool" behind "your role does not
// grant this one", which is right for a server whose tool set is a secret. This
// one's is not: the names are fixed and public, and a user who sees zero tools
// debugs the server while one who sees a refusal asks for access. What stays
// hidden is the catalogue, refused by name elsewhere.
//
// A name this service knows is handed to its tool anyway: each refuses on the
// condition that hid it, naming the reason — a missing AST engine is not a
// missing grant — and writes the record that condition deserves, the statements
// of a refused db_query among them.
//
// help is the exception: "there is something to be a help for" is a fact about
// the other ten, so only the registry can state it.
func (r refusal) answer(ctx context.Context, name string, _ []string) mcp.ToolCallResult {
	if t, ok := r.byName[name]; ok && name != ToolHelp {
		return t.Call(ctx, callArgs(ctx))
	}

	rec, done := r.s.beginAudit(ctx, name)
	defer done()

	if _, ok := r.byName[name]; ok { // help, hidden for this role
		observe(name, "-", outcomeForbidden)
		return refuse(rec, ToolError{
			Code:    ErrCodeForbiddenRole,
			Message: "your role does not grant " + name,
			Env:     r.s.env,
		})
	}
	// Not counted per tool: a counter labelled with client input has no bound on
	// its cardinality. The library's own series uses a fixed placeholder.
	return refuse(rec, ToolError{
		Code:    ErrCodeBadArgs,
		Message: fmt.Sprintf("unknown tool %q", name),
		Env:     r.s.env,
	})
}
