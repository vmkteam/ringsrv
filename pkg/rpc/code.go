package rpc

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring/code"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/ratelimit"
)

// Tools that answer about code: an investigation that stops at the symptom ends
// with "looks like a bug somewhere".
const (
	ToolCodeRead   = "code_read"
	ToolCodeSearch = "code_search"
)

// Error codes of the code tools.
const (
	ErrCodeRepoUnknown = "RepoUnknown"
	ErrCodeRefUnknown  = "RefUnknown"
	ErrCodePathDenied  = "PathDenied"
	ErrCodeNotIndexed  = "NotIndexed"
)

// codeAccess resolves the caller and the repository in one step: every code tool
// needs both and refuses on the same conditions.
func (s ToolsService) codeAccess(ctx context.Context, tool, repo string) (code.Repo, *ToolError) {
	acc, err := s.codeGrant(ctx, tool)
	if err != nil {
		return code.Repo{}, err
	}
	return s.repoOf(acc, repo)
}

// repoOf turns a repository name into the domain's view of it. A name this role
// may not ask about is refused exactly like one that does not exist: the shape
// of an error must not leak the catalogue.
func (s ToolsService) repoOf(acc target.Access, name string) (code.Repo, *ToolError) {
	r, ok := s.targets.Repos[name]
	if !ok || !slices.Contains(acc.Repos, name) {
		return code.Repo{}, &ToolError{
			Code:    ErrCodeRepoUnknown,
			Message: fmt.Sprintf("unknown repository %q", name),
			Env:     s.env,
			Repos:   acc.Repos,
		}
	}
	return code.Repo{Name: name, Cfg: r}, nil
}

// codeGrant answers "may this caller use this tool at all", without naming a
// repository — the half of codeAccess code_search needs first.
func (s ToolsService) codeGrant(ctx context.Context, tool string) (target.Access, *ToolError) {
	acc, err := s.access(ctx)
	if err != nil {
		return acc, &ToolError{Code: ErrCodeForbiddenRole, Message: err.Error(), Env: s.env}
	}
	if !acc.HasTool(tool) {
		return acc, &ToolError{Code: ErrCodeForbiddenRole, Message: "your role does not grant " + tool, Env: s.env}
	}
	if !s.code.Available() {
		return acc, &ToolError{Code: ErrCodeNotIndexed, Message: "code tools are not configured on this instance", Env: s.env}
	}
	if needsEngine(tool) && !s.code.HasEngine() {
		return acc, &ToolError{Code: ErrCodeNoEngine, Message: noEngineMessage(tool), Env: s.env}
	}
	return acc, nil
}

func noEngineMessage(tool string) string {
	return "this instance runs without the AST engine, so " + tool + " cannot answer; " + engineAlternatives
}

// engineAlternatives is what still works without the engine.
const engineAlternatives = "code_search finds call sites textually and code_history mode=range lists what a release changed"

// codeWindow is one place to read: a path and an optional line range.
type codeWindow struct {
	Path     string
	LineFrom int
	LineTo   int
}

// windowSuffixRe is the line part of a window: ":12" or ":12-40".
var windowSuffixRe = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)

// parseWindow reads "path", "path:12" or "path:12-40" — the shape a code_search
// match already has, so one call's answer is the next call's argument.
func parseWindow(s string) (codeWindow, error) {
	w := codeWindow{Path: s}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return w, nil
	}
	m := windowSuffixRe.FindStringSubmatch(s[i+1:])
	if m == nil {
		return codeWindow{}, fmt.Errorf("window %q: expected path, path:line or path:from-to", s)
	}
	w.Path = s[:i]
	w.LineFrom, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		w.LineTo, _ = strconv.Atoi(m[2])
	}
	return w, nil
}

// maxReadWindows bounds one code_read: a search points at a handful of places.
const maxReadWindows = 20

