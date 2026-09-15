package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitT runs git in dir for test fixtures, with an author so commits work on a
// machine with no global config. Same shape as vmksrv's helper.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// originRepo builds a small repository with two commits and returns its path
// plus both SHAs. Tests run against real git: the whole package is argv and
// exit codes, and a fake would test the fake.
func originRepo(t *testing.T) (path, first, second string) {
	t.Helper()
	path = t.TempDir()
	gitT(t, path, "init", "--initial-branch=master")

	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(path, name), []byte(body), 0o600))
	}
	write("main.go", "package main\n\nfunc main() {}\n")
	gitT(t, path, "add", ".")
	gitT(t, path, "commit", "-m", "PLF-1 initial")
	first = gitT(t, path, "rev-parse", "HEAD")

	write("main.go", "package main\n\nfunc main() { println(\"hi\") }\n")
	gitT(t, path, "add", ".")
	gitT(t, path, "commit", "-m", "PLF-2 print")
	second = gitT(t, path, "rev-parse", "HEAD")
	return path, first, second
}

func newStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	return New(Options{
		ReposDir:     filepath.Join(root, "repos"),
		WorktreesDir: filepath.Join(root, "worktrees"),
		Timeout:      time.Minute,
	})
}

func TestCheck(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	require.NoError(t, s.Check(t.Context()))
	assert.Contains(t, s.Version(), "git version")

	// A missing binary has to be a clear boot error, not a puzzle on the first
	// question about code.
	missing := New(Options{Git: "git-that-does-not-exist", Timeout: time.Second})
	require.Error(t, missing.Check(t.Context()))
}

func TestEnsureAndFetch(t *testing.T) {
	t.Parallel()
	origin, _, second := originRepo(t)
	s := newStore(t)
	spec := Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}

	require.NoError(t, s.Ensure(t.Context(), spec))
	assert.FileExists(t, filepath.Join(s.ClonePath("apisrv"), "HEAD"))

	// A commit made after the clone is invisible until a fetch, and visible
	// right after — that is the whole contract of the mirror.
	gitT(t, origin, "commit", "--allow-empty", "-m", "PLF-3 later")
	third := gitT(t, origin, "rev-parse", "HEAD")

	_, err := s.ResolveSHA(t.Context(), "apisrv", third[:10])
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, s.Ensure(t.Context(), spec)) // second Ensure fetches
	got, err := s.ResolveSHA(t.Context(), "apisrv", third[:10])
	require.NoError(t, err)
	assert.Equal(t, third, got)

	// Tags are not mirrored: they inflated the measured mirror to 20102 refs
	// against one, and no question we ask needs them.
	gitT(t, origin, "tag", "v1.0.0", second)
	require.NoError(t, s.Fetch(t.Context(), spec))
	out, err := s.run(t.Context(), s.ClonePath("apisrv"), "tag", "--list")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(out)))
}

// Sentry reports a release as a short SHA, and everything downstream needs the
// full one: it is the cache key, the ref= of a GitLab call and the audit line.
func TestResolveSHA(t *testing.T) {
	t.Parallel()
	origin, first, _ := originRepo(t)
	s := newStore(t)
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	got, err := s.ResolveSHA(t.Context(), "apisrv", first[:8])
	require.NoError(t, err)
	assert.Equal(t, first, got)

	_, err = s.ResolveSHA(t.Context(), "apisrv", "deadbeefdeadbeef")
	require.ErrorIs(t, err, ErrNotFound)

	// A tree or a blob must not answer: without ^{commit} a prefix can resolve
	// to one, and the mistake surfaces much later as something unrelated.
	tree := gitT(t, origin, "rev-parse", "HEAD^{tree}")
	_, err = s.ResolveSHA(t.Context(), "apisrv", tree)
	assert.Error(t, err)
}

