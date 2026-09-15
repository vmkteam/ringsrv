// Package git runs git and keeps the repositories on disk: a clone per
// repository and a worktree per commit, so every question about code is answered
// against the commit that is actually deployed. It knows how to drive an
// external tool and nothing about catalogues, roles or tools.
//
// Nothing the model writes reaches git's argv: each mode builds a fixed command,
// the subcommand comes from a closed list, refs must look like a SHA or a branch
// name, and paths are checked before they are passed after "--". `git -c`, an
// alias like `!sh`, `--upload-pack` and `--exec` all run arbitrary programs.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// allowedSubcommands is the whole surface of git this service uses. Anything
// outside the list is a programming error, not a configuration one.
var allowedSubcommands = map[string]bool{
	"version":   true,
	"clone":     true,
	"fetch":     true,
	"rev-parse": true,
	"worktree":  true,
	"cat-file":  true,
	"grep":      true,
	"log":       true,
	"blame":     true,
	"branch":    true,
	"tag":       true,
	"diff":      true,
}

// Errors callers distinguish.
var (
	// ErrNotFound means the object is not in this clone. The caller fetches once
	// and then falls back to the GitLab API.
	ErrNotFound = errors.New("git: object not found")
	// ErrAmbiguous means a short SHA matches several objects — four characters
	// already matched five in the measured repository, and "not found" would
	// send the investigation down the wrong path.
	ErrAmbiguous = errors.New("git: ambiguous short sha")
	// ErrBadRef and ErrBadPath are refusals to build a command at all.
	ErrBadRef  = errors.New("git: bad ref")
	ErrBadPath = errors.New("git: bad path")
)

// shaRe is what a ref may look like when it is not a known branch name.
var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// branchRe is deliberately narrow: no spaces, no dashes at the start, nothing
// that could be read as an option.
var branchRe = regexp.MustCompile(`^[\w][\w./-]{0,100}$`)

// ValidateRef accepts a SHA or a plain branch name and rejects everything that
// could be mistaken for an option.
func ValidateRef(ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("%w: empty", ErrBadRef)
	case strings.HasPrefix(ref, "-"):
		return fmt.Errorf("%w: %q looks like an option", ErrBadRef, ref)
	case shaRe.MatchString(ref), branchRe.MatchString(ref):
		return nil
	default:
		return fmt.Errorf("%w: %q is neither a sha nor a branch name", ErrBadRef, ref)
	}
}

// ValidatePath rejects paths that leave the tree or read as an option. The
// Exclude list of the repository is checked separately, by the caller that
// knows it.
func ValidatePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty", ErrBadPath)
	case strings.HasPrefix(p, "-"):
		return fmt.Errorf("%w: %q looks like an option", ErrBadPath, p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w: %q must be repository-relative", ErrBadPath, p)
	case strings.Contains(p, ".."):
		return fmt.Errorf("%w: %q contains a parent-directory reference", ErrBadPath, p)
	case strings.ContainsAny(p, "\x00\n"):
		return fmt.Errorf("%w: %q contains a control character", ErrBadPath, p)
	default:
		return nil
	}
}

// run executes one git command in dir and returns stdout. The environment is
// inherited, with three variables set: no credential prompt, because a missing
// token would otherwise hang until the timeout, and no system or global config,
// because an alias like `!sh` from a stray config is the arbitrary-code path
// the rest of this package closes.
func (s *Store) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return s.runEnv(ctx, dir, nil, args...)
}

// runEnv is run with extra environment variables — the way a clone token
// reaches git without touching argv or the clone's config (see authEnv).
func (s *Store) runEnv(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	if len(args) == 0 || !allowedSubcommands[args[0]] {
		return nil, fmt.Errorf("git: subcommand %q is not allowed", first(args))
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, s.opts.Git, args...) //nolint:gosec // argv is built here, never from a caller
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	cmd.Env = append(cmd.Env, env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git: git %s timed out after %s", args[0], s.timeout())
		}
		return nil, fmt.Errorf("git: git %s: %w: %s", args[0], err, clip(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (s *Store) timeout() time.Duration {
	if s.opts.Timeout > 0 {
		return s.opts.Timeout
	}
	return DefaultTimeout
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// clip keeps a git error readable in a log line. Stderr of a failed clone can
// be pages long, and none of it after the first lines is news.
func clip(s string) string {
	s = strings.TrimSpace(s)
	const limit = 300
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// Check reports whether git is usable at all. Called at startup, so a missing
// binary is a boot error rather than a confusing failure on the first question.
func (s *Store) Check(ctx context.Context) error {
	out, err := s.run(ctx, "", "version")
	if err != nil {
		return fmt.Errorf("git: git is required but not usable: %w", err)
	}
	s.version = strings.TrimSpace(string(out))
	return nil
}

// Version is what `git version` answered at startup, for the status endpoint.
func (s *Store) Version() string { return s.version }

// classify maps git's stderr onto the errors the domain distinguishes. One list
// for the whole package: three lists in three call sites each missed a wording
// the others caught, and an unmatched message reads as "the server broke"
// instead of "no such commit".
func classify(err error, what string) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())

	// "is ambiguous" only: "ambiguous argument 'x'" is git's wording for an
	// unknown revision, not for a short SHA matching two objects. Under --verify,
	// which is how this package resolves refs, git says "Needed a single
	// revision" for both, so ErrAmbiguous is reachable from other commands only.
	if strings.Contains(text, "is ambiguous") {
		return fmt.Errorf("%w: %q", ErrAmbiguous, what)
	}
	for _, s := range []string{
		"unknown revision",
		"needed a single revision",
		"not a valid object name",
		"invalid object name",
		"does not exist",
		"no such path",
		"there is no path",
		"exists on disk, but not in",
		"bad revision",
	} {
		if strings.Contains(text, s) {
			return fmt.Errorf("%w: %s", ErrNotFound, what)
		}
	}
	return err
}
