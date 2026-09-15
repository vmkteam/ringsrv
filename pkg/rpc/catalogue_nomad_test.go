package rpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
	"github.com/vmkteam/ringsrv/pkg/ring/target/targettest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The nomad DefaultJQ recognises the shape of an answer by its keys and keeps
// what an on-call reader goes there for. It is exercised on fixtures
// projected from the dev cluster on 2026-09-02 — the raw bodies carry template
// secrets and 350 KB of dispatch children — through the same gojq path the
// server uses, on every shipped catalogue, so the three copies cannot drift.
func TestCatalogue_NomadDefaultJQ(t *testing.T) {
	t.Parallel()
	load := func(t *testing.T, name string) any {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("testdata", "nomad", name))
		require.NoError(t, err)
		return body
	}
	for _, c := range targettest.Catalogs(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			cat, err := target.Load(c.Path)
			require.NoError(t, err, c.Source)
			p, ok := cat.Profile("nomad")
			require.True(t, ok)
			require.NotEmpty(t, p.DefaultJQ)

			apply := func(t *testing.T, fixture string) any {
				t.Helper()
				body := load(t, fixture).([]byte)
				out, err := ring.ApplyJQ(context.Background(), p.DefaultJQ, body, time.Second)
				require.NoError(t, err, fixture)
				return out
			}

			t.Run("jobs list keeps parents and rolls dispatch children up", func(t *testing.T) {
				out := apply(t, "jobs.json").(map[string]any)
				jobs := out["Jobs"].([]any)
				assert.Len(t, jobs, 2)
				first := jobs[0].(map[string]any)
				assert.Contains(t, first, "Summary")
				assert.NotContains(t, first, "JobSummary", "the envelope is gone")
				assert.IsType(t, "", first["SubmitTime"], "nanoseconds became a timestamp")
				children := out["DispatchChildren"].([]any)
				require.Len(t, children, 1, "three children of one parent make one row")
				child := children[0].(map[string]any)
				assert.EqualValues(t, 3, child["Count"])
				assert.Contains(t, child, "ByStatus")
				assert.Contains(t, child["Last"], "ID")
			})

			t.Run("allocation list without task states survives the null", func(t *testing.T) {
				out := apply(t, "allocations-nostates.json").([]any)
				require.Len(t, out, 2)
				for _, a := range out {
					assert.Empty(t, a.(map[string]any)["TaskStates"])
					assert.Contains(t, a, "ClientStatus")
				}
			})

			t.Run("job allocations keep the last event of every task", func(t *testing.T) {
				out := apply(t, "job-allocations.json").([]any)
				require.Len(t, out, 1)
				states := out[0].(map[string]any)["TaskStates"].(map[string]any)
				require.NotEmpty(t, states)
				for _, s := range states {
					st := s.(map[string]any)
					assert.Contains(t, st, "State")
					assert.Contains(t, st["LastEvent"], "Type")
					assert.NotContains(t, st, "Events", "the list carries one event per task, the single allocation all of them")
				}
			})

			t.Run("single allocation keeps every event", func(t *testing.T) {
				out := apply(t, "allocation.json").(map[string]any)
				assert.Contains(t, out, "Name")
				states := out["TaskStates"].(map[string]any)
				require.NotEmpty(t, states)
				for _, s := range states {
					events := s.(map[string]any)["Events"].([]any)
					assert.NotEmpty(t, events)
					assert.Contains(t, events[0], "DisplayMessage")
				}
			})

			t.Run("job spec comes down to image and resources", func(t *testing.T) {
				out := apply(t, "job.json").(map[string]any)
				assert.Contains(t, out, "Meta")
				groups := out["TaskGroups"].([]any)
				require.NotEmpty(t, groups)
				task := groups[0].(map[string]any)["Tasks"].([]any)[0].(map[string]any)
				assert.NotEmpty(t, task["Image"])
				assert.Contains(t, task, "MemoryMB")
			})

			t.Run("versions keep what changed between deploys", func(t *testing.T) {
				out := apply(t, "versions.json").(map[string]any)
				versions := out["Versions"].([]any)
				require.Len(t, versions, 2)
				assert.Contains(t, versions[0], "Version")
				assert.NotContains(t, out, "Diffs")
			})

			t.Run("unknown shapes pass untouched", func(t *testing.T) {
				out, err := ring.ApplyJQ(context.Background(), p.DefaultJQ, []byte(`{"Children":{"Dead":0},"JobID":"apisrv"}`), time.Second)
				require.NoError(t, err)
				assert.Equal(t, map[string]any{"Children": map[string]any{"Dead": 0}, "JobID": "apisrv"}, out, "integers decode exactly since the db tools (ring.decodeJSON)")
			})
		})
	}
}

// Dispatch and periodic children carry a slash in the job ID; Nomad takes it
// raw or as %2F, and the allowlist has to take both without opening the paths
// that run, stop or scale a job.
func TestCatalogue_NomadAllowlist(t *testing.T) {
	t.Parallel()
	cat, err := target.Load(targettest.Path(t, targettest.Dev))
	require.NoError(t, err)
	p, ok := cat.Profile("nomad")
	require.True(t, ok)

	for _, path := range []string{
		"/v1/jobs",
		"/v1/allocations?task_states=false&filter=ClientStatus+%3D%3D+%22failed%22",
		"/v1/job/sast-gitsync/dispatch-1788330094-1ab3fcd9",
		"/v1/job/sast-gitsync%2Fdispatch-1788330094-1ab3fcd9/allocations",
		"/v1/job/apisrv/versions",
		"/v1/job/apisrv/summary",
		"/v1/job/apisrv/evaluations",
		"/v1/node/9961de91-9ec9-f106-ae7e-90510628f887",
	} {
		assert.True(t, p.Allows("GET", path), path)
	}
	for _, path := range []string{
		"/v1/job/apisrv/dispatch",
		"/v1/job/apisrv/scale",
		"/v1/job/apisrv/periodic/force",
		"/v1/node/abc/drain",
		"/v1/job/apisrv/dispatch-1/allocations/extra",
	} {
		assert.False(t, p.Allows("GET", path), path)
		assert.False(t, p.Allows("POST", path), path)
	}
}