// CodeReadArgs are the inputs to code_read.
type CodeReadArgs struct {
	Repo string `json:"repo" jsonschema:"required" jsonschema_description:"Repository from repo_map."`
	// Ref is required: defaulting to the branch is how an investigation ends up
	// reading code that is not deployed.
	Ref      string `json:"ref" jsonschema:"required" jsonschema_description:"Deployed commit SHA, not a branch."`
	Path     string `json:"path,omitempty" jsonschema_description:"Repo-relative path."`
	LineFrom int    `json:"line_from,omitempty" jsonschema_description:"First line, 1-based."`
	LineTo   int    `json:"line_to,omitempty" jsonschema_description:"Last line, inclusive."`
	// Windows reads several places of one commit in one call: a search answer
	// usually points at three or four.
	Windows  []string `json:"windows,omitempty" jsonschema_description:"path, path:12 or path:12-40; up to 20."`
	MaxLines int      `json:"max_lines,omitempty" jsonschema_description:"Default 200."`
	Intent   string   `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// CodeReadWindowsResult answers a windows read: one file per window, in the
// order asked.
type CodeReadWindowsResult struct {
	Repo    string           `json:"repo"`
	Ref     string           `json:"ref"`
	Windows []CodeReadResult `json:"windows"`
	// Budget is the hourly work budget with these windows paid for.
	Budget *Budget `json:"budget,omitempty"`
}

// readWindows turns the two ways of asking — one path, or a list of windows —
// into one list, and refuses the ambiguous and the empty.
func readWindows(args CodeReadArgs) ([]codeWindow, string) {
	switch {
	case args.Path != "" && len(args.Windows) > 0:
		return nil, "pass either path or windows, not both"
	case args.Path != "":
		return []codeWindow{{Path: args.Path, LineFrom: args.LineFrom, LineTo: args.LineTo}}, ""
	case len(args.Windows) == 0:
		return nil, "path or windows is required"
	case len(args.Windows) > maxReadWindows:
		return nil, fmt.Sprintf("%d windows asked, at most %d in one call", len(args.Windows), maxReadWindows)
	}
	out := make([]codeWindow, 0, len(args.Windows))
	for _, s := range args.Windows {
		w, err := parseWindow(s)
		if err != nil {
			return nil, err.Error()
		}
		if w.Path == "" {
			return nil, "every window needs a path"
		}
		out = append(out, w)
	}
	return out, ""
}

// CodeReadResult carries the text with line numbers: without them the reference
// path:line@sha cannot be assembled, and an answer about code without one is a
// rumour.
type CodeReadResult struct {
	Repo      string   `json:"repo"`
	Ref       string   `json:"ref"`
	Path      string   `json:"path"`
	LineFrom  int      `json:"line_from"`
	LineTo    int      `json:"line_to"`
	Total     int      `json:"total_lines"`
	Truncated bool     `json:"truncated"`
	Lines     []string `json:"lines,omitempty"`
	// DedupOf points at the window this one repeats: read once, lines are there.
	DedupOf *int `json:"dedup_of,omitempty"`
	// Budget is the hourly work budget with this read paid for — on the answer
	// to a single path; a window of a list leaves it to the list.
	Budget *Budget `json:"budget,omitempty"`
}

// newCodeReadResult numbers the lines: numbering is a property of the answer,
// not of the file, which is why the domain hands over plain lines.
func newCodeReadResult(repo string, f *code.File) CodeReadResult {
	numbered := make([]string, 0, len(f.Lines))
	for i, line := range f.Lines {
		numbered = append(numbered, fmt.Sprintf("%d: %s", f.From+i, line))
	}
	return CodeReadResult{
		Repo: repo, Ref: f.Ref, Path: f.Path,
		LineFrom: f.From, LineTo: f.To, Total: f.Total,
		Truncated: f.Truncated, Lines: numbered,
	}
}

// CodeSearchArgs are the inputs to code_search.
type CodeSearchArgs struct {
	Query      string   `json:"query" jsonschema:"required" jsonschema_description:"Literal, or ERE when regex is true."`
	Ref        string   `json:"ref" jsonschema:"required" jsonschema_description:"Commit SHA."`
	Repos      []string `json:"repos,omitempty" jsonschema_description:"Default: all your repositories."`
	PathGlob   string   `json:"path_glob,omitempty" jsonschema_description:"Path glob, e.g. *.go."`
	Regex      bool     `json:"regex,omitempty" jsonschema_description:"Treat query as ERE."`
	MaxMatches int      `json:"max_matches,omitempty" jsonschema_description:"Default 50."`
	Context    int      `json:"context,omitempty" jsonschema_description:"Context lines per match; usually saves a code_read."`
	Intent     string   `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// CodeSearchResult keeps the flat "repo path:line: text" shape: cheaper than a
// JSON wrapper around every line, and already familiar to the model.
type CodeSearchResult struct {
	Ref       string   `json:"ref"`
	Matches   []string `json:"matches"`
	Truncated bool     `json:"truncated"`
	// ReposWithoutRef lists the repositories left out for not having the commit:
	// one SHA lives in one repository, and the answer says which it skipped
	// rather than failing on them.
	ReposWithoutRef []string `json:"repos_without_ref,omitempty"`
	// Budget is the hourly work budget with this search paid for.
	Budget *Budget `json:"budget,omitempty"`
}

// readCode answers code_read.
func (s ToolsService) readCode(ctx context.Context, arguments map[string]any, rec *auditRecord) mcp.ToolCallResult {
	var args CodeReadArgs
	if err := mcp.DecodeArgs(arguments, &args); err != nil {
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	windows, bad := readWindows(args)
	if bad != "" {
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: bad, Env: s.env})
	}
	paths := make([]string, 0, len(windows))
	for _, w := range windows {
		paths = append(paths, w.Path)
	}
	rec.Target, rec.Path, rec.Intent = args.Repo, strings.Join(paths, ","), args.Intent

	repo, toolErr := s.codeAccess(ctx, ToolCodeRead, args.Repo)
	if toolErr != nil {
		return refuse(rec, *toolErr)
	}

	// The same window twice in one list is read once, but the answer still has
	// one entry per window asked: dropping repeats would stop windows[i] from
	// meaning "the i-th window you sent".
	dupes := dedup(windows, func(w codeWindow) codeWindow { return w })

	// One window that cannot be read fails the call: a hole in a partial answer
	// reads as "that place is empty".
	files := make([]*code.File, len(windows))
	for i, w := range windows {
		if dupes[i] >= 0 {
			continue
		}
		started := time.Now()
		file, err := s.code.Read(ctx, repo, args.Ref, w.Path, code.Window{
			From: w.LineFrom, To: w.LineTo, Max: args.MaxLines,
		})
		// Each window is billed on its own, like a statement of db_query.
		ratelimit.ChargeFor(ctx, args.Repo, time.Since(started))
		if err != nil {
			return refuse(rec, s.codeError(err, args.Repo, w.Path))
		}
		files[i] = file
		rec.Truncated = rec.Truncated || file.Truncated
	}
	ref := files[0].Ref
	rec.Method = ref

	if len(args.Windows) == 0 {
		out := newCodeReadResult(args.Repo, files[0])
		out.Budget = budgetOf(ctx)
		return okResultJSON(out, s.env)
	}
	out := CodeReadWindowsResult{Repo: args.Repo, Ref: ref, Windows: make([]CodeReadResult, len(windows)), Budget: budgetOf(ctx)}
	for i, f := range files {
		if from := dupes[i]; from >= 0 {
			// The lines are already in the answer once; this entry says where.
			out.Windows[i] = CodeReadResult{
				Repo: args.Repo, Ref: ref, Path: windows[i].Path,
				LineFrom: windows[i].LineFrom, LineTo: windows[i].LineTo, DedupOf: &from,
			}
			continue
		}
		out.Windows[i] = newCodeReadResult(args.Repo, f)
	}
	return okResultJSON(out, s.env)
}

