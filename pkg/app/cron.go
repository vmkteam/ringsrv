package app

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/vmkteam/cron"
)

// Everything that runs on a timer: the manager, the schedules and the start.
// The job bodies stay with their subject — syncMirrors and pruneStorage in
// mirrors.go, Reprove in dbq — so this file is what answers "what runs by
// itself, and how often" without reading either.
const (
	// mirrorSync is the safety net behind the webhook: the push makes a fetch
	// immediate, this makes it certain. A webhook that silently stops arriving is
	// the normal failure here, and nothing looks broken until somebody asks about
	// a fresh release.
	mirrorSyncJob      = "mirror-sync"
	mirrorSyncSchedule = "*/15 * * * *"
	// Without this job the Storage.MaxDiskBytes budget is a config value that
	// does nothing, and worktrees accumulate one checkout per commit asked about.
	storagePruneJob      = "storage-prune"
	storagePruneSchedule = "*/30 * * * *"
	// The hourly re-proof is for ClickHouse, whose client has no per-connection
	// hook; PostgreSQL proves every connection on its own. An unproven target
	// retries on its first call regardless.
	dbReproveJob      = "db-reprove"
	dbReproveSchedule = "0 * * * *"
)

// registerCron builds the one manager and schedules whatever this instance has
// to schedule. An instance with neither repositories nor databases gets no
// manager at all — nothing runs by itself, and startCron says nothing.
//
// WithSkipActive is what keeps a slow job from being started again by the next
// tick: a fetch of every mirror can outlast its own window.
func (a *App) registerCron() {
	if a.repos == nil && a.db == nil {
		return
	}
	a.cron = cron.NewManager()
	a.cron.Use(
		cron.WithMetrics(a.appName),
		// embedlog satisfies cron.Logger as it is: Print/Error with a context.
		cron.WithSLog(a.Logger),
		cron.WithSkipActive(),
		cron.WithRecover(),
	)
	if a.repos != nil {
		a.cron.AddFunc(mirrorSyncJob, mirrorSyncSchedule, a.syncMirrors)
		a.cron.AddFunc(storagePruneJob, storagePruneSchedule, a.pruneStorage)
	}
	if a.db != nil {
		// A target that stays unproven is the job's failure, so /debug/cron and
		// the metrics say so instead of reporting an hourly success over
		// databases nobody can query.
		a.cron.AddFunc(dbReproveJob, dbReproveSchedule, a.db.Reprove)
	}
}

// startCron runs whatever was scheduled. An instance with neither
// repositories nor databases has no manager and nothing to start.
func (a *App) startCron(ctx context.Context) {
	if a.cron == nil {
		return
	}
	if err := a.cron.Run(ctx); err != nil {
		a.Error(ctx, "cron start failed", "err", err.Error())
		return
	}
	a.echo.GET("/debug/cron", echo.WrapHandler(http.HandlerFunc(a.cron.Handler)))
	a.Print(ctx, "cron started", "mirrors", a.repos != nil, "databases", a.db != nil)
}
