package rpc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
	"github.com/vmkteam/ringsrv/pkg/ring/target/targettest"

	"github.com/stretchr/testify/require"
)

// MaxToolSurfaceBytes is the budget for tools/list, in bytes — roughly five
// bytes a token for this mix of Cyrillic prose and ASCII schema.
//
// It has been raised deliberately, each time against what the raise buys: the
// help tool, so the cheat sheets reach Claude Desktop; the SAST database, so a
// base the roles can query is named at all; and batching, where every tool takes
// a list and the api_call schema alone gained ~280 bytes. Each raise was paid
// for by trimming descriptions first, and the alternative was always cutting the
// guidance they carry.
const MaxToolSurfaceBytes = 15360

// The constant cost of the tool surface is paid on every request by every
// client, so it is measured on every shipped catalogue and the widest roles
// rather than estimated: a description that grows past the budget fails a test
// instead of a session.
func TestToolSurfaceFitsBudget(t *testing.T) {
	t.Parallel()
	for _, c := range targettest.Catalogs(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			cat, err := target.Load(c.Path)
			require.NoError(t, err, c.Source)

			// The database tools are part of the surface wherever the
			// catalogue names a database: scripted clients
			// stand in for the drivers.
			m := dbq.NewManager(dbq.Options{})
			for name, d := range cat.Databases {
				require.NoError(t, m.Add(dbq.Target{Name: name, Driver: d.Driver, Client: dbqtest.New(nil), MaxRows: 1, Timeout: time.Second}))
			}
			s := NewToolsService(ToolsDeps{
				Targets: cat,
				Repos:   git.New(git.Options{ReposDir: t.TempDir(), WorktreesDir: t.TempDir()}),
				Graph:   codegraph.New(codegraph.Options{}),
				DB:      m,
			})
			for _, groups := range [][]string{{"ringsrv-users"}, {"ringsrv-developers"}, {"ringsrv-oncall"}} {
				if cat.Resolve(groups).Empty() {
					continue // this catalogue has no such role
				}
				list, err := s.List(ctxWithGroups(groups...), "")
				require.NoError(t, err)
				b, err := json.Marshal(list)
				require.NoError(t, err)
				t.Logf("%v: %d tools, %d bytes", groups, len(list.Tools), len(b))
				for _, tool := range list.Tools {
					tb, err := json.Marshal(tool)
					require.NoError(t, err)
					t.Logf("  %-14s %5d bytes (description %d, schema %d)", tool.Name, len(tb), len(tool.Description), len(tool.InputSchema))
				}
				require.LessOrEqual(t, len(b), MaxToolSurfaceBytes, "tools/list for %v is over the surface budget", groups)
			}
		})
	}
}

// No shipped catalogue uses AllowHeaders yet, so the widest role is never
// measured in the shape the feature produces. The argument costs bytes from the
// moment one target opts in, and the headroom it eats is worth knowing before a
// catalogue edit discovers it.
func TestToolSurfaceFitsBudget_WithHeaders(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "targets.toml"))
	require.NoError(t, err)

	// The widest role's own targets are the ones that would carry it, and
	// Prometheus sits in every role.
	withHeaders := strings.Replace(string(raw), `AllowMethods = ["GET"]`,
		"AllowHeaders = [\"Authorization2\", \"Platform\"]\nAllowMethods = [\"GET\"]", 1)
	cat, err := target.Parse([]byte(withHeaders))
	require.NoError(t, err)

	m := dbq.NewManager(dbq.Options{})
	for name, d := range cat.Databases {
		require.NoError(t, m.Add(dbq.Target{Name: name, Driver: d.Driver, Client: dbqtest.New(nil), MaxRows: 1, Timeout: time.Second}))
	}
	s := NewToolsService(ToolsDeps{
		Targets: cat,
		Repos:   git.New(git.Options{ReposDir: t.TempDir(), WorktreesDir: t.TempDir()}),
		Graph:   codegraph.New(codegraph.Options{}),
		DB:      m,
	})

	list, err := s.List(ctxWithGroups("ringsrv-oncall"), "")
	require.NoError(t, err)
	b, err := json.Marshal(list)
	require.NoError(t, err)

	t.Logf("oncall with headers: %d bytes of %d", len(b), MaxToolSurfaceBytes)
	require.LessOrEqual(t, len(b), MaxToolSurfaceBytes,
		"a target that opts into AllowHeaders must still fit the surface budget")
}
