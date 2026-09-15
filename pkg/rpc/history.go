package rpc

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/code"

	"github.com/vmkteam/mcpkit/mcp"
)

// ToolCodeHistory answers about the history of a line rather than of a file:
// who touched the lines the stack trace points at, and when.
const ToolCodeHistory = "code_history"

// CodeHistoryArgs are the inputs to code_history. Which fields matter depends
// on mode; the description says which, and a missing one is refused by name.
type CodeHistoryArgs struct {
	Repo string `json:"repo" jsonschema:"required" jsonschema_description:"Repository from repo_map."`
	Mode string `json:"mode" jsonschema:"required,enum=lines,enum=blame,enum=contains,enum=range,enum=pickaxe" jsonschema_description:"See help(code)."`

	Ref  string `json:"ref,omitempty" jsonschema_description:"Commit SHA (lines, blame)."`
	Path string `json:"path,omitempty" jsonschema_description:"File path (lines, blame, range?)."`

	LineFrom int `json:"line_from,omitempty" jsonschema_description:"First line (lines)."`
	LineTo   int `json:"line_to,omitempty" jsonschema_description:"Last line (lines)."`
	Line     int `json:"line,omitempty" jsonschema_description:"Line (blame)."`

	SHA string `json:"sha,omitempty" jsonschema_description:"Commit (contains)."`

	From string `json:"from,omitempty" jsonschema_description:"Range start, exclusive (range, pickaxe)."`
	To   string `json:"to,omitempty" jsonschema_description:"Range end, inclusive (range, pickaxe)."`

	Query string `json:"query,omitempty" jsonschema_description:"String to look for (pickaxe)."`

	WithFiles  bool   `json:"with_files,omitempty" jsonschema_description:"List touched paths (range)."`
	MaxCommits int    `json:"max_commits,omitempty" jsonschema_description:"Default 50 for range, 20 otherwise."`
	Intent     string `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// HistoryCommit is one commit, the same shape whichever mode produced it.
type HistoryCommit struct {
	SHA     string   `json:"sha"`
	Short   string   `json:"short"`
	Subject string   `json:"subject"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	Files   []string `json:"files,omitempty"`
	// Task is null, not absent, when the subject carries no issue key: the
	// model must be able to tell "no task" from "nobody looked".
	Task *string `json:"task"`
}

// CodeHistoryResult is the answer of every mode. contains fills Branches and
// Tags instead of Commits — the question it answers is about refs — and the
// rest of the envelope stays the same so the caller reads one shape.
type CodeHistoryResult struct {
	Repo     string          `json:"repo"`
	Mode     string          `json:"mode"`
	Ref      string          `json:"ref,omitempty"`
	Commits  []HistoryCommit `json:"commits"`
	Branches []string        `json:"branches,omitempty"`
	Tags     []string        `json:"tags,omitempty"`
	// Note says what the answer could not cover, e.g. that this mirror carries
	// no tags — otherwise an empty list reads as "released nowhere".
	Note string `json:"note,omitempty"`
}

var codeHistoryInputSchema = mcp.SchemaFor(CodeHistoryArgs{})

// history answers code_history.
func (s ToolsService) history(ctx context.Context, arguments map[string]any, rec *auditRecord) mcp.ToolCallResult {
	var args CodeHistoryArgs
	if err := mcp.DecodeArgs(arguments, &args); err != nil {
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	rec.Target, rec.Method, rec.Intent = args.Repo, args.Mode, args.Intent
	rec.Path = args.Path
	if args.Mode == code.ModePickaxe {
		rec.Path = args.Query
	}

	repo, toolErr := s.codeAccess(ctx, ToolCodeHistory, args.Repo)
	if toolErr != nil {
		return refuse(rec, *toolErr)
	}

	res, err := s.code.History(ctx, repo, code.HistoryQuery{
		Mode:      args.Mode,
		Ref:       args.Ref,
		Path:      args.Path,
		LineFrom:  args.LineFrom,
		LineTo:    args.LineTo,
		Line:      args.Line,
		SHA:       args.SHA,
		From:      args.From,
		To:        args.To,
		Query:     args.Query,
		WithFiles: args.WithFiles,
		Max:       args.MaxCommits,
	})
	if err != nil {
		return refuse(rec, s.codeError(err, args.Repo, args.Path))
	}

	out := CodeHistoryResult{
		Repo: args.Repo, Mode: res.Mode, Ref: res.Ref,
		Commits:  mcp.Map(res.Commits, newHistoryCommit),
		Branches: res.Branches, Tags: res.Tags,
	}
	if res.NoTags {
		// D10: tags are not mirrored, and an empty list here would otherwise be
		// read as "this commit is in no release".
		out.Note = "теги в этом зеркале не хранятся — список веток полон, список тегов всегда пуст"
	}
	return okResultJSON(out, s.env)
}

// newHistoryCommit converts a commit for the wire. The issue key becomes a
// pointer here and nowhere else: JSON needs null to say "this commit carries
// none", while the domain says the same thing with an empty string.
func newHistoryCommit(c code.Commit) HistoryCommit {
	h := HistoryCommit{
		SHA: c.SHA, Short: c.Short, Subject: c.Subject,
		Author: c.Author, Date: c.Date, Files: c.Files,
	}
	if c.Task != "" {
		task := c.Task
		h.Task = &task
	}
	return h
}

func (s ToolsService) codeHistoryDescription() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · история строк, не файла. Режимы: lines (path, line_from, line_to) · blame (path, line) · contains (sha) · range (from, to, path?) · pickaxe (from, to, query). ", strings.ToUpper(s.env))
	b.WriteString(repoHint + "; task у коммита — ключ задачи из subject или null.")
	return b.String()
}
