package app

import (
	"context"
	"crypto/subtle"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/gitlab"

	"github.com/labstack/echo/v4"
	"golang.org/x/sync/errgroup"
)

// Mirrors have to know about a commit before anyone asks about one: the window
// where Sentry already reports a release and the object is not in the mirror yet
// is the first few minutes of an incident.
//
// Two mechanisms: the webhook below makes it immediate, the cron makes it
// certain — see cron.go for the schedules.
const (
	// webhookBodyLimit is what we read of a push event. GitLab sends the whole
	// commit list with every changed path, and a push of twenty commits passed
	// 64 KiB easily: the truncated JSON failed to parse, and four such 400s in a
	// row are what makes GitLab disable a hook for good. A body beyond this
	// limit is accepted and logged rather than refused.
	webhookBodyLimit = 4 << 20
	webhookTimeout   = 5 * time.Minute
	// maxWebhookFetches caps the fetches a burst of pushes can have in flight.
	// The fetch is single-flighted per repository, but the goroutines waiting on
	// it are not, and a flood used to leave thousands alive for five minutes.
	maxWebhookFetches = 8
	// syncConcurrency and syncDeadline keep the safety net inside its own window:
	// run serially, four slow mirrors would outlast the schedule and
	// WithSkipActive would drop the next tick — the net failing exactly when the
	// webhook is most likely broken too.
	syncConcurrency = 4
	syncDeadline    = 10 * time.Minute
)

// initMirrorFetches prepares the pool behind webhook-driven fetches: a fetch
// that outlives its request must not outlive the process unnoticed.
func (a *App) initMirrorFetches() {
	a.fetchSlots = make(chan struct{}, maxWebhookFetches)
}

// registerMirrorHandlers wires the webhook, skipped when the instance has no
// code tools: an endpoint that answers 200 and does nothing is worse than one
// that is not there. The cron half of the same guarantee is in registerCron.
func (a *App) registerMirrorHandlers() {
	if a.repos == nil {
		return
	}
	a.initMirrorFetches()
	a.repos.RegisterMetrics()
	a.echo.POST("/v1/webhook/gitlab", a.handleGitLabWebhook)
}

// waitFetches blocks until the webhook fetches in flight are done or ctx
// expires. Called from Shutdown, after the listener is closed.
func (a *App) waitFetches(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		a.fetchWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		a.Error(context.WithoutCancel(ctx), "shutdown: webhook fetches still running, giving up on them")
	}
}

// handleGitLabWebhook accepts a push and returns immediately: the endpoint is
// public, so it must be cheap and must not hold the connection for a fetch.
func (a *App) handleGitLabWebhook(c echo.Context) error {
	secret := a.targets.WebhookSecret
	if secret == "" {
		// Refusing is the safe default: an unauthenticated endpoint that
		// triggers fetches is a way to make this service busy from outside.
		return c.NoContent(http.StatusNotFound)
	}
	// Constant time: a comparison that returns early leaks the secret one byte
	// at a time to anyone willing to measure.
	got := c.Request().Header.Get("X-Gitlab-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		// Logged because the usual reason a webhook "does not arrive" is a
		// secret that does not match, and silence makes that invisible.
		a.Print(c.Request().Context(), "webhook rejected", "reason", "bad token", "ip", c.RealIP())
		return c.NoContent(http.StatusUnauthorized)
	}

	// Past the secret, nothing answers 4xx: GitLab disables a hook after four
	// consecutive ones, and a body we could not parse is our problem to log
	// rather than a reason to lose every future push. The cron covers the drop.
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, webhookBodyLimit+1))
	if err != nil {
		a.Error(c.Request().Context(), "webhook body not read, event dropped", "err", err.Error())
		return c.NoContent(http.StatusOK)
	}
	if len(body) > webhookBodyLimit {
		a.Error(c.Request().Context(), "webhook body too large, event dropped", "limit", webhookBodyLimit)
		return c.NoContent(http.StatusOK)
	}
	push, err := gitlab.ParsePush(body)
	if err != nil {
		a.Error(c.Request().Context(), "webhook body not parsed, event dropped", "bytes", len(body), "err", err.Error())
		return c.NoContent(http.StatusOK)
	}

	// Anything but a push to a repository we know is accepted and ignored:
	// GitLab retries on non-2xx, and retrying a tag event forever helps nobody.
	name, ok := a.targets.RepoByProject(push.ProjectID)
	if !push.IsPush() || !ok {
		return c.NoContent(http.StatusOK)
	}

	// The fetch outlives the request on purpose, with its own timeout: GitLab
	// does not need to wait for a clone. A full slot pool means a fetch is
	// already under way, and dropping the event costs nothing.
	//
	// The context is taken here rather than inside the goroutine: echo returns
	// the context to its pool the moment the handler does, so reading c.Request()
	// later races with the next request's Reset and would log its id.
	ctx := context.WithoutCancel(c.Request().Context())
	select {
	case a.fetchSlots <- struct{}{}:
		a.fetchWG.Go(func() {
			defer func() { <-a.fetchSlots }()
			a.fetchMirror(ctx, name)
		})
	default:
		a.Print(ctx, "webhook fetch skipped: too many in flight", "repo", name)
	}
	return c.NoContent(http.StatusOK)
}

// fetchMirror brings one mirror up to date. Errors are logged, not returned:
// nobody is waiting for this, and the staleness metric is what makes a
// repeatedly failing fetch visible.
func (a *App) fetchMirror(ctx context.Context, name string) {
	repo, ok := a.targets.Repos[name]
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()

	if err := a.repos.Ensure(ctx, repo.Spec(name)); err != nil {
		a.Error(ctx, "mirror fetch failed", "repo", name, "err", err.Error())
		return
	}
	a.Print(ctx, "mirror fetched", "repo", name)
}

// pruneStorage evicts the worktrees nobody is reading once the disk budget is
// exceeded. Their AST indexes live inside them, so both go together.
func (a *App) pruneStorage(ctx context.Context) error {
	freed, err := a.repos.Prune(ctx)
	if err != nil {
		return err
	}
	if freed > 0 {
		a.Print(ctx, "storage pruned", "freed_bytes", freed)
	}
	return nil
}

// syncMirrors fetches every repository in the catalogue. It is the fallback
// for a webhook that never arrived, so it walks everything rather than only
// what was asked about recently.
func (a *App) syncMirrors(ctx context.Context) error {
	names := make([]string, 0, len(a.targets.Repos))
	for name := range a.targets.Repos {
		names = append(names, name)
	}
	sort.Strings(names)

	ctx, cancel := context.WithTimeout(ctx, syncDeadline)
	defer cancel()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(syncConcurrency)
	for _, name := range names {
		g.Go(func() error {
			a.fetchMirror(gctx, name)
			return nil
		})
	}
	return g.Wait()
}