func TestWorktree(t *testing.T) {
	t.Parallel()
	origin, first, second := originRepo(t)
	s := newStore(t)
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	path, release, err := s.Worktree(t.Context(), "apisrv", first)
	require.NoError(t, err)
	defer release()

	body, err := os.ReadFile(filepath.Join(path, "main.go"))
	require.NoError(t, err)
	assert.NotContains(t, string(body), "println", "the tree is the first commit, not HEAD")

	// Detached on purpose: a working tree that can accept a commit is one
	// somebody will eventually commit to.
	head, err := os.ReadFile(filepath.Join(s.ClonePath("apisrv"), "worktrees", first, "HEAD"))
	require.NoError(t, err)
	assert.Equal(t, first, strings.TrimSpace(string(head)))

	second2, release2, err := s.Worktree(t.Context(), "apisrv", second)
	require.NoError(t, err)
	defer release2()
	assert.NotEqual(t, path, second2)
}

// Two questions about one commit must produce one checkout. Without this a
// Sentry issue opened by two people at once pays for the same worktree twice.
func TestWorktree_SingleFlight(t *testing.T) {
	t.Parallel()
	origin, first, _ := originRepo(t)
	s := newStore(t)
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	var created int
	var mu sync.Mutex
	s.onCreate = func() { mu.Lock(); created++; mu.Unlock() }

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			path, release, err := s.Worktree(t.Context(), "apisrv", first)
			if assert.NoError(t, err) {
				assert.NotEmpty(t, path)
				release()
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, created, "ten questions about one commit, one checkout")
}

func TestPrune(t *testing.T) {
	t.Parallel()
	origin, first, second := originRepo(t)
	s := newStore(t)
	s.opts.MaxDiskBytes = 1 // anything is over budget
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	oldPath, releaseOld, err := s.Worktree(t.Context(), "apisrv", first)
	require.NoError(t, err)
	releaseOld() // nobody is reading it any more

	busyPath, releaseBusy, err := s.Worktree(t.Context(), "apisrv", second)
	require.NoError(t, err)
	defer releaseBusy() // still being read

	freed, err := s.Prune(t.Context())
	require.NoError(t, err)
	assert.Positive(t, freed)

	assert.NoDirExists(t, oldPath, "the tree nobody reads goes first")
	assert.DirExists(t, busyPath, "the tree somebody is reading is never removed under them")

	// The clone itself is not a cache entry and must survive.
	assert.FileExists(t, filepath.Join(s.ClonePath("apisrv"), "HEAD"))

	// A pruned worktree can be created again — administrative files were
	// cleaned up, otherwise `worktree add` refuses the path.
	again, release, err := s.Worktree(t.Context(), "apisrv", first)
	require.NoError(t, err)
	defer release()
	assert.DirExists(t, again)
}

func TestRun_RejectsUnknownSubcommand(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.run(context.Background(), t.TempDir(), "push", "origin", "master")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed")
}

// Nothing the model writes reaches git's argv: `git -c`, an alias like `!sh`,
// `--upload-pack` and `--exec` all run arbitrary programs, and a ref is the
// shortest path from a tool argument to a command line.
func TestValidateRef(t *testing.T) {
	t.Parallel()
	valid := []string{
		"abc1234",
		"abc1234def5678abc1234def5678abc1234def56",
		"master",
		"release/2026-09",
		"feature.x",
	}
	for _, ref := range valid {
		t.Run("ok/"+ref, func(t *testing.T) {
			t.Parallel()
			assert.NoError(t, ValidateRef(ref))
		})
	}

	bad := []string{
		"",
		"--upload-pack=sh",
		"-c",
		"--exec=/bin/sh",
		"master; rm -rf /",
		"../../etc/passwd",
		"master master",
		"ref\nwith-newline",
		"$(whoami)",
	}
	for _, ref := range bad {
		t.Run("rejected/"+ref, func(t *testing.T) {
			t.Parallel()
			assert.ErrorIs(t, ValidateRef(ref), ErrBadRef)
		})
	}
}

func TestValidatePath(t *testing.T) {
	t.Parallel()
	valid := []string{"main.go", "internal/rpc/order.go", "docs/llm/index.md"}
	for _, p := range valid {
		t.Run("ok/"+p, func(t *testing.T) {
			t.Parallel()
			assert.NoError(t, ValidatePath(p))
		})
	}

	bad := []string{"", "/etc/passwd", "../secrets", "internal/../../etc", "--output=x", "with\x00null"}
	for _, p := range bad {
		t.Run("rejected", func(t *testing.T) {
			t.Parallel()
			assert.ErrorIs(t, ValidatePath(p), ErrBadPath)
		})
	}
}

