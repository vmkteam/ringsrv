package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/code"

	"github.com/vmkteam/mcpkit/mcp"
)

// ToolWhy answers "why is this code like this" — a question the code cannot
// answer. Every link of the chain already exists: the commit convention, the
// branch named after the task, the artefacts left in comments. Nothing walked
// it automatically.
const ToolWhy = "why"

// WhyArgs are the inputs to why. Either a line or a symbol identifies the code.
type WhyArgs struct {
	Repo   string `json:"repo" jsonschema:"required" jsonschema_description:"Repository from repo_map."`
	Ref    string `json:"ref" jsonschema:"required" jsonschema_description:"Commit SHA."`
	Path   string `json:"path" jsonschema:"required" jsonschema_description:"Repo-relative path."`
	Line   int    `json:"line,omitempty" jsonschema_description:"Line; or give symbol."`
	Symbol string `json:"symbol,omitempty" jsonschema_description:"Symbol instead of line."`
	Intent string `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// WhyCommit is the commit that last touched the line.
type WhyCommit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// WhyTask is what the issue tracker knows. Absent rather than empty when the
// tracker was not reachable — see Enrichment.
type WhyTask struct {
	ID          string   `json:"id"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Artifacts   []string `json:"artifacts,omitempty"`
}

// WhyResult is the answer of why.
type WhyResult struct {
	// Reference is "repo path:line@sha". Without it an answer about code is a
	// rumour: nothing else in the answer can be checked.
	Reference string    `json:"reference"`
	Commit    WhyCommit `json:"commit"`
	// Previous is filled when the blamed commit only reformatted the line: one
	// is who touched it last, the other who wrote it.
	Previous *WhyCommit `json:"previous,omitempty"`
	// Task is null when the commit carries no issue key — a valid answer.
	Task *WhyTask `json:"task"`
	// Enrichment says what happened when the tracker was asked: "ok",
	// "no-access", "unavailable" or "no-task". Without it a missing summary is
	// indistinguishable from an issue with no summary.
	Enrichment string `json:"enrichment"`
}

var whyInputSchema = mcp.SchemaFor(WhyArgs{})

// Enrichment outcomes.
const (
	enrichOK          = "ok"
	enrichNoTask      = "no-task"
	enrichNoAccess    = "no-access"
	enrichUnavailable = "unavailable"
)

// why answers the why tool.
func (s ToolsService) why(ctx context.Context, arguments map[string]any, rec *auditRecord) mcp.ToolCallResult {
	var args WhyArgs
	if err := mcp.DecodeArgs(arguments, &args); err != nil {
		return refuse(rec, ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	rec.Target, rec.Path, rec.Intent = args.Repo, args.Path, args.Intent

	repo, toolErr := s.codeAccess(ctx, ToolWhy, args.Repo)
	if toolErr != nil {
		return refuse(rec, *toolErr)
	}

	chain, err := s.code.Why(ctx, repo, code.WhyQuery{
		Ref: args.Ref, Path: args.Path, Line: args.Line, Symbol: args.Symbol,
	})
	if err != nil {
		return refuse(rec, s.codeError(err, args.Repo, args.Path))
	}
	rec.Method = chain.Ref

	out := WhyResult{
		Reference: fmt.Sprintf("%s %s:%d@%s", args.Repo, chain.Path, chain.Line, chain.Ref),
		Commit:    newWhyCommit(chain.Commit),
	}
	if chain.Previous != nil {
		p := newWhyCommit(*chain.Previous)
		out.Previous = &p
	}
	if chain.TaskID == "" {
		out.Enrichment = enrichNoTask
		return okResultJSON(out, s.env)
	}
	out.Task = &WhyTask{ID: chain.TaskID}

	// Enrichment is best-effort by design: an unreachable tracker must cost
	// the caller the summary, not the answer.
	task, status := s.issue(ctx, repo, chain.TaskID)
	out.Enrichment = status
	if task != nil {
		out.Task = task
	}
	return okResultJSON(out, s.env)
}

func newWhyCommit(c code.Commit) WhyCommit {
	return WhyCommit{SHA: c.SHA, Short: c.Short, Subject: c.Subject, Author: c.Author, Date: c.Date}
}

// issue resolves which tracker this caller may read and asks the domain for the
// task. No access means no enrichment, not a privileged read on their behalf.
func (s ToolsService) issue(ctx context.Context, repo code.Repo, id string) (*WhyTask, string) {
	name := repo.Cfg.IssueTarget
	if name == "" {
		return nil, enrichNoAccess
	}
	acc, err := s.access(ctx)
	if err != nil {
		return nil, enrichNoAccess
	}
	profile, ok := s.targets.Profile(name)
	if !ok || !acc.HasTarget(name) {
		return nil, enrichNoAccess
	}

	issue, err := s.code.Issue(ctx, profile, repo, id)
	switch {
	case errors.Is(err, code.ErrNoTracker):
		return nil, enrichNoAccess
	case err != nil:
		return nil, enrichUnavailable
	}
	return &WhyTask{
		ID:          issue.ID,
		Summary:     issue.Summary,
		Description: issue.Description,
		Artifacts:   issue.Artifacts,
	}, enrichOK
}

func (s ToolsService) whyDescription() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · почему код такой: коммит строки (path+line или path+symbol), задача из subject, артефакты из трекера. ", strings.ToUpper(s.env))
	b.WriteString(repoHint + "; enrichment: ok | no-task | no-access | unavailable.")
	return b.String()
}
