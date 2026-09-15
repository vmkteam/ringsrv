// Package code answers questions about a repository at a commit: what a file
// says, who touched a line, who calls a symbol, what a release changed and why
// the code is the way it is.
//
// It orchestrates two clients — the git mirrors and the AST engine — and the
// catalogue entry of the repository. What it does not know is who is asking:
// roles, tool names, error codes and the wording an LLM reads live above it.
package code

import (
	"context"
	"errors"
	"slices"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
)

// Manager is the entry point. Both clients may be nil on an instance that runs
// without code tools or without the engine; Available and HasEngine say which.
type Manager struct {
	repos *git.Store
	graph *codegraph.Client
	// upstream is how why reaches the issue tracker — the same client api_call
	// uses, here only for the one request the why chain ends with.
	upstream *upstream.Client
}

// NewManager wires the manager to its clients.
func NewManager(repos *git.Store, graph *codegraph.Client, up *upstream.Client) *Manager {
	return &Manager{repos: repos, graph: graph, upstream: up}
}

// Available reports whether this instance can answer about code at all.
func (s *Manager) Available() bool { return s != nil && s.repos != nil }

// HasEngine reports whether the AST engine is configured. Without it the
// symbol-level questions — who calls this, what did the release change — have
// no answer, and saying so beats answering them badly.
func (s *Manager) HasEngine() bool { return s != nil && s.graph != nil }

// Repo is a repository as this package needs it: the name the mirror is keyed
// by, and the catalogue entry that says what may be handed out of it.
type Repo struct {
	Name string
	Cfg  *target.Repo
}

// Excluded reports whether the catalogue keeps this path out of answers.
func (r Repo) Excluded(path string) bool { return r.Cfg.Excluded(path) }

// resolve turns whatever the caller passed as a ref into a full SHA.
func (s *Manager) resolve(ctx context.Context, repo Repo, ref string) (string, error) {
	shas, err := s.resolveAll(ctx, repo, ref)
	if err != nil {
		return "", err
	}
	return shas[0], nil
}

// resolveAll resolves several refs against one mirror, fetching once if an
// object is unknown: a mirror that has not caught up is the normal state during
// the first minutes of an incident. Together, because the fetch is the expensive
// part and a range asks about two commits.
func (s *Manager) resolveAll(ctx context.Context, repo Repo, refs ...string) ([]string, error) {
	if slices.Contains(refs, "") {
		return nil, ErrRefRequired
	}

	// No pre-emptive fetch: a fetch of a 700 MB mirror is hundreds of
	// milliseconds on every code call, while the webhook and the cron keep the
	// SHA there already. The miss below pays for freshness when it is missing.
	spec := repo.Cfg.Spec(repo.Name)
	if !s.repos.Have(repo.Name) {
		if err := s.repos.Ensure(ctx, spec); err != nil {
			return nil, err
		}
	}

	out := make([]string, len(refs))
	for i, ref := range refs {
		sha, err := s.repos.ResolveSHA(ctx, repo.Name, ref)
		if errors.Is(err, git.ErrNotFound) {
			if ferr := s.repos.Fetch(ctx, spec); ferr == nil {
				sha, err = s.repos.ResolveSHA(ctx, repo.Name, ref)
			}
		}
		if err != nil {
			return nil, err
		}
		out[i] = sha
	}
	return out, nil
}

// indexedTree checks out the commit and makes sure it is indexed. Both steps
// are single-flighted by their own clients, so ten questions about one release
// produce one checkout and one index.
func (s *Manager) indexedTree(ctx context.Context, repo, sha string) (string, func(), error) {
	dir, release, err := s.repos.Worktree(ctx, repo, sha)
	if err != nil {
		return "", nil, err
	}
	if err := s.graph.Ensure(ctx, dir); err != nil {
		release()
		return "", nil, err
	}
	return dir, release, nil
}

// Commit is a commit as the answers carry it: the git fields plus the issue key
// read by the repository's own convention.
type Commit struct {
	SHA     string
	Short   string
	Subject string
	Author  string
	Date    string
	Files   []string
	// Task is empty when the subject carries no issue key: an answer, not a
	// failure to look.
	Task string
}

// newCommit converts a git commit, reading the issue key and dropping the paths
// this repository never hands out: a commit that touched a key must not name it
// here just because it arrived by another route.
func newCommit(repo Repo, c git.Commit) Commit {
	return Commit{
		SHA: c.SHA, Short: c.Short, Subject: c.Subject,
		Author: c.Author, Date: c.Date,
		Files: keepAllowed(repo, c.Files),
		Task:  repo.Cfg.TaskID(c.Subject),
	}
}

// keepAllowed drops the paths this repository never hands out.
func keepAllowed(repo Repo, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	res := make([]string, 0, len(paths))
	for _, p := range paths {
		if !repo.Excluded(p) {
			res = append(res, p)
		}
	}
	return res
}