// classify is the most fragile thing in this package: it reads git's prose. The
// cases below are what git 2.50 actually prints, captured by running it — three
// earlier copies of this logic each missed a message and turned "no such commit"
// into "the server broke".
func TestClassify(t *testing.T) {
	t.Parallel()
	notFound := []string{
		"git: git rev-parse: exit status 128: fatal: ambiguous argument 'deadbeef': unknown revision or path not in the working tree.",
		"git: git rev-parse: exit status 128: fatal: Needed a single revision",
		"git: git cat-file: exit status 128: fatal: invalid object name 'zzz'.",
		"git: git cat-file: exit status 128: fatal: path 'x.go' does not exist in 'HEAD'",
		"git: git cat-file: exit status 128: fatal: path 'x.go' exists on disk, but not in 'HEAD'",
		"git: git log: exit status 128: fatal: There is no path x.go in the commit",
		"git: git blame: exit status 128: fatal: no such path x.go in HEAD",
		"git: git blame: exit status 128: fatal: bad revision 'nope'",
	}
	for _, msg := range notFound {
		t.Run("notfound/"+msg[:40], func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, classify(errors.New(msg), "what"), ErrNotFound)
		})
	}

	// Ambiguity is its own answer where git offers it — and under --verify it
	// does not: an unknown revision and a short SHA matching two objects both
	// come back as "Needed a single revision".
	amb := classify(errors.New("git: git rev-parse: exit status 128: error: short object ID abc is ambiguous"), "abc")
	require.ErrorIs(t, amb, ErrAmbiguous)

	// Anything else stays what it was: a timeout or a broken binary is not a
	// missing commit, and calling it one sends the caller looking for a typo.
	other := errors.New("git: git fetch: exit status 128: fatal: unable to access: Could not resolve host")
	require.NotErrorIs(t, classify(other, "x"), ErrNotFound)
	require.NotErrorIs(t, classify(other, "x"), ErrAmbiguous)
	require.NoError(t, classify(nil, "x"))
}

// Definition is what why(symbol) uses instead of a checkout and an index: a
// grep on the bare mirror, 13ms against seconds. It has to find a declaration
// where one exists and fall back to a mention where the shape is unfamiliar.
func TestDefinition(t *testing.T) {
	t.Parallel()
	origin := t.TempDir()
	gitT(t, origin, "init", "--initial-branch=master")
	require.NoError(t, os.WriteFile(filepath.Join(origin, "order.go"), []byte(`package rpc

import "fmt"

// Create makes an order.
func Create(id int) error {
	return fmt.Errorf("no")
}

type Repo struct{}

func (r Repo) byID(id int) int {
	return id
}

var used = Create
`), 0o600))
	gitT(t, origin, "add", ".")
	gitT(t, origin, "commit", "-m", "PLF-1 initial")
	sha := gitT(t, origin, "rev-parse", "HEAD")

	s := newStore(t)
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	// The declaration wins over the doc comment above it and over the use below.
	line, err := s.Definition(t.Context(), "apisrv", sha, "order.go", "Create")
	require.NoError(t, err)
	assert.Equal(t, 6, line)

	line, err = s.Definition(t.Context(), "apisrv", sha, "order.go", "Repo")
	require.NoError(t, err)
	assert.Equal(t, 10, line)

	line, err = s.Definition(t.Context(), "apisrv", sha, "order.go", "byID")
	require.NoError(t, err)
	assert.Equal(t, 12, line)

	// Absent is absent — the caller is told rather than handed line 1.
	_, err = s.Definition(t.Context(), "apisrv", sha, "order.go", "NoSuchThing")
	require.ErrorIs(t, err, ErrNotFound)

	// The name goes into a regular expression, so it is checked first.
	for _, bad := range []string{"", "a.*", "a|b", "a b", "../x"} {
		_, err := s.Definition(t.Context(), "apisrv", sha, "order.go", bad)
		require.ErrorIs(t, err, ErrBadPath, "symbol %q", bad)
	}
}

