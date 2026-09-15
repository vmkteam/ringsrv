package git

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// DefaultTimeout caps a single git invocation. It is for the case where
// something hangs on the network, not for the honest slow path.
const DefaultTimeout = 5 * time.Minute

// Spec is what the catalogue knows about a repository, reduced to what this
// package needs. The conversion lives in Repo.Spec, so this package stays
// unaware of roles, tools and TOML.
type Spec struct {
	Name          string
	CloneURL      string
	CloneToken    string
	DefaultBranch string
	Refspec       []string
	// Tags is false for every repository we have: tags inflated the Loki mirror
	// to 20102 refs against one, and no question we ask needs them.
	Tags bool
}

// Options configure the store.
type Options struct {
	ReposDir     string
	WorktreesDir string
	// MaxDiskBytes caps worktrees, not clones: a clone is the price of having
	// the repository at all, a worktree is a cache entry.
	MaxDiskBytes int64
	// Git is the binary to run; empty means "git" from PATH.
	Git     string
	Timeout time.Duration
}

// Store owns the clones and the worktrees.
type Store struct {
	opts Options

	// single-flight over (repo, sha) for checkouts and over the name for
	// fetches: two questions about one commit must produce one checkout.
	group singleflight.Group

	mu sync.Mutex
	// inUse counts worktrees currently being read, so the LRU never deletes the
	// tree somebody is reading right now.
	inUse map[string]int
	// lastUsed drives the eviction order.
	lastUsed map[string]time.Time
	// lastFetch is when each mirror last caught up: the staleness metric, and
	// the first thing to look at when blast_radius says a commit does not exist.
	lastFetch map[string]time.Time
	now       func() time.Time

	version string

	// onCreate fires when a worktree is actually checked out. Tests use it to
	// prove that ten concurrent questions produce one checkout.
	onCreate func()
}

// New builds a store; directories are created on demand. The paths are made
// absolute here, because git runs with its working directory set to the clone.
func New(opts Options) *Store {
	if opts.Git == "" {
		opts.Git = "git"
	}
	opts.ReposDir = absPath(opts.ReposDir)
	opts.WorktreesDir = absPath(opts.WorktreesDir)
	return &Store{
		opts:      opts,
		inUse:     map[string]int{},
		lastUsed:  map[string]time.Time{},
		lastFetch: map[string]time.Time{},
		now:       time.Now,
	}
}

// ClonePath is where the repository itself lives.
func (s *Store) ClonePath(name string) string {
	return filepath.Join(s.opts.ReposDir, name+".git")
}

// WorktreePath is where a given commit is checked out. Stable across restarts on
// purpose: an AST index keyed by path stays valid, which makes it a
// content-addressed cache rather than a rebuild.
func (s *Store) WorktreePath(name, sha string) string {
	return filepath.Join(s.opts.WorktreesDir, name, sha)
}

// Ensure makes sure the clone exists and is reasonably fresh: it clones on the
// first call and fetches afterwards.
func (s *Store) Ensure(ctx context.Context, spec Spec) error {
	if s.Have(spec.Name) {
		if err := s.scrubCredentials(spec.Name); err != nil {
			return err
		}
		return s.Fetch(ctx, spec)
	}
	// Single-flighted: two first questions about a repository used to race into
	// two clones, and the second died on "already exists". The check inside is
	// for the caller that arrives just after the winner finished.
	_, err := s.once(ctx, "clone:"+spec.Name, func(ctx context.Context) (string, error) {
		if s.Have(spec.Name) {
			return "", nil
		}
		return "", s.clone(ctx, spec)
	})
	return err
}

// SetMaxDiskBytes changes the eviction budget. It exists for tests and for a
// future reload; production sets it once through Options.
func (s *Store) SetMaxDiskBytes(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.MaxDiskBytes = n
}

// Have reports whether the mirror is on disk. Callers that only need the clone
// to exist use this instead of Ensure: the webhook and the cron keep it current
// anyway, and a fetch of a large mirror is a network round trip.
func (s *Store) Have(name string) bool {
	_, err := os.Stat(filepath.Join(s.ClonePath(name), "HEAD"))
	return err == nil
}

