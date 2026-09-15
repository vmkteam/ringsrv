package code

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/client/tracker"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// reformatRe recognises a commit that only moved text around: blame reports the
// last change of a line, and a formatting pass overwrites the authorship of
// everything it touched.
//
// The words are narrow on purpose. Matching "fmt", "format" and "rename" on
// their own caught "wrap with fmt.Errorf" and "rename column in migration", and
// the answer then named the commit before the one that wrote the behaviour.
var reformatRe = regexp.MustCompile(`(?i)\b(gofmt|go fmt|goimports|reformat(?:ting)?|formatting only|lint(?:er)?|golangci|whitespace|typo|rename (?:var|variable|param|package|file))\b`)

// reformatHides reports whether blame stopped at a commit that only moved text.
// A commit with an issue key is the answer whatever its subject says: it was
// done for that task, and the word "lint" in it changes nothing.
func reformatHides(subject, taskID string) bool {
	return taskID == "" && reformatRe.MatchString(subject)
}

// IssueTextBytes caps the summary and the description an issue contributes to
// an answer, each. A description can be a pasted log of a megabyte, and the
// answer is about one line of code.
const IssueTextBytes = 8 << 10

// WhyQuery identifies the code to explain, by line or by symbol.
type WhyQuery struct {
	Ref    string
	Path   string
	Line   int
	Symbol string
}

// Why is the chain from a line to the commit that wrote it. The issue itself is
// fetched a layer up: which tracker the caller may read is a question about
// their role.
type Why struct {
	Ref  string
	Path string
	Line int
	// Commit is what blame reports for the line.
	Commit Commit
	// Previous is filled when the blamed commit only reformatted the line. Both
	// are kept: one is who touched it last, the other is who wrote it.
	Previous *Commit
	// TaskID is the issue key of whichever commit is the answer, empty when it
	// carries none.
	TaskID string
}

// Why walks from a line to the commit that explains it.
func (s *Manager) Why(ctx context.Context, repo Repo, q WhyQuery) (*Why, error) {
	if repo.Excluded(q.Path) {
		return nil, ErrPathDenied
	}
	if q.Line <= 0 && q.Symbol == "" {
		return nil, ErrLineOrSymbol
	}

	sha, err := s.resolve(ctx, repo, q.Ref)
	if err != nil {
		return nil, err
	}

	line, err := s.whyLine(ctx, repo, q, sha)
	if err != nil {
		return nil, err
	}

	blamed, err := s.repos.Blame(ctx, repo.Name, sha, q.Path, line)
	if err != nil {
		return nil, err
	}

	out := &Why{Ref: sha, Path: q.Path, Line: line, Commit: newCommit(repo, *blamed)}
	out.TaskID = out.Commit.Task
	// A formatting commit without a key of its own hides the one that wrote
	// the behaviour.
	if reformatHides(blamed.Subject, out.Commit.Task) {
		if prev := s.previousTouch(ctx, repo, sha, q.Path, line, blamed.SHA); prev != nil {
			p := newCommit(repo, *prev)
			out.Previous = &p
			out.TaskID = p.Task
		}
	}
	return out, nil
}

// whyLine turns whichever of line/symbol was given into a line number.
func (s *Manager) whyLine(ctx context.Context, repo Repo, q WhyQuery, sha string) (int, error) {
	if q.Line > 0 {
		return q.Line, nil
	}

	// An index that already exists answers exactly and costs nothing; building
	// one to learn a single line number does not. An unindexed commit is
	// answered by grep: this is a place to start a blame, not a claim about types.
	if s.HasEngine() {
		if dir := s.repos.WorktreePath(repo.Name, sha); s.graph.Indexed(dir) {
			if node, err := s.graph.Node(ctx, dir, q.Symbol, q.Path); err == nil {
				return node.StartLine, nil
			}
		}
	}

	line, err := s.repos.Definition(ctx, repo.Name, sha, q.Path, q.Symbol)
	if err != nil {
		if errors.Is(err, git.ErrNotFound) {
			return 0, &SymbolNotFoundError{Symbol: q.Symbol, Path: q.Path, Ref: sha}
		}
		return 0, err
	}
	return line, nil
}

// previousTouch finds the commit before the blamed one that changed this line.
// Best-effort: failing to find it costs a field, not the answer.
func (s *Manager) previousTouch(ctx context.Context, repo Repo, sha, path string, line int, blamedSHA string) *git.Commit {
	commits, err := s.repos.LogLines(ctx, repo.Name, sha, path, line, line, 5)
	if err != nil {
		return nil
	}
	for i := range commits {
		if commits[i].SHA != blamedSHA {
			return &commits[i]
		}
	}
	return nil
}

// Issue asks the tracker what the task says. The profile arrives already
// checked; what belongs here is the request and the reading of the answer, so
// the API layer never talks to a client directly. Best-effort: an unreachable
// tracker costs the summary, not the answer.
func (s *Manager) Issue(ctx context.Context, prof *target.Profile, repo Repo, id string) (*tracker.Issue, error) {
	if s.upstream == nil {
		return nil, ErrNoTracker
	}
	path := repo.Cfg.IssueRequest(id)
	if !prof.Allows(http.MethodGet, path) || prof.CheckQuery(path) != nil {
		return nil, ErrNoTracker
	}

	resp, err := s.upstream.Do(ctx, prof.Upstream(), upstream.Request{Method: http.MethodGet, Path: path})
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("%w: tracker answered %d", ErrTrackerDown, resp.Status)
	}

	issue := tracker.Parse(resp.Body, id)
	if issue == nil {
		return nil, fmt.Errorf("%w: tracker answered something that is not an issue", ErrTrackerDown)
	}
	// The tracker's text reaches the model like any upstream body, so it takes
	// the same two steps: the profile's redaction and a size cap.
	issue.Summary = shapeIssueText(issue.Summary, prof)
	issue.Description = shapeIssueText(issue.Description, prof)
	return issue, nil
}

func shapeIssueText(s string, prof *target.Profile) string {
	s, _ = redact.Text(s, redact.Options{Mode: prof.Redact, Rules: prof.RedactRules})
	s, _, _ = mcp.Truncate(s, IssueTextBytes)
	return s
}
