package rpc

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/code"

	"github.com/vmkteam/mcpkit/mcp"
)

// ToolBlastRadius is the main hypothesis of the project: the symbols a release
// changed, intersected with the frames of a stack trace, are usually a handful
// of lines and usually the cause. On 14 replayed regressions each left a single
// row with the right issue key, against releases that changed 21 to 48 symbols.
const ToolBlastRadius = "blast_radius"

// Error codes of blast_radius.
const (
	ErrCodeRangeTooBig = "RangeTooBig"
	ErrCodeNoPrev      = "PreviousReleaseUnknown"
)

// BlastRadiusArgs are the inputs to blast_radius.
type BlastRadiusArgs struct {
	Repo string `json:"repo" jsonschema:"required" jsonschema_description:"Repository from repo_map."`
	// Release is what Sentry calls a release: usually a short commit SHA.
	Release string `json:"release" jsonschema:"required" jsonschema_description:"Deployed commit, short or full SHA."`
	Prev    string `json:"prev,omitempty" jsonschema_description:"Previously deployed commit. Required; ask Sentry via api_call."`
	// Frames are the stack frames. This is the whole point of the tool: without
	// them the answer is a list of everything that changed.
	Frames     []string `json:"frames,omitempty" jsonschema_description:"Stack frames as path:line. The intersection with them is the answer."`
	MaxSymbols int      `json:"max_symbols,omitempty" jsonschema_description:"Cap on listed symbols; default 50."`
	Intent     string   `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// ChangedSymbol is one symbol the release touched.
type ChangedSymbol struct {
	Path string `json:"path"`
	// Symbol is empty when no symbol owns the change — a package-level constant
	// or a data table. Kind says "file" then, so an empty name reads as what it
	// is rather than as a name that went missing.
	Symbol string `json:"symbol"`
	// Receiver is the type of a method, so two String methods in one file stay
	// two rows.
	Receiver string `json:"receiver,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Line     int    `json:"line,omitempty"`
	EndAt    int    `json:"end_line,omitempty"`
	Change   string `json:"change"`
	Layer    string `json:"layer,omitempty"`
	// Frame is the stack frame this symbol was matched against; only set in the
	// intersection, where it is the reason the symbol is in the answer.
	Frame string `json:"frame,omitempty"`
	// Match grades the hit: symbol (the frame line is inside a changed
	// symbol), hunk (no symbol owns the line, a changed hunk covers it) or
	// file (the file changed elsewhere — the weakest answer there is).
	Match string `json:"match,omitempty"`
	// Task is the issue key of the commit that touched it, null when the commit
	// followed no convention.
	Task *string `json:"task"`
}

// BlastRadiusResult puts the intersection first: it is the answer, and
// everything after it is context for when the answer is empty.
type BlastRadiusResult struct {
	Repo    string `json:"repo"`
	Release string `json:"release"`
	Prev    string `json:"prev"`
	Counts  Counts `json:"counts"`
	// Intersection is the symbols the release changed that the stack trace
	// also names. Empty with frames given is itself an answer: this release
	// did not touch the code that broke.
	Intersection []ChangedSymbol `json:"intersection"`
	Changed      []ChangedSymbol `json:"changed,omitempty"`
	Note         string          `json:"note,omitempty"`
}

// Counts is the shape of the range before anything is read.
type Counts struct {
	Commits int `json:"commits"`
	Files   int `json:"files"`
	// Symbols counts what was resolved, not what changed: with frames given only
	// the files a frame points into are resolved to symbols. Files is the honest
	// "how big was this release" number.
	Symbols      int `json:"symbols"`
	Intersection int `json:"intersection"`
	Frames       int `json:"frames"`
	// UnmatchedFrames counts frames that named no symbol here — usually a path
	// from another service. Without it an empty intersection reads as "the
	// release is innocent" when the frames may simply never have matched.
	UnmatchedFrames int `json:"unmatched_frames"`
	// WeakMatches counts intersection rows with match "file": the release
	// touched the files of the trace, not the lines. Three of ten innocent
	// releases in the control group ended up here.
	WeakMatches int `json:"weak_matches"`
}