// The clone token reaches git through its config environment and nowhere
// else: not argv, where ps would show it, and not the clone's own config on
// the volume, where it would outlive its rotation.
func TestAuthEnv(t *testing.T) {
	t.Parallel()
	assert.Nil(t, authEnv(Spec{CloneURL: "https://git.example.com/a.git"}))

	env := authEnv(Spec{CloneURL: "https://git.example.com/a.git", CloneToken: "secret"})
	require.Len(t, env, 3)
	assert.Equal(t, "GIT_CONFIG_COUNT=1", env[0])
	assert.Equal(t, "GIT_CONFIG_KEY_0=http.extraheader", env[1])
	// Basic of "oauth2:secret", byte for byte what git derived from the URL
	// form before.
	assert.Equal(t, "GIT_CONFIG_VALUE_0=Authorization: Basic b2F1dGgyOnNlY3JldA==", env[2])
}

// A clone made by an earlier version carries the token in its config. Ensure
// scrubs it on the way, so the volume stops holding a rotated secret without
// anyone cleaning it by hand.
func TestEnsure_ScrubsCredentialsLeftByOlderClones(t *testing.T) {
	t.Parallel()
	origin, _, _ := originRepo(t)
	s := newStore(t)
	spec := Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}
	require.NoError(t, s.Ensure(t.Context(), spec))

	cfg := filepath.Join(s.ClonePath("apisrv"), "config")
	b, err := os.ReadFile(cfg)
	require.NoError(t, err)
	// Plant what the old clone command left behind.
	planted := strings.Replace(string(b), "url = ", "url = https://oauth2:secret@git.example.com/x.git\n\turl-old = ", 1)
	require.Contains(t, planted, "oauth2:secret@")
	require.NoError(t, os.WriteFile(cfg, []byte(planted), 0o600))

	// Ensure fetches afterwards, which fails against the planted remote — the
	// scrub has to happen before that, and it does.
	_ = s.Ensure(t.Context(), spec)
	b, err = os.ReadFile(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "secret", "the token is gone from the clone's config")
	assert.Contains(t, string(b), "url = https://git.example.com/x.git")
}

// Fetches update refs/remotes/origin/* and never the clone's own heads, so both
// a branch name and "which branches contain this" have to look there. Otherwise
// "master" stays the clone-day snapshot and a later commit is on no branch.
func TestBranchesFollowTheRemote(t *testing.T) {
	t.Parallel()
	origin, _, second := originRepo(t)
	s := newStore(t)
	spec := Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}
	require.NoError(t, s.Ensure(t.Context(), spec))

	// Move master and create a branch after the clone.
	gitT(t, origin, "commit", "--allow-empty", "-m", "PLF-3 later")
	third := gitT(t, origin, "rev-parse", "HEAD")
	gitT(t, origin, "branch", "hotfix", third)
	require.NoError(t, s.Fetch(t.Context(), spec))

	got, err := s.ResolveSHA(t.Context(), "apisrv", "master")
	require.NoError(t, err)
	assert.Equal(t, third, got, "master is where origin's master is now")

	got, err = s.ResolveSHA(t.Context(), "apisrv", "hotfix")
	require.NoError(t, err)
	assert.Equal(t, third, got, "a branch created after the clone resolves")

	branches, _, err := s.Contains(t.Context(), "apisrv", third)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"master", "hotfix"}, branches)

	branches, _, err = s.Contains(t.Context(), "apisrv", second)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"master", "hotfix"}, branches, "stale heads and remote refs merge into one name each")
}

// A caller whose context is cancelled stops waiting; the work carries on for
// whoever is still there. It used to run on the first caller's context and die
// with it.
func TestOnce_WorkOutlivesTheCallerThatStartedIt(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	started := make(chan struct{}, 4)
	proceed := make(chan struct{})
	var runs int
	var mu sync.Mutex
	work := func(ctx context.Context) (string, error) { //nolint:unparam // the shape once expects
		mu.Lock()
		runs++
		mu.Unlock()
		assert.NoError(t, ctx.Err(), "the work must not inherit the cancellation")
		started <- struct{}{}
		<-proceed
		return "done", nil
	}

	// The first caller arrives already cancelled: it starts the work and
	// leaves at once.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.once(cancelled, "k", work)
	require.ErrorIs(t, err, context.Canceled)
	<-started

	// The second joins the run still blocked on proceed. The pause gives it time
	// to reach DoChan before the run is let go; otherwise it arrives after the
	// key was cleared and starts a second run, which is not what is tested here.
	got := make(chan string, 1)
	go func() {
		v, err := s.once(context.Background(), "k", work)
		assert.NoError(t, err)
		got <- v
	}()
	time.Sleep(200 * time.Millisecond)
	close(proceed)
	assert.Equal(t, "done", <-got)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, runs, "one run, shared by both")
}

