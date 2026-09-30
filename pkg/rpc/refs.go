package rpc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/code"

	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/ratelimit"
)

// ToolCodeRefs answers "who calls this". An incident rarely needs it — the stack
// trace already carries the chain — but an open question about code does.
const ToolCodeRefs = "code_refs"

// Error codes of the engine-backed tools.
const (
	// ErrCodeNoEngine says the instance ships without the AST engine.
	ErrCodeNoEngine = "EngineUnavailable"
	// ErrCodeAmbiguous carries the candidate paths so the next call can pick.
	ErrCodeAmbiguous = "AmbiguousSymbol"
)

// engineTools are the tools that cannot answer without the AST engine. The same
// table gates tools/list and the call, so an instance without the engine does
// not advertise a tool that can only refuse.
var engineTools = map[string]bool{
	ToolCodeRefs:    true,
	ToolBlastRadius: true,
}

func needsEngine(tool string) bool { return engineTools[tool] }

// implementorsRefusal is why the tool says no. The reason travels with a way
// forward, because "not supported" without one just moves the work to the user.
const implementorsRefusal = "implementors are not supported: on Go the engine resolves interface implementations by name, " +
	"which produces plausible wrong answers rather than empty ones. " +
	"Search for the interface name with code_search and read the types that mention it."

// CodeRefsArgs are the inputs to code_refs.
type CodeRefsArgs struct {
	Repo   string `json:"repo" jsonschema:"required" jsonschema_description:"Repository from repo_map."`
	Ref    string `json:"ref" jsonschema:"required" jsonschema_description:"Commit SHA."`
	Symbol string `json:"symbol" jsonschema:"required" jsonschema_description:"Function, method or type name."`
	Path   string `json:"path,omitempty" jsonschema_description:"File of the symbol; needed when ambiguous."`
	Kind   string `json:"kind,omitempty" jsonschema:"enum=callers,enum=implementors,enum=both" jsonschema_description:"callers (default) | implementors (unsupported) | both."`
	Max    int    `json:"max,omitempty" jsonschema_description:"Default 40."`
	Intent string `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// RefEntry is one site that references the symbol.
type RefEntry struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Symbol string `json:"symbol"`
	// Layer comes from the repository's Layers map: it costs nothing and
	// immediately answers "did this cross a layer boundary".
	Layer string `json:"layer,omitempty"`
	// Confidence is the engine's own word: "inferred" means the edge was matched
	// by name, which is where Go methods of one name get confused.
	Confidence string `json:"confidence,omitempty"`
	Relation   string `json:"relation,omitempty"`
}

// CodeRefsResult is the answer of code_refs.
type CodeRefsResult struct {
	Repo    string     `json:"repo"`
	Ref     string     `json:"ref"`
	Symbol  string     `json:"symbol"`
	Kind    string     `json:"kind"`
	Callers []RefEntry `json:"callers"`
	// Unsupported names what this answer does not cover: with kind=both the
	// callers are real and the implementors are missing, and halving an answer
	// silently is worse than not answering.
	Unsupported string `json:"unsupported,omitempty"`
	// Budget is the hourly work budget with this lookup paid for.
	Budget *Budget `json:"budget,omitempty"`
}

var codeRefsInputSchema = mcp.SchemaFor(CodeRefsArgs{})

// refs answers code_refs.
func (s ToolsService) refs(ctx context.Context, arguments map[string]any, rec *auditRecord) mcp.ToolCallResult {
	var args CodeRefsArgs
	if err := mcp.DecodeArgs(arguments, &args); err != nil {
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	rec.Target, rec.Path, rec.Intent = args.Repo, args.Symbol, args.Intent
	if args.Path != "" {
		rec.Path = args.Symbol + " @ " + args.Path
	}

	kind := args.Kind
	if kind == "" {
		kind = code.KindCallers
	}
	rec.Method = kind

	repo, toolErr := s.codeAccess(ctx, ToolCodeRefs, args.Repo)
	if toolErr != nil {
		return refuse(rec, *toolErr)
	}

	started := time.Now()
	found, err := s.code.Refs(ctx, repo, code.RefsQuery{
		Ref: args.Ref, Symbol: args.Symbol, Path: args.Path, Kind: kind, Max: args.Max,
	})
	ratelimit.ChargeFor(ctx, args.Repo, time.Since(started))
	if err != nil {
		return refuse(rec, s.codeError(err, args.Repo, args.Symbol))
	}

	out := CodeRefsResult{
		Repo: args.Repo, Ref: found.Ref, Symbol: args.Symbol, Kind: found.Kind,
		Callers: mcp.Map(found.Callers, newRefEntry(args.Repo)),
		Budget:  budgetOf(ctx),
	}
	if found.Kind == code.KindBoth {
		out.Unsupported = implementorsRefusal
	}
	return okResultJSON(out, s.env)
}

func newRefEntry(repo string) func(code.Ref) RefEntry {
	return func(r code.Ref) RefEntry {
		return RefEntry{
			Repo: repo, Path: r.Path, Line: r.Line, Symbol: r.Symbol,
			Layer: r.Layer, Confidence: r.Confidence, Relation: r.Relation,
		}
	}
}

func (s ToolsService) codeRefsDescription() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · кто вызывает символ на коммите; kind: callers (implementors не поддерживается). ", strings.ToUpper(s.env))
	b.WriteString(repoHint + ". AmbiguousSymbol — повторите с path одного из кандидатов; confidence: inferred — связь по имени.")
	return b.String()
}
