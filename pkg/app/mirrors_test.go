package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring/code/gittest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
)

const testWebhookSecret = "s3cret-token"

// mirrorApp builds just enough App to answer a webhook: a catalogue with one
// repository, a real git store and an echo with the handlers registered.
func mirrorApp(t *testing.T, secret string) (*App, string) {
	t.Helper()

	origin := t.TempDir()
	gittest.Git(t, origin, "init", "--initial-branch=master")
	require.NoError(t, os.WriteFile(filepath.Join(origin, "main.go"), []byte("package main\n"), 0o600))
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-1 initial")

	cat, err := target.Parse([]byte(`
Env = "dev"
WebhookSecret = "` + secret + `"

[Profiles.prom]
Description  = "dev · Prometheus"
BaseURL      = "https://prom.example.com"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Repos.apisrv]
Description   = "test repo"
GitLabProject = 42
CloneURL      = "file://` + origin + `"
DefaultBranch = "master"
`))
	require.NoError(t, err)

	root := t.TempDir()
	a := &App{
		appName: "ringsrv-test",
		Logger:  embedlog.Logger{},
		echo:    echo.New(),
		targets: cat,
		repos: git.New(git.Options{
			ReposDir:     filepath.Join(root, "repos"),
			WorktreesDir: filepath.Join(root, "worktrees"),
			Timeout:      time.Minute,
		}),
	}
	a.initMirrorFetches()
	a.echo.POST("/v1/webhook/gitlab", a.handleGitLabWebhook)
	// A webhook fetch outlives the request; the temp dirs must outlive the
	// fetch, or the cleanup races a clone still writing into them.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a.waitFetches(ctx)
	})
	return a, origin
}

func post(t *testing.T, a *App, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/gitlab", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Gitlab-Token", token)
	}
	rec := httptest.NewRecorder()
	a.echo.ServeHTTP(rec, req)
	return rec
}

func TestWebhook_Auth(t *testing.T) {
	t.Parallel()
	a, _ := mirrorApp(t, testWebhookSecret)
	push := `{"object_kind":"push","project":{"id":42}}`

	assert.Equal(t, http.StatusUnauthorized, post(t, a, "", push).Code, "no token")
	assert.Equal(t, http.StatusUnauthorized, post(t, a, "wrong", push).Code, "somebody else's token")
	assert.Equal(t, http.StatusOK, post(t, a, testWebhookSecret, push).Code)

	// Nothing was fetched for the refused calls: the store only knows about a
	// repository after a successful fetch.
	off, _ := mirrorApp(t, "")
	assert.Equal(t, http.StatusNotFound, post(t, off, "", push).Code,
		"without a secret the endpoint is off, not open")
}

// A valid push fetches the repository named by the project id, and the commit
// pushed after the clone becomes visible.
func TestWebhook_Fetches(t *testing.T) {
	t.Parallel()
	a, origin := mirrorApp(t, testWebhookSecret)

	// Prime the mirror, then push something it has not seen.
	require.NoError(t, a.repos.Ensure(t.Context(), a.targets.Repos["apisrv"].Spec("apisrv")))
	gittest.Git(t, origin, "commit", "--allow-empty", "-m", "ABC-2 later")
	fresh := gittest.Git(t, origin, "rev-parse", "HEAD")

	_, err := a.repos.ResolveSHA(t.Context(), "apisrv", fresh[:10])
	require.ErrorIs(t, err, git.ErrNotFound, "not in the mirror yet — this is the window the webhook closes")

	require.Equal(t, http.StatusOK, post(t, a, testWebhookSecret, `{"object_kind":"push","project":{"id":42}}`).Code)

	// The handler answers before the fetch finishes, so the assertion waits.
	require.Eventually(t, func() bool {
		_, err := a.repos.ResolveSHA(t.Context(), "apisrv", fresh[:10])
		return err == nil
	}, 30*time.Second, 100*time.Millisecond, "the pushed commit becomes resolvable")

	// Staleness is what makes a mirror that stopped catching up visible.
	age, ok := a.repos.Staleness()["apisrv"]
	require.True(t, ok)
	assert.Less(t, age, time.Minute)
}

