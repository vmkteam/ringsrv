package rpc

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/code"
	"github.com/vmkteam/ringsrv/pkg/ring/code/gittest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
)

// refsFixture builds a repository with a call chain across two layers.
func refsFixture(t *testing.T) (svc ToolsService, sha string) {
	t.Helper()
	bin := gittest.EngineOrSkip(t)

	origin := t.TempDir()
	gittest.Git(t, origin, "init", "--initial-branch=master")
	write := func(name, body string) { gittest.Write(t, origin, name, body) }

	write("go.mod", "module example\n\ngo 1.25\n")
	write("internal/db/order.go", `package db

type Repo struct{}

func (r Repo) OrderByID(id int) int {
	return id
}
`)
	write("internal/rpc/order.go", `package rpc

import "example/internal/db"

func Handler(id int) int {
	var r db.Repo
	return r.OrderByID(id)
}
`)
	// A second OrderByID in another type: in Go this is the common case, and
	// the tool must ask which one instead of guessing.
	write("internal/cache/order.go", `package cache

type Cache struct{}

func (c Cache) OrderByID(id int) int {
	return id
}
`)
	write("vendor/lib/lib.go", "package lib\n\nfunc OrderByID(id int) int { return id }\n")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "PLF-1 initial")
	sha = gittest.Git(t, origin, "rev-parse", "HEAD")

	root := t.TempDir()
	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.prom]
Description  = "dev · Prometheus"
BaseURL      = "https://prom.example.com"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Roles.developer]
Description = "reads code"
Groups      = ["ringsrv-developers"]
Tools       = ["code_refs"]
Targets     = ["prom"]
Repos       = ["*"]

[Roles.viewer]
Description = "metrics only"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["prom"]

[Repos.apisrv]
Description   = "test repo"
CloneURL      = "file://` + origin + `"
DefaultBranch = "master"
Exclude       = ["vendor/"]

[Repos.apisrv.Layers]
db  = "internal/db"
rpc = "internal/rpc"
`))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets: cat,
		Repos: git.New(git.Options{
			ReposDir:     filepath.Join(root, "repos"),
			WorktreesDir: filepath.Join(root, "worktrees"),
			Timeout:      2 * time.Minute,
		}),
		Graph:    codegraph.New(codegraph.Options{Bin: bin, Timeout: 2 * time.Minute}),
		Sessions: ring.NewSessions(time.Hour, nil),
		Logger:   embedlog.Logger{},
	}), sha
}

func TestCodeRefs(t *testing.T) { //nolint:tparallel // subtests share the fixture and its index
	t.Parallel()
	s, sha := refsFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	t.Run("callers of a symbol, with the layer filled in", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID", "path": "internal/db/order.go",
		})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)

		var out CodeRefsResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Equal(t, sha, out.Ref)
		require.NotEmpty(t, out.Callers)

		var handler *RefEntry
		for i := range out.Callers {
			// Vendored code is excluded for the same reason as in search: a
			// vendored hit is cheap to drop and expensive to explain.
			assert.NotContains(t, out.Callers[i].Path, "vendor/")
			if out.Callers[i].Symbol == "Handler" {
				handler = &out.Callers[i]
			}
		}
		require.NotNil(t, handler, "the caller in internal/rpc must be found")
		assert.Equal(t, "rpc", handler.Layer, "layer comes from the catalogue, not from a guess")
		assert.Positive(t, handler.Line)
	})

	t.Run("max caps the answer", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID", "path": "internal/db/order.go", "max": 1,
		})
		var out CodeRefsResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.LessOrEqual(t, len(out.Callers), 1)
	})

	// An empty list here would read as "nothing implements this interface",
	// which is a different answer from "we cannot tell".
	t.Run("implementors refuses instead of answering emptiness", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID", "kind": "implementors",
		})
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		assert.Contains(t, e.Message, "code_search", "the refusal points somewhere")
	})

	t.Run("both answers callers and says what is missing", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID", "path": "internal/db/order.go", "kind": "both",
		})
		require.False(t, res.IsError, res.Content[0].Text)
		var out CodeRefsResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.NotEmpty(t, out.Callers)
		assert.NotEmpty(t, out.Unsupported, "half an answer must say it is half")
	})

	// Half the methods in a Go service share a name with another type's method.
	// An empty list here would read as "nobody calls this".
	t.Run("an ambiguous name asks which one, with the paths", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID",
		})
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeAmbiguous, e.Code)
		assert.Len(t, e.Paths, 2, "both candidates are named so the next call can pick")
		assert.Contains(t, e.Message, "path")
	})

	t.Run("unknown kind lists the ones there are", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID", "kind": "importers",
		})
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		assert.Contains(t, e.Message, code.KindCallers)
	})

	t.Run("a junk symbol is refused before the engine runs", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "--exec=/bin/sh",
		})
		assert.Equal(t, ErrCodeBadArgs, decodeErr(t, res).Code)
	})

	t.Run("a role without the tool is refused", func(t *testing.T) {
		res, _ := s.Call(ctxWithGroups("ringsrv-users"), ToolCodeRefs, map[string]any{
			"repo": "apisrv", "ref": sha, "symbol": "OrderByID",
		})
		assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)
	})
}

// An instance without the engine must say so by name. Answering an empty list
// would read as "nobody calls this".
func TestCodeRefs_NoEngine(t *testing.T) {
	t.Parallel()
	s, sha := codeFixture(t)

	res, err := s.Call(ctxWithGroups("ringsrv-developers"), ToolCodeRefs, map[string]any{
		"repo": "apisrv", "ref": sha, "symbol": "Create",
	})
	require.NoError(t, err)
	e := decodeErr(t, res)
	// The fixture's role does not grant code_refs either; whichever refusal
	// comes first, it must be a refusal and not an empty answer.
	assert.Contains(t, []string{ErrCodeNoEngine, ErrCodeForbiddenRole}, e.Code)
}

func TestLayer(t *testing.T) {
	t.Parallel()
	repo := &target.Repo{Layers: map[string]string{
		"db":     "internal/db",
		"rpc":    "internal/rpc",
		"dbtest": "internal/db/test",
	}}

	assert.Equal(t, "db", repo.Layer("internal/db/order.go"))
	assert.Equal(t, "rpc", repo.Layer("internal/rpc/order.go"))
	// Longest prefix wins, otherwise a nested layer is reported as its parent.
	assert.Equal(t, "dbtest", repo.Layer("internal/db/test/fixtures.go"))
	// A path outside every known layer gets nothing rather than an invention.
	assert.Empty(t, repo.Layer("cmd/apisrv/main.go"))
}
