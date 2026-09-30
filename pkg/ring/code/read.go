package code

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/git"

	"golang.org/x/sync/errgroup"
)

// defaultMaxLines caps a read. Two hundred lines is a long function and a
// short file; more than that and the answer stops being an answer.
const defaultMaxLines = 200

const defaultMaxMatches = 50

// searchConcurrency caps the greps in flight: each is a git process on a mirror,
// and eight hides the latency of a handful of repositories without turning a
// search into a fork bomb.
const searchConcurrency = 8

// Window is the slice of a file the caller asked for. A zero window means the
// whole file, capped by Max.
type Window struct {
	From int
	To   int
	Max  int
}

// File is a window of a file at a commit. The lines come without numbers:
// numbering belongs to whoever formats the answer.
type File struct {
	Ref       string
	Path      string
	From      int
	To        int
	Total     int
	Truncated bool
	Lines     []string
}

// Read returns a window of a file at a commit.
func (s *Manager) Read(ctx context.Context, repo Repo, ref, path string, w Window) (*File, error) {
	if repo.Excluded(path) {
		return nil, ErrPathDenied
	}

	sha, err := s.resolve(ctx, repo, ref)
	if err != nil {
		return nil, err
	}

	body, err := s.repos.Blob(ctx, repo.Name, sha, path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	out := sliceLines(lines, w)
	out.Ref, out.Path = sha, path
	return out, nil
}

// sliceLines cuts the requested window.
func sliceLines(lines []string, w Window) *File {
	total := len(lines)
	from, to := w.From, w.To
	if from < 1 {
		from = 1
	}
	if to <= 0 || to > total {
		to = total
	}
	if from > total {
		return &File{From: from, To: from, Total: total}
	}

	truncated := false
	if limit := git.LimitOr(w.Max, defaultMaxLines); to-from+1 > limit {
		to = from + limit - 1
		truncated = true
	}
	return &File{From: from, To: to, Total: total, Truncated: truncated, Lines: lines[from-1 : to]}
}

// SearchQuery is what code_search asks for.
type SearchQuery struct {
	Query   string
	Ref     string
	Glob    string
	Regex   bool
	Context int
	Max     int
}

// Match is one line that matched, named by the repository it came from.
type Match struct {
	Repo string
	Path string
	Line int
	Text string
	// Context marks a line shown around a match rather than a match.
	Context bool
}

// SearchResult is the merged answer over every repository asked.
type SearchResult struct {
	Ref       string
	Matches   []Match
	Truncated bool
	// Skipped names the repositories that do not have the commit. One SHA
	// lives in one repository, and a search is usually asked across several.
	Skipped []string
	// Work is how long each repository's part took, a skipped one included —
	// finding out it lacks the commit may have cost a fetch. The parts run at
	// once, so the wall clock of the search is the slowest of them, not the
	// work done.
	Work []RepoWork
}

// RepoWork is the time one repository's part of a search took.
type RepoWork struct {
	Repo string
	Took time.Duration
}

// Search greps every repository given, in parallel — five of them used to mean
// five greps in a row, about a second each. The limit keeps a role with many
// repositories from opening a git process for each at once.
func (s *Manager) Search(ctx context.Context, repos []Repo, q SearchQuery) (*SearchResult, error) {
	if q.Ref == "" {
		return nil, ErrRefRequired
	}

	// One SHA lives in one repository. Resolving against the mirrors first says
	// which of them can have the commit; the rest are reported, not failed on.
	// The fetch is spent only when no mirror knows it — a search across eleven
	// repositories must not cost ten fetches.
	local, anyLocal := s.resolveLocal(ctx, repos, q.Ref)

	type found struct {
		sha       string
		matches   []Match
		truncated bool
		skipped   bool
		took      time.Duration
	}
	results := make([]found, len(repos))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(searchConcurrency)
	for i, repo := range repos {
		g.Go(func() error {
			started := time.Now()
			sha, err := s.searchRef(gctx, repo, q.Ref, local[i], anyLocal)
			if err != nil {
				return err
			}
			if sha == "" {
				results[i] = found{skipped: true, took: time.Since(started)}
				return nil
			}
			matches, truncated, err := s.grep(gctx, repo, sha, q)
			if err != nil {
				return err
			}
			results[i] = found{sha: sha, matches: matches, truncated: truncated, took: time.Since(started)}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Merged in the order the repositories were asked about, so two identical
	// searches read the same way.
	res := &SearchResult{Work: make([]RepoWork, len(repos))}
	for i, r := range results {
		res.Work[i] = RepoWork{Repo: repos[i].Name, Took: r.took}
		if r.skipped {
			res.Skipped = append(res.Skipped, repos[i].Name)
			continue
		}
		res.Ref = r.sha
		res.Truncated = res.Truncated || r.truncated
		res.Matches = append(res.Matches, r.matches...)
	}
	if len(res.Skipped) == len(repos) {
		return nil, fmt.Errorf("%s: %w", q.Ref, git.ErrNotFound)
	}
	return res, nil
}

// resolveLocal answers from the mirrors alone — no clone, no fetch — and
// reports whether any of them knows the commit.
func (s *Manager) resolveLocal(ctx context.Context, repos []Repo, ref string) ([]string, bool) {
	local := make([]string, len(repos))
	known := false
	for i, repo := range repos {
		if !s.repos.Have(repo.Name) {
			continue
		}
		if sha, err := s.repos.ResolveSHA(ctx, repo.Name, ref); err == nil {
			local[i], known = sha, true
		}
	}
	return local, known
}

// searchRef decides what a repository contributes: its SHA, or "" to be skipped.
// A repository without the commit is skipped when another mirror has it; when
// none does, the fetch inside resolve gets its chance first.
func (s *Manager) searchRef(ctx context.Context, repo Repo, ref, local string, anyLocal bool) (string, error) {
	if local != "" {
		return local, nil
	}
	if anyLocal {
		return "", nil
	}
	sha, err := s.resolve(ctx, repo, ref)
	if errors.Is(err, git.ErrNotFound) {
		return "", nil
	}
	return sha, err
}

// grep runs the search in one repository at one commit. Excluded paths are
// filtered after the search: git's pathspec cannot express "everything but".
func (s *Manager) grep(ctx context.Context, repo Repo, sha string, q SearchQuery) ([]Match, bool, error) {
	matches, truncated, err := s.repos.Grep(ctx, repo.Name, sha, git.GrepOptions{
		Pattern: q.Query,
		Regexp:  q.Regex,
		Glob:    q.Glob,
		Context: q.Context,
		Max:     git.LimitOr(q.Max, defaultMaxMatches),
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]Match, 0, len(matches))
	for _, m := range matches {
		if repo.Excluded(m.Path) {
			continue
		}
		out = append(out, Match{Repo: repo.Name, Path: m.Path, Line: m.Line, Text: m.Text, Context: m.Context})
	}
	return out, truncated, nil
}