// GitLab retries on non-2xx, so anything we cannot act on is accepted and
// dropped rather than refused forever.
func TestWebhook_IgnoresWhatItCannotUse(t *testing.T) {
	t.Parallel()
	a, _ := mirrorApp(t, testWebhookSecret)

	cases := map[string]string{
		"a tag event":          `{"object_kind":"tag_push","project":{"id":42}}`,
		"an unknown project":   `{"object_kind":"push","project":{"id":999}}`,
		"no project at all":    `{"object_kind":"push"}`,
		"the older field name": `{"object_kind":"push","project_id":42}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, http.StatusOK, post(t, a, testWebhookSecret, body).Code)
		})
	}

	// Past the secret nothing answers 4xx: GitLab disables a hook after four
	// of them in a row, and a body we could not parse is ours to log.
	assert.Equal(t, http.StatusOK, post(t, a, testWebhookSecret, `not json`).Code)
	assert.Equal(t, http.StatusOK, post(t, a, testWebhookSecret, strings.Repeat("x", webhookBodyLimit+1)).Code,
		"a body over the limit is dropped with 200, not refused")
}

// Shutdown waits for the fetches a webhook started: they outlive the request
// on purpose, and must not outlive the process unnoticed.
func TestShutdown_WaitsForFetches(t *testing.T) {
	t.Parallel()
	a, _ := mirrorApp(t, testWebhookSecret)

	release := make(chan struct{})
	a.fetchSlots <- struct{}{}
	a.fetchWG.Go(func() {
		defer func() { <-a.fetchSlots }()
		<-release
	})

	done := make(chan error, 1)
	go func() { done <- a.Shutdown(5 * time.Second) }()

	select {
	case <-done:
		t.Fatal("Shutdown returned while a fetch was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-done)

	// And it gives up rather than hangs when a fetch does not finish in time.
	stuck := make(chan struct{})
	a.fetchSlots <- struct{}{}
	a.fetchWG.Go(func() {
		defer func() { <-a.fetchSlots }()
		<-stuck
	})
	require.NoError(t, a.Shutdown(300*time.Millisecond))
	close(stuck)
}

// The catalogue answers which repository a project id belongs to.
func TestRepoByProject(t *testing.T) {
	t.Parallel()
	a, _ := mirrorApp(t, testWebhookSecret)

	name, ok := a.targets.RepoByProject(42)
	assert.True(t, ok)
	assert.Equal(t, "apisrv", name)

	_, ok = a.targets.RepoByProject(0)
	assert.False(t, ok, "an event without a project id matches nothing")
}

// The cron is the safety net for a webhook that stopped arriving, so it walks
// the whole catalogue.
func TestSyncMirrors(t *testing.T) {
	t.Parallel()
	a, origin := mirrorApp(t, testWebhookSecret)

	gittest.Git(t, origin, "commit", "--allow-empty", "-m", "ABC-3 while nobody looked")
	fresh := gittest.Git(t, origin, "rev-parse", "HEAD")

	require.NoError(t, a.syncMirrors(t.Context()))

	got, err := a.repos.ResolveSHA(t.Context(), "apisrv", fresh[:10])
	require.NoError(t, err)
	assert.Equal(t, fresh, got)
}

// The fetch outlives the request, and echo returns the context to its pool the
// moment the handler does. So nothing the goroutine reads may point back into
// that context: the next push resets it, and the request id of a stranger ends
// up in the log line of this fetch — when the race does not simply read a
// half-written Request. Pushes in a row make the overlap likely; -race is what
// reports it (CI job 80773).
func TestWebhook_FetchReadsNothingOfTheRequest(t *testing.T) {
	t.Parallel()
	a, _ := mirrorApp(t, testWebhookSecret)

	for range 20 {
		assert.Equal(t, http.StatusOK, post(t, a, testWebhookSecret, `{"object_kind":"push","project":{"id":42}}`).Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.waitFetches(ctx)
}

// Two pushes in a row must not run two fetches of one repository.
func TestFetch_SingleFlight(t *testing.T) {
	t.Parallel()
	a, _ := mirrorApp(t, testWebhookSecret)
	spec := a.targets.Repos["apisrv"].Spec("apisrv")
	require.NoError(t, a.repos.Ensure(t.Context(), spec))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			assert.NoError(t, a.repos.Fetch(context.Background(), spec))
		})
	}
	wg.Wait()

	_, ok := a.repos.Staleness()["apisrv"]
	assert.True(t, ok)
}

// The LRU that MaxDiskBytes promises had no caller in production until this
// job existed: the budget was a config value that limited nothing.
func TestPruneStorage(t *testing.T) {
	t.Parallel()
	a, _ := mirrorApp(t, testWebhookSecret)
	a.repos.SetMaxDiskBytes(1) // anything on disk is over budget

	spec := a.targets.Repos["apisrv"].Spec("apisrv")
	require.NoError(t, a.repos.Ensure(t.Context(), spec))
	sha, err := a.repos.ResolveSHA(t.Context(), "apisrv", "master")
	require.NoError(t, err)

	path, release, err := a.repos.Worktree(t.Context(), "apisrv", sha)
	require.NoError(t, err)
	release()
	require.DirExists(t, path)

	require.NoError(t, a.pruneStorage(t.Context()))
	assert.NoDirExists(t, path, "a worktree nobody is reading goes when the budget is exceeded")
	assert.FileExists(t, filepath.Join(a.repos.ClonePath("apisrv"), "HEAD"), "the mirror itself is not a cache entry")
}