// A checkout abandoned by a cancelled request still finishes, and the marker
// says so; before, the half-written directory passed for a worktree.
func TestWorktree_FinishesAfterTheCallerLeft(t *testing.T) {
	t.Parallel()
	origin, first, _ := originRepo(t)
	s := newStore(t)
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := s.Worktree(cancelled, "apisrv", first)
	require.ErrorIs(t, err, context.Canceled)

	// Nobody asks again: the checkout started by the cancelled call completes
	// on its own, or the marker never appears.
	path := s.WorktreePath("apisrv", first)
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyMarker(path))
		return err == nil
	}, 30*time.Second, 50*time.Millisecond, "the checkout finishes without a waiter")
	assert.Equal(t, 0, s.busy2("apisrv@"+first), "the cancelled caller released its hold")
}

// A directory without its marker is a checkout that did not finish — a
// timeout, a kill — and answering from it would describe a tree that never
// existed. It is rebuilt, not trusted.
func TestWorktree_HalfWrittenTreeIsRebuilt(t *testing.T) {
	t.Parallel()
	origin, first, _ := originRepo(t)
	s := newStore(t)
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	var created int
	var mu sync.Mutex
	s.onCreate = func() { mu.Lock(); created++; mu.Unlock() }
	count := func() int { mu.Lock(); defer mu.Unlock(); return created }

	path, release, err := s.Worktree(t.Context(), "apisrv", first)
	require.NoError(t, err)
	release()
	assert.FileExists(t, readyMarker(path))

	// Simulate the kill: the tree is there, the marker is not, and a file is
	// missing from what git wrote.
	require.NoError(t, os.Remove(readyMarker(path)))
	require.NoError(t, os.Remove(filepath.Join(path, "main.go")))

	again, release2, err := s.Worktree(t.Context(), "apisrv", first)
	require.NoError(t, err)
	defer release2()
	assert.Equal(t, path, again)
	assert.FileExists(t, filepath.Join(path, "main.go"), "the tree was checked out again, not reused")
	assert.FileExists(t, readyMarker(path))
	assert.Equal(t, 2, count())

	// A finished tree is reused as before.
	_, release3, err := s.Worktree(t.Context(), "apisrv", first)
	require.NoError(t, err)
	release3()
	assert.Equal(t, 2, count())
}

// The first two questions about a repository used to race into two clones,
// and the second died on "already exists".
func TestEnsure_CloneSingleFlight(t *testing.T) {
	t.Parallel()
	origin, _, _ := originRepo(t)
	s := newStore(t)
	spec := Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			assert.NoError(t, s.Ensure(context.Background(), spec))
		})
	}
	wg.Wait()
	assert.True(t, s.Have("apisrv"))
}

// A tree is held from the moment it is asked for, so the LRU cannot remove it
// while `worktree add` is still writing it; and the marker goes with the tree.
func TestPrune_MarkerAndHold(t *testing.T) {
	t.Parallel()
	origin, first, _ := originRepo(t)
	s := newStore(t)
	s.opts.MaxDiskBytes = 1
	require.NoError(t, s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: origin, DefaultBranch: "master"}))

	path, release, err := s.Worktree(t.Context(), "apisrv", first)
	require.NoError(t, err)
	assert.Equal(t, 1, s.busy2("apisrv@"+first))
	release()

	_, err = s.Prune(t.Context())
	require.NoError(t, err)
	assert.NoDirExists(t, path)
	assert.NoFileExists(t, readyMarker(path), "the marker does not outlive its tree")
}

// busy2 is the in-use count, for tests that check the hold is taken early.
func (s *Store) busy2(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse[key]
}