// searchCode answers code_search.
func (s ToolsService) searchCode(ctx context.Context, arguments map[string]any, rec *auditRecord) mcp.ToolCallResult {
	var args CodeSearchArgs
	if err := mcp.DecodeArgs(arguments, &args); err != nil {
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	rec.Target, rec.Path, rec.Intent = strings.Join(args.Repos, ","), args.Query, args.Intent

	acc, toolErr := s.codeGrant(ctx, ToolCodeSearch)
	if toolErr != nil {
		return refuse(rec, *toolErr)
	}

	names := args.Repos
	if len(names) == 0 {
		names = acc.Repos
	}
	if len(names) == 0 {
		return refuse(rec, ToolError{Code: ErrCodeForbiddenRole, Message: "your role grants no repositories", Env: s.env})
	}
	repos := make([]code.Repo, 0, len(names))
	for _, name := range names {
		repo, toolErr := s.repoOf(acc, name)
		if toolErr != nil {
			return refuse(rec, *toolErr)
		}
		repos = append(repos, repo)
	}

	found, err := s.code.Search(ctx, repos, code.SearchQuery{
		Query:   args.Query,
		Ref:     args.Ref,
		Glob:    args.PathGlob,
		Regex:   args.Regex,
		Context: args.Context,
		Max:     args.MaxMatches,
	})
	if err != nil {
		// Nothing charged, so the wall clock prices the failed search.
		return refuse(rec, s.codeError(err, strings.Join(names, ","), args.PathGlob))
	}
	// Each repository is billed its own part: they ran at once, so the wall
	// clock alone would charge for the slowest instead of the work done.
	for _, w := range found.Work {
		ratelimit.ChargeFor(ctx, w.Repo, w.Took)
	}

	rec.Method, rec.Truncated = found.Ref, found.Truncated

	out := CodeSearchResult{Ref: found.Ref, Truncated: found.Truncated, Matches: make([]string, 0, len(found.Matches)), ReposWithoutRef: found.Skipped, Budget: budgetOf(ctx)}
	for _, m := range found.Matches {
		// git's own convention: ":" around the line number of a match, "-"
		// around a context line.
		sep := ":"
		if m.Context {
			sep = "-"
		}
		out.Matches = append(out.Matches, fmt.Sprintf("%s %s%s%d%s %s", m.Repo, m.Path, sep, m.Line, sep, m.Text))
	}
	return okResultJSON(out, s.env)
}

// codeError turns whatever the domain or its clients returned into the shared
// error envelope: the one place that decides which failures are the caller's,
// which are the mirror's and which are the engine's.
func (s ToolsService) codeError(err error, repo, what string) ToolError {
	if e, ok := s.codeRefusal(err, what); ok {
		return e
	}

	e := ToolError{Env: s.env}
	var mode *code.ModeError
	var kind *code.KindError
	var big *code.RangeTooBigError
	var missing *code.SymbolNotFoundError
	var amb *codegraph.AmbiguousError
	switch {
	case errors.As(err, &mode):
		e.Code = ErrCodeBadArgs
		e.Message = fmt.Sprintf("unknown mode %q; available: %s", mode.Mode, strings.Join(mode.Modes, ", "))
	case errors.As(err, &kind):
		e.Code = ErrCodeBadArgs
		e.Message = fmt.Sprintf("unknown kind %q; available: %s", kind.Kind, strings.Join(kind.Kinds, ", "))
	case errors.As(err, &big):
		e.Code = ErrCodeRangeTooBig
		e.Message = fmt.Sprintf("%d commits between these two, more than the %d this tool reads; narrow the range — a diff that big is minutes of git and thousands of files",
			big.Commits, big.Max)
	case errors.As(err, &missing):
		e.Code = ErrCodeRefUnknown
		e.Message = fmt.Sprintf("%q not found in %s at %s; pass line if you know it", missing.Symbol, missing.Path, missing.Ref)
	case errors.As(err, &amb):
		// Two methods of one name in different types is normal in Go, and the
		// engine refuses to guess. The candidates have to travel with the
		// refusal: an empty list would read as "nobody calls this".
		e.Code = ErrCodeAmbiguous
		e.Message = fmt.Sprintf("%q exists in several places; repeat the call with path set to one of them", amb.Symbol)
		e.Paths = amb.Paths()

	case errors.Is(err, git.ErrNotFound), errors.Is(err, git.ErrAmbiguous):
		e.Code = ErrCodeRefUnknown
		e.Message = fmt.Sprintf("%s: %s", repo, err)
	case errors.Is(err, git.ErrBadRef), errors.Is(err, codegraph.ErrBadSymbol):
		e.Code = ErrCodeBadArgs
		e.Message = err.Error()
	case errors.Is(err, git.ErrBadPath):
		e.Code = ErrCodePathDenied
		e.Message = fmt.Sprintf("%s: %s", what, err)
	case errors.Is(err, codegraph.ErrNotFound):
		e.Code = ErrCodeRefUnknown
		e.Message = fmt.Sprintf("%s: %s", what, err)

	default:
		e.Code = ErrCodeUpstream
		e.Message = err.Error()
	}
	return e
}

// codeRefusal handles the refusals the domain makes on purpose. Each says what
// to do instead: with this few tools the error is the documentation.
func (s ToolsService) codeRefusal(err error, what string) (ToolError, bool) {
	e := ToolError{Env: s.env}
	switch {
	case errors.Is(err, code.ErrRefRequired):
		e.Code = ErrCodeBadArgs
		e.Message = "ref is required: reading a branch instead of the deployed commit is the mistake this tool exists to prevent"
	case errors.Is(err, code.ErrPathDenied):
		e.Code = ErrCodePathDenied
		e.Message = fmt.Sprintf("path %q is excluded for this repository", what)
	case errors.Is(err, code.ErrNoEngine):
		e.Code = ErrCodeNoEngine
		e.Message = "this instance runs without the AST engine; " + engineAlternatives
	case errors.Is(err, code.ErrLineOrSymbol):
		e.Code = ErrCodeBadArgs
		e.Message = "either line or symbol is required"
	case errors.Is(err, code.ErrPrevRequired):
		e.Code = ErrCodeNoPrev
		e.Message = "prev is required: ask Sentry for the previous release with api_call " +
			"(GET /api/0/projects/<org>/<project>/releases/) and pass it here"
	case errors.Is(err, code.ErrPickaxeRange):
		e.Code = ErrCodeBadArgs
		e.Message = "pickaxe requires from and to: searching the whole history takes tens of seconds, a range takes half a second"
	case errors.Is(err, code.ErrPickaxeQuery):
		e.Code = ErrCodeBadArgs
		e.Message = "pickaxe requires query — the string whose appearance to look for"
	case errors.Is(err, code.ErrImplementors):
		e.Code = ErrCodeBadArgs
		e.Message = implementorsRefusal
	default:
		return ToolError{}, false
	}
	return e, true
}

// callCode dispatches the code tools and writes the audit record they share.
func (s ToolsService) callCode(ctx context.Context, tool string, arguments map[string]any) mcp.ToolCallResult {
	rec, done := s.beginAudit(ctx, tool)
	defer done()

	res := s.dispatchCode(ctx, tool, arguments, rec)
	if !res.IsError {
		rec.Decision = audit.DecisionAllow
		rec.BytesOut = res.Size()
		observe(tool, "-", outcomeOK)
		return res
	}
	// The outcome comes from the error the tool actually returned: reporting
	// every failure as "forbidden" made an alert on refusals fire on git
	// timeouts. Whatever refused wrote its code into the record on the way out;
	// Upstream is the answer for a failure that named none.
	failure := rec.DenyReason
	if failure == "" {
		failure = ErrCodeUpstream
		rec.DenyReason = failure
	}
	observe(tool, "-", outcomeOf(failure))
	return res
}

// dispatchCode refuses a caller without a role before any tool reads its
// arguments, then hands the call to the tool.
func (s ToolsService) dispatchCode(ctx context.Context, tool string, arguments map[string]any, rec *auditRecord) mcp.ToolCallResult {
	if _, terr := s.grant(ctx, rec); terr != nil {
		return refuse(rec, *terr)
	}
	switch tool {
	case ToolCodeRead:
		return s.readCode(ctx, arguments, rec)
	case ToolCodeSearch:
		return s.searchCode(ctx, arguments, rec)
	case ToolCodeHistory:
		return s.history(ctx, arguments, rec)
	case ToolCodeRefs:
		return s.refs(ctx, arguments, rec)
	case ToolBlastRadius:
		return s.blastRadius(ctx, arguments, rec)
	case ToolWhy:
		return s.why(ctx, arguments, rec)
	default:
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: "unknown code tool " + tool, Env: s.env})
	}
}

