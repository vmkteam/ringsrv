package app

import (
	"testing"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/cron"
)

// registerCron decides what this instance does on a timer, and a job it fails
// to schedule fails quietly: nothing logs, no metric moves, the mirror simply
// goes stale until somebody asks about a release that is not in it. The job
// bodies are mirrors_test's business; this is about the registration.
//
// Empty clients are enough — registration reads the two pointers for nil and
// takes method values off them, so nothing here needs a repository or a pool.
//
// Only one case may build a manager: cron.WithMetrics registers its collectors
// in the default prometheus registry, under names that carry no instance of
// their own, so a second registerCron in this process panics. That also makes
// the file unfit for -count > 1, and it is why the two one-sided combinations
// (repositories without databases and the reverse) are read rather than run.
func TestRegisterCron(t *testing.T) {
	t.Parallel()

	t.Run("neither: nothing runs by itself", func(t *testing.T) {
		t.Parallel()
		a := appWith(t, func(*Config) {})

		a.registerCron()

		// No manager at all, rather than an empty one: startCron has nothing to
		// start and Shutdown has nothing to wait for.
		assert.Nil(t, a.cron, "an instance with neither repositories nor databases gets no cron manager")
	})

	t.Run("both: every job is scheduled", func(t *testing.T) {
		t.Parallel()
		a := appWith(t, func(*Config) {})
		a.repos, a.db = &git.Store{}, &dbq.Manager{}

		a.registerCron()

		require.NotNil(t, a.cron)
		assert.Equal(t, []string{mirrorSyncJob, storagePruneJob, dbReproveJob}, jobNames(a.cron.State()),
			"the mirrors and the re-proof are scheduled by one call now")
		for _, s := range a.cron.State() {
			assert.NotEmpty(t, s.Schedule, "%s runs on no schedule", s.Name)
		}
	})
}

func jobNames(states cron.States) []string {
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, s.Name)
	}
	return names
}