var blastRadiusInputSchema = mcp.SchemaFor(BlastRadiusArgs{})

// blastRadius answers blast_radius.
func (s ToolsService) blastRadius(ctx context.Context, arguments map[string]any, rec *auditRecord) mcp.ToolCallResult {
	var args BlastRadiusArgs
	if err := mcp.DecodeArgs(arguments, &args); err != nil {
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	rec.Target, rec.Intent = args.Repo, args.Intent
	rec.Path = strings.Join(args.Frames, " ")

	repo, toolErr := s.codeAccess(ctx, ToolBlastRadius, args.Repo)
	if toolErr != nil {
		return refuse(rec, *toolErr)
	}

	res, err := s.code.Blast(ctx, repo, code.BlastQuery{
		Release: args.Release, Prev: args.Prev,
		Frames: args.Frames, MaxSymbols: args.MaxSymbols,
	})
	if err != nil {
		return refuse(rec, s.codeError(err, args.Repo, ""))
	}
	rec.Method = res.Release
	// Counts.Intersection is taken before the max_symbols cut.
	rec.Truncated = len(res.Intersection) < res.Counts.Intersection

	return okResultJSON(newBlastResult(args.Repo, res), s.env)
}

// newBlastResult wraps the answer and says in words what the numbers mean. The
// notes live here rather than in the domain: they are written for the model.
func newBlastResult(repo string, b *code.Blast) BlastRadiusResult {
	out := BlastRadiusResult{
		Repo: repo, Release: b.Release, Prev: b.Prev,
		Counts: Counts{
			Commits: b.Counts.Commits, Files: b.Counts.Files, Symbols: b.Counts.Symbols,
			Intersection: b.Counts.Intersection, Frames: b.Counts.Frames,
			UnmatchedFrames: b.Counts.UnmatchedFrames, WeakMatches: b.Counts.WeakMatches,
		},
		Intersection: mcp.Map(b.Intersection, newChangedSymbol),
		Changed:      mcp.Map(b.Changed, newChangedSymbol),
	}

	switch {
	case b.EmptyRange:
		// A known release with nothing in it is a different answer from an
		// unknown release, and both are useful.
		out.Note = "между prev и release нет коммитов — этот релиз ничего не менял"
	case b.Counts.Frames == 0:
		out.Note = "кадры стектрейса не переданы, поэтому ответ — весь список изменённого. " +
			"Передайте frames как path:line, и останутся единицы символов"
	case len(out.Intersection) == 0:
		out.Note = "пересечения нет: этот релиз не трогал код из кадров стектрейса. " +
			"Проверьте, что пути кадров — репозиторные, и что prev действительно предыдущий выкат"
	case b.Counts.WeakMatches == b.Counts.Intersection:
		out.Note = "все совпадения слабые (match: file): релиз менял эти файлы, но не строки, на которые указывают кадры. " +
			"Это не причина — проверьте why по строке кадра и ищите в другом месте"
	}
	return out
}

func newChangedSymbol(s code.Symbol) ChangedSymbol {
	c := ChangedSymbol{
		Path: s.Path, Symbol: s.Name, Receiver: s.Receiver, Kind: s.Kind,
		Line: s.Line, EndAt: s.EndLine, Change: s.Change,
		Layer: s.Layer, Frame: s.Frame, Match: s.Match,
	}
	if s.Task != "" {
		task := s.Task
		c.Task = &task
	}
	return c
}

// blastRadiusDescription is deliberately short: the tool surface is paid for on
// every request, and how to read the answer lives in ringsrv://tools/code.md.
func (s ToolsService) blastRadiusDescription() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · символы, изменённые между prev и release, пересечённые с кадрами стектрейса (frames: path:line). ", strings.ToUpper(s.env))
	b.WriteString(repoHint + "; prev спросите у Sentry через api_call. Пустое пересечение — ответ «релиз не трогал упавший код», смотрите unmatched_frames; match: symbol|hunk|file — сила совпадения. ")
	b.WriteString(codeSheetHint)
	return b.String()
}