func (s *Store) clone(ctx context.Context, spec Spec) error {
	if err := os.MkdirAll(s.opts.ReposDir, 0o750); err != nil {
		return fmt.Errorf("git: create repos dir: %w", err)
	}

	args := []string{"clone", "--bare"}
	if !spec.Tags {
		args = append(args, "--no-tags")
	}
	// Neither --depth nor --filter=blob:none: the first throws away the history
	// this whole thing exists for, the second fetches blobs on every grep.
	args = append(args, spec.CloneURL, s.ClonePath(spec.Name))

	if _, err := s.runEnv(ctx, s.opts.ReposDir, authEnv(spec), args...); err != nil {
		return err
	}
	// A bare clone follows only the default branch; we want every head, because
	// a hotfix deployed from a branch has to stay reachable.
	if err := s.setFetchRefspec(spec); err != nil {
		return err
	}
	// The clone wrote refs/heads/* once and will never update them — the refspec
	// points fetches at refs/remotes/origin/*. One fetch right away creates
	// those, so a branch name resolves to something that moves.
	return s.Fetch(ctx, spec)
}

// authEnv hands git the clone token through its config environment rather than
// the URL: a token in the URL is written into the clone's own config and stays
// on the volume after rotation, and one in argv is visible in the process list.
func authEnv(spec Spec) []string {
	if spec.CloneToken == "" {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("oauth2:" + spec.CloneToken))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// credentialURLRe finds a remote URL that carries userinfo, which is what an
// earlier version of this package left behind.
var credentialURLRe = regexp.MustCompile(`(?m)^(\s*url = https?://)[^@\s/]+@`)

// scrubCredentials removes a token a previous version put into the clone's
// config. Idempotent and cheap, so it runs on every Ensure of an existing clone.
func (s *Store) scrubCredentials(name string) error {
	cfg := filepath.Join(s.ClonePath(name), "config")
	b, err := os.ReadFile(cfg)
	if err != nil {
		return fmt.Errorf("git: read clone config: %w", err)
	}
	if !credentialURLRe.Match(b) {
		return nil
	}
	out := credentialURLRe.ReplaceAll(b, []byte("$1"))
	return os.WriteFile(cfg, out, 0o600) //nolint:gosec // G703: the path is Storage.ReposDir plus a catalogue name the validator restricts
}

// absPath keeps a relative directory from being resolved against git's working
// directory, which is the clone rather than the process's own.
func absPath(p string) string {
	if p == "" {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// setFetchRefspec writes the refspec into the clone's config file directly.
// Shelling out to `git config` would mean allowing a subcommand that takes
// arbitrary keys and values, and this is the only setting we ever change.
func (s *Store) setFetchRefspec(spec Spec) error {
	cfg := filepath.Join(s.ClonePath(spec.Name), "config")
	b, err := os.ReadFile(cfg)
	if err != nil {
		return fmt.Errorf("git: read clone config: %w", err)
	}
	out := strings.Replace(string(b),
		"fetch = +refs/heads/*:refs/heads/*",
		"fetch = "+refspec(spec), 1)
	if !strings.Contains(out, "fetch = ") {
		out += "\n[remote \"origin\"]\n\tfetch = " + refspec(spec) + "\n"
	}
	return os.WriteFile(cfg, []byte(out), 0o600) //nolint:gosec // G703: the path is Storage.ReposDir plus a catalogue name the validator restricts
}

// Fetch updates the clone. Callers that just missed an object should fetch once
// and then fall back to the GitLab API rather than fetching in a loop.
func (s *Store) Fetch(ctx context.Context, spec Spec) error {
	// Two pushes in a row, or a push and the cron that covers for it, must not
	// run two fetches: the second would wait on git's own lock anyway.
	_, err := s.once(ctx, "fetch:"+spec.Name, func(ctx context.Context) (string, error) {
		args := []string{"fetch", "--prune"}
		if !spec.Tags {
			args = append(args, "--no-tags")
		}
		args = append(args, "origin")
		if _, err := s.runEnv(ctx, s.ClonePath(spec.Name), authEnv(spec), args...); err != nil {
			return "", err
		}
		s.markFetched(spec.Name)
		return "", nil
	})
	return err
}

func (s *Store) markFetched(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFetch[name] = s.now()
}

// Staleness reports how long ago each mirror last caught up. A repository never
// fetched in this process is absent rather than zero: "never asked about" and
// "fetched just now" must not look the same.
func (s *Store) Staleness() map[string]time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := make(map[string]time.Duration, len(s.lastFetch))
	now := s.now()
	for name, at := range s.lastFetch {
		res[name] = now.Sub(at)
	}
	return res
}

// ResolveSHA turns a short SHA — what Sentry calls a release — into the full
// one, which is the cache key, the `ref=` of a GitLab call and the audit line.
//
// A branch name is looked up under refs/remotes/origin first: the clone's own
// refs/heads/* are a snapshot from the day it was made, so "master" resolved
// against them would name a commit that stopped being master long ago.
func (s *Store) ResolveSHA(ctx context.Context, name, ref string) (string, error) {
	if err := ValidateRef(ref); err != nil {
		return "", err
	}
	candidates := []string{ref}
	if !shaRe.MatchString(ref) {
		candidates = []string{"refs/remotes/origin/" + ref, ref}
	}
	var err error
	for _, c := range candidates {
		// ^{commit} is not decoration: without it a prefix can resolve to a blob
		// or a tree, and the error surfaces much later as something unrelated.
		var out []byte
		out, err = s.run(ctx, s.ClonePath(name), "rev-parse", "--verify", "--end-of-options", c+"^{commit}")
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", classify(err, ref)
}

// Worktree returns a checkout of one commit, creating it on first use. The
// checkout is detached: a working tree that can accept a commit is a working
// tree somebody will eventually commit to.
func (s *Store) Worktree(ctx context.Context, name, sha string) (string, func(), error) {
	if !shaRe.MatchString(sha) {
		return "", nil, fmt.Errorf("%w: worktree needs a full sha, got %q", ErrBadRef, sha)
	}

	key := name + "@" + sha
	// Taken before the checkout, not after: a tree with a zero count and no
	// lastUsed sorts as the oldest of all, so Prune would remove it while
	// `worktree add` was still writing it.
	s.acquire(key)
	path, err := s.once(ctx, key, func(ctx context.Context) (string, error) {
		return s.createWorktree(ctx, name, sha)
	})
	if err != nil {
		s.release(key)
		return "", nil, err
	}
	return path, func() { s.release(key) }, nil
}

// readyMarker names the file that says a checkout finished. The directory alone
// proves nothing: a checkout killed by a timeout leaves a half-written tree that
// os.Stat reports as present.
func readyMarker(path string) string { return path + ".ready" }

func (s *Store) createWorktree(ctx context.Context, name, sha string) (string, error) {
	path := s.WorktreePath(name, sha)
	if _, err := os.Stat(readyMarker(path)); err == nil {
		return path, nil
	}
	if _, err := os.Stat(path); err == nil {
		// A directory without its marker is a checkout that did not finish. It
		// goes, together with git's record of it: `worktree add` refuses a path
		// it believes is still in use.
		if err := os.RemoveAll(path); err != nil {
			return "", fmt.Errorf("git: remove half-written worktree %s: %w", path, err)
		}
		if _, err := s.run(ctx, s.ClonePath(name), "worktree", "prune"); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("git: create worktree dir: %w", err)
	}
	if _, err := s.run(ctx, s.ClonePath(name), "worktree", "add", "--detach", "--force", path, sha); err != nil {
		return "", err
	}
	if err := os.WriteFile(readyMarker(path), []byte(sha+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("git: mark worktree %s ready: %w", path, err)
	}
	if s.onCreate != nil {
		s.onCreate()
	}
	return path, nil
}

// once runs fn for a key at most once at a time and shares the result with
// everyone who asked while it was running.
//
// DoChan rather than Do, so a caller whose context is cancelled stops waiting
// while the work carries on for whoever is still there. The work gets a context
// that cannot be cancelled — run() still puts its own timeout on it.
func (s *Store) once(ctx context.Context, key string, fn func(ctx context.Context) (string, error)) (string, error) {
	work := context.WithoutCancel(ctx)
	ch := s.group.DoChan(key, func() (any, error) { return fn(work) })
	select {
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		path, _ := res.Val.(string)
		return path, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Store) acquire(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inUse[key]++
	s.lastUsed[key] = s.now()
}

func (s *Store) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse[key] > 0 {
		s.inUse[key]--
	}
	s.lastUsed[key] = s.now()
}

func refspec(spec Spec) string {
	if len(spec.Refspec) > 0 {
		return spec.Refspec[0]
	}
	return "+refs/heads/*:refs/remotes/origin/*"
}

// Prune keeps the worktrees inside the disk budget, dropping the least recently
// used first. Clones are never touched, and trees currently being read are
// skipped — the reader would find the file gone halfway through.
func (s *Store) Prune(ctx context.Context) (freed int64, err error) {
	if s.opts.MaxDiskBytes <= 0 {
		return 0, nil
	}

	trees, total, err := s.scanWorktrees()
	if err != nil {
		return 0, err
	}
	if total <= s.opts.MaxDiskBytes {
		return 0, nil
	}

	// Oldest first; a tree nobody has asked for yet sorts as oldest of all.
	sort.Slice(trees, func(i, j int) bool { return trees[i].used.Before(trees[j].used) })

	pruned := map[string]bool{}
	for _, t := range trees {
		if total-freed <= s.opts.MaxDiskBytes {
			break
		}
		if s.busy(t.key) {
			continue
		}
		if err := os.RemoveAll(t.path); err != nil {
			return freed, fmt.Errorf("git: remove worktree %s: %w", t.path, err)
		}
		if err := os.Remove(readyMarker(t.path)); err != nil && !os.IsNotExist(err) {
			return freed, fmt.Errorf("git: remove worktree marker %s: %w", t.path, err)
		}
		freed += t.size
		pruned[t.repo] = true
		s.forget(t.key)
	}

	// git keeps administrative files for every worktree it created; without
	// prune they accumulate and `worktree add` starts refusing paths.
	for repo := range pruned {
		if _, err := s.run(ctx, s.ClonePath(repo), "worktree", "prune"); err != nil {
			return freed, err
		}
	}
	return freed, nil
}

type treeInfo struct {
	repo, key, path string
	size            int64
	used            time.Time
}

func (s *Store) scanWorktrees() ([]treeInfo, int64, error) {
	repos, err := os.ReadDir(s.opts.WorktreesDir)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("git: read worktrees dir: %w", err)
	}

	var out []treeInfo
	var total int64
	for _, r := range repos {
		if !r.IsDir() {
			continue
		}
		shas, err := os.ReadDir(filepath.Join(s.opts.WorktreesDir, r.Name()))
		if err != nil {
			return nil, 0, fmt.Errorf("git: read worktrees of %s: %w", r.Name(), err)
		}
		for _, sha := range shas {
			if !sha.IsDir() {
				continue
			}
			path := filepath.Join(s.opts.WorktreesDir, r.Name(), sha.Name())
			size, err := dirSize(path)
			if err != nil {
				return nil, 0, err
			}
			key := r.Name() + "@" + sha.Name()
			out = append(out, treeInfo{repo: r.Name(), key: key, path: path, size: size, used: s.usedAt(key)})
			total += size
		}
	}
	return out, total, nil
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("git: size of %s: %w", path, err)
	}
	return total, nil
}

func (s *Store) busy(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse[key] > 0
}

func (s *Store) usedAt(key string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsed[key]
}

func (s *Store) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastUsed, key)
	delete(s.inUse, key)
}
