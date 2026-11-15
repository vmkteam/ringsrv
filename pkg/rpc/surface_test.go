package rpc

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/kb"
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
			measureSurface(t, cat, [][]string{{"ringsrv-users"}, {"ringsrv-developers"}, {"ringsrv-oncall"}})
		})
	}
}

// The shipped catalogues live in the internal repository, so the test above
// skips in a public checkout. This one runs in every checkout: the example
// catalogue, and an instance of databases alone — the one a knowledge pack is
// for.
func TestToolSurfaceFitsBudget_Example(t *testing.T) {
	t.Parallel()
	cat, err := target.Load(filepath.Join(targettest.Root(t), targettest.Example))
	require.NoError(t, err)
	measureSurface(t, cat, [][]string{{"ringsrv-users"}, {"ringsrv-developers"}, {"ringsrv-oncall"}})

	analyst, err := target.Parse([]byte(analystCatalogTOML))
	require.NoError(t, err)
	measureSurface(t, analyst, [][]string{{"analysts"}})
}

// measureSurface holds tools/list of each role to the budget, with a knowledge
// pack and without: a pack adds a sentence to help and, with a note on a table,
// a word to db_introspect.
func measureSurface(t *testing.T, cat *target.Catalog, roles [][]string) {
	t.Helper()
	// The database tools are part of the surface wherever the catalogue names
	// a database: scripted clients stand in for the drivers.
	m := dbq.NewManager(dbq.Options{})
	for name, d := range cat.Databases {
		require.NoError(t, m.Add(dbq.Target{Name: name, Driver: d.Driver, Client: dbqtest.New(nil), MaxRows: 1, Timeout: time.Second}))
	}
	for _, pack := range []*kb.Pack{nil, surfacePack(t, cat)} {
		s := NewToolsService(ToolsDeps{
			Targets: cat,
			Repos:   git.New(git.Options{ReposDir: t.TempDir(), WorktreesDir: t.TempDir()}),
			Graph:   codegraph.New(codegraph.Options{}),
			DB:      m,
			KB:      pack,
		})
		for _, groups := range roles {
			if cat.Resolve(groups).Empty() {
				continue // this catalogue has no such role
			}
			list, err := s.List(ctxWithGroups(groups...), "")
			require.NoError(t, err)
			b, err := json.Marshal(list)
			require.NoError(t, err)
			t.Logf("%v, pack %v: %d tools, %d bytes", groups, pack != nil, len(list.Tools), len(b))
			for _, tool := range list.Tools {
				tb, err := json.Marshal(tool)
				require.NoError(t, err)
				t.Logf("  %-14s %5d bytes (description %d, schema %d)", tool.Name, len(tb), len(tool.Description), len(tool.InputSchema))
			}
			require.LessOrEqual(t, len(b), MaxToolSurfaceBytes, "tools/list for %v (pack %v) is over the surface budget", groups, pack != nil)
		}
	}
}

// surfacePack is the smallest pack that changes the tool surface: a topic, and a
// note on a table of the catalogue's first database when it has one.
func surfacePack(t *testing.T, cat *target.Catalog) *kb.Pack {
	t.Helper()
	files := map[string]string{"playbook/rules.md": "---\ndescription: Правила\n---\nПравила.\n"}
	sources := map[string]string{}
	if names := slices.Sorted(maps.Keys(cat.Databases)); len(names) > 0 {
		sources["main"] = names[0]
		files["schema/main/orders.md"] = "---\ntable: orders\nsource: main\npurpose: Заказы\n---\n"
	}
	pack, err := kb.Load(kb.Options{Dir: writeKB(t, files), URIScheme: kbScheme, Sources: sources},
		kb.Deps{Catalog: cat, ReservedTopics: ReservedSheetNames(cat)})
	require.NoError(t, err)
	return pack
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