// refuse is errResult with the code written into the record, which is where the
// dispatcher reads it from. Reading it back out of the JSON the tool had just
// built works only until an answer is not JSON, so a refusal records itself at
// the point it is made.
func refuse(rec *auditRecord, e ToolError) mcp.ToolCallResult {
	rec.DenyReason = e.Code
	return errResult(e)
}

var (
	codeReadInputSchema   = mcp.SchemaFor(CodeReadArgs{})
	codeSearchInputSchema = mcp.SchemaFor(CodeSearchArgs{})
)

// codeSheetHint points at the one document that explains the code tools. Paid
// for on every request, so it appears on two of the six rather than on all.
const codeSheetHint = "Подробности: help(code)"

// repoHint says where the repository names are. Listing them in every code tool
// cost 1.5 KB of a 12 KiB budget; now repo_map alone lists them, and a wrong
// name gets RepoUnknown with the list attached.
const repoHint = "repo — имя из repo_map"

func (s ToolsService) codeReadDescription() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · файл или окно строк на конкретном коммите, с номерами строк. ", strings.ToUpper(s.env))
	b.WriteString(repoHint + ", ref — задеплоенный SHA, не ветка. " + codeSheetHint)
	return b.String()
}

func (s ToolsService) codeSearchDescription() string {
	var b strings.Builder
	// repos has always been a list, and the log shows it used one name at a
	// time — the same query against one service, then another, seconds apart.
	fmt.Fprintf(&b, "%s · git grep по коммиту; regex: true включает ERE. repos — список: один запрос по нескольким репозиториям = один вызов, а не по вызову на репозиторий (без repos — все ваши). ", strings.ToUpper(s.env))
	b.WriteString("Ответ — строки «repo path:line: текст»; пусто значит «такой строки в этом коммите нет».")
	return b.String()
}
