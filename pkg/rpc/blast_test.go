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

// blastFixture rebuilds the shape of the incident the tool was measured on:
// a release touches several files, one of them the
// function a stack trace points into, and the frames have to pick that one out.
func blastFixture(t *testing.T) (svc ToolsService, prev, release string) {
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
	write("internal/rpc/product.go", `package rpc

func PriceTotal(ids []int) int {
	total := 0
	for _, id := range ids {
		total += id
	}
	return total
}

func Untouched(a int) int {
	return a
}
`)
	// A file whose changes land both inside a function and outside every symbol:
	// package-level SQL and templates carry a good deal of behaviour, and a
	// release that changes one can break production.
	write("internal/rpc/cache.go", `package rpc

const (
	sqlOrders = `+"`select 1`"+`
)

func CacheKey(a int) string {
	return sqlOrders
}
`)
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-1 initial")
	prev = gittest.Git(t, origin, "rev-parse", "HEAD")

	// The release: the guilty change lands in PriceTotal, an innocent
	// one in another file. Only the frames can tell them apart.
	write("internal/rpc/product.go", `package rpc

func PriceTotal(ids []int) int {
	total := 0
	for _, id := range ids {
		total += id * 2
	}
	return total
}

func Untouched(a int) int {
	return a
}
`)
	write("internal/db/order.go", `package db

type Repo struct{}

func (r Repo) OrderByID(id int) int {
	return id + 1
}
`)
	write("internal/rpc/added.go", `package rpc

func BrandNew(a int) int {
	return a * 3
}
`)
	write("internal/rpc/cache.go", `package rpc

const (
	sqlOrders = `+"`select 2`"+`
)

func CacheKey(a int) string {
	return sqlOrders + "x"
}
`)
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-2 double the ids")
	release = gittest.Git(t, origin, "rev-parse", "HEAD")

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
Tools       = ["blast_radius"]
Targets     = ["prom"]
Repos       = ["*"]

[Repos.apisrv]
Description   = "test repo"
CloneURL      = "file://` + origin + `"
DefaultBranch = "master"
TaskIDRegexp  = "^([A-Z]+-\\d+)"

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
	}), prev, release
}

func TestBlastRadius(t *testing.T) { //nolint:tparallel // subtests share the fixture and its index
	t.Parallel()
	s, prev, release := blastFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	ask := func(t *testing.T, args map[string]any) BlastRadiusResult {
		t.Helper()
		res, err := s.Call(ctx, ToolBlastRadius, args)
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out BlastRadiusResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		return out
	}

	// The hypothesis itself: a release changed several symbols, the trace names
	// one line, and what is left is the cause.
	t.Run("frames cut the release down to the cause", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames": []string{"internal/rpc/product.go:6"},
		})
		assert.Equal(t, release, out.Release)
		assert.Equal(t, 1, out.Counts.Commits)
		assert.Positive(t, out.Counts.Symbols, "the release changed something")

		require.Len(t, out.Intersection, 1, "one frame, one symbol")
		hit := out.Intersection[0]
		assert.Equal(t, "PriceTotal", hit.Symbol)
		assert.Equal(t, "internal/rpc/product.go", hit.Path)
		assert.Equal(t, code.ChangeBody, hit.Change, "one line inside an existing function is a body change")
		assert.Equal(t, "rpc", hit.Layer)
		assert.Equal(t, "internal/rpc/product.go:6", hit.Frame, "the answer says which frame matched")
		require.NotNil(t, hit.Task)
		assert.Equal(t, "ABC-2", *hit.Task, "the task comes from the commit that touched the file")

		// The other changed symbol is real but not in the trace, so it stays
		// out of the answer and lives in the context list.
		assert.Less(t, len(out.Intersection), out.Counts.Symbols)
	})

	// A function that appeared in this release is not a function whose
	// signature changed: one explains a new failure mode, the other explains a
	// caller that broke.
	t.Run("a symbol added whole is added, not signature", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames": []string{"internal/rpc/added.go:3"},
		})
		require.Len(t, out.Intersection, 1)
		assert.Equal(t, code.ChangeAdded, out.Intersection[0].Change)
	})

	// Without frames the tool cannot answer, and it says so rather than
	// pretending a list of everything is an answer.
	t.Run("no frames means the whole list, and the note says so", func(t *testing.T) {
		out := ask(t, map[string]any{"repo": "apisrv", "release": release, "prev": prev})
		assert.Empty(t, out.Intersection)
		assert.NotEmpty(t, out.Changed)
		assert.Contains(t, out.Note, "frames")
	})

	// "The release did not touch what broke" is a real answer worth giving
	// clearly — but only if the frames actually matched something.
	t.Run("a frame in untouched code gives an empty intersection", func(t *testing.T) {
		res, err := s.Call(ctx, ToolBlastRadius, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames": []string{"internal/rpc/product.go:12"},
		})
		require.NoError(t, err)
		// Empty must look empty on the wire: a null here reads as "this was
		// not computed", which is a different and wrong answer.
		assert.Contains(t, res.Content[0].Text, `"intersection":[]`)

		var out BlastRadiusResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Empty(t, out.Intersection)
		assert.Equal(t, 1, out.Counts.UnmatchedFrames)
		assert.NotEmpty(t, out.Note)
	})

	// Found by replaying closed incidents: a release that changed both a
	// function and a package-level constant reported only the function, so a
	// frame pointing at the constant matched nothing and the answer read "this
	// release did not touch the code that broke" — about a file it had changed.
	t.Run("a change outside every symbol is still reported", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames": []string{"internal/rpc/cache.go:4"},
		})
		require.Len(t, out.Intersection, 1)
		hit := out.Intersection[0]
		assert.Empty(t, hit.Symbol, "no symbol owns a package-level const")
		assert.Equal(t, code.KindFile, hit.Kind, "and the kind says so instead of leaving a nameless row")
		assert.Equal(t, "internal/rpc/cache.go", hit.Path)
		require.NotNil(t, hit.Task)
		assert.Equal(t, "ABC-2", *hit.Task)
		assert.Zero(t, out.Counts.UnmatchedFrames)
	})

	// The file-level row must not tag along when a symbol answers, or every
	// real hit would come with a nameless twin.
	t.Run("a symbol beats the file-level row for the same file", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames": []string{"internal/rpc/cache.go:8"},
		})
		require.Len(t, out.Intersection, 1)
		assert.Equal(t, "CacheKey", out.Intersection[0].Symbol)
	})

	// Sentry reports build paths, a Go panic reports module paths, neither is
	// repository-relative. Matching on the tail is what makes them line up.
	t.Run("a frame with a build path prefix still matches", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames": []string{"/builds/acme/apisrv/internal/rpc/product.go:6"},
		})
		require.Len(t, out.Intersection, 1)
		assert.Equal(t, "PriceTotal", out.Intersection[0].Symbol)
		assert.Zero(t, out.Counts.UnmatchedFrames)
	})

	t.Run("a frame from another service is counted, not silently dropped", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames": []string{"vendor/other/lib.go:10", "internal/rpc/product.go:6"},
		})
		assert.Len(t, out.Intersection, 1)
		assert.Equal(t, 1, out.Counts.UnmatchedFrames, "the unmatched frame is visible in the counts")
	})

	t.Run("max_symbols caps the intersection", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "release": release, "prev": prev,
			"frames":      []string{"internal/rpc/product.go:6", "internal/db/order.go:6"},
			"max_symbols": 1,
		})
		assert.Len(t, out.Intersection, 1)
		assert.Equal(t, 2, out.Counts.Intersection, "the count stays honest about what was cut")
	})

	// A release with nothing in it and an unknown release are different
	// answers, and confusing them sends an investigation the wrong way.
	t.Run("a release with no commits is not an error", func(t *testing.T) {
		out := ask(t, map[string]any{"repo": "apisrv", "release": release, "prev": release})
		assert.Empty(t, out.Intersection)
		assert.Zero(t, out.Counts.Commits)
		assert.NotEmpty(t, out.Note)
	})

	t.Run("an unknown release is a different error", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolBlastRadius, map[string]any{
			"repo": "apisrv", "release": "0000000000000000000000000000000000000000", "prev": prev,
		})
		assert.Equal(t, ErrCodeRefUnknown, decodeErr(t, res).Code)
	})

	// ringsrv does not call Sentry on the user's behalf: it says where to get
	// the value instead of guessing one.
	t.Run("prev is required and the refusal says where to get it", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolBlastRadius, map[string]any{"repo": "apisrv", "release": release})
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeNoPrev, e.Code)
		assert.Contains(t, e.Message, "api_call")
	})
}

// blastFixture2 holds the shapes the first fixture could not: a deleted
// function, a file that only moved, two methods of one name in one file, two
// tasks in one file, and a file whose only change is far from the frame. Each of
// these used to produce a wrong row.
func blastFixture2(t *testing.T) (svc ToolsService, prev, release string) {
	t.Helper()
	bin := gittest.EngineOrSkip(t)

	origin := t.TempDir()
	gittest.Git(t, origin, "init", "--initial-branch=master")
	write := func(name, body string) { gittest.Write(t, origin, name, body) }

	write("go.mod", "module example\n\ngo 1.25\n")
	write("internal/rpc/names.go", `package rpc

type A struct{}

func (a A) String() string {
	return "a"
}

type B struct{}

func (b B) String() string {
	return "b"
}

func Gone(x int) int {
	return x
}
`)
	write("internal/rpc/old.go", `package rpc

func Moved(x int) int {
	return x + 1
}
`)
	write("internal/rpc/two.go", `package rpc

func First(x int) int {
	return x
}

func Second(x int) int {
	return x
}
`)
	write("internal/rpc/weak.go", `package rpc

const limit = 10

func Stable(x int) int {
	return x + limit
}
`)
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-1 initial")
	prev = gittest.Git(t, origin, "rev-parse", "HEAD")

	// Second commit: one task touches First only.
	write("internal/rpc/two.go", `package rpc

func First(x int) int {
	return x * 2
}

func Second(x int) int {
	return x
}
`)
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-2 double first")

	// Third commit, another task: Second changes, Gone is deleted, B.String
	// changes, old.go only moves, and weak.go changes a constant far from
	// Stable.
	write("internal/rpc/two.go", `package rpc

func First(x int) int {
	return x * 2
}

func Second(x int) int {
	return x * 3
}
`)
	write("internal/rpc/names.go", `package rpc

type A struct{}

func (a A) String() string {
	return "a"
}

type B struct{}

func (b B) String() string {
	return "bee"
}
`)
	write("internal/rpc/weak.go", `package rpc

const limit = 20

func Stable(x int) int {
	return x + limit
}
`)
	gittest.Git(t, origin, "mv", "internal/rpc/old.go", "internal/rpc/moved.go")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-3 triple second, drop Gone")
	release = gittest.Git(t, origin, "rev-parse", "HEAD")

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
Tools       = ["blast_radius"]
Targets     = ["prom"]
Repos       = ["*"]

[Repos.apisrv]
Description   = "test repo"
CloneURL      = "file://` + origin + `"
DefaultBranch = "master"
TaskIDRegexp  = "^([A-Z]+-\\d+)"
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
	}), prev, release
}

func TestBlastRadius_Shapes(t *testing.T) { //nolint:tparallel // subtests share the fixture and its index
	t.Parallel()
	s, prev, release := blastFixture2(t)
	ctx := ctxWithGroups("ringsrv-developers")

	ask := func(t *testing.T, frames ...string) BlastRadiusResult {
		t.Helper()
		args := map[string]any{"repo": "apisrv", "release": release, "prev": prev}
		if len(frames) > 0 {
			args["frames"] = frames
		}
		res, err := s.Call(ctx, ToolBlastRadius, args)
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out BlastRadiusResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		return out
	}
	find := func(rows []ChangedSymbol, path string, pred func(ChangedSymbol) bool) *ChangedSymbol {
		for i := range rows {
			if rows[i].Path == path && pred(rows[i]) {
				return &rows[i]
			}
		}
		return nil
	}

	// A deletion hunk names the line the removed block used to follow, and
	// that line belongs to the function *before* the deleted one. B.String was
	// being reported as removed for the deletion of Gone.
	t.Run("a deleted function does not brand its neighbour as removed", func(t *testing.T) {
		out := ask(t, "internal/rpc/names.go:12")
		require.Len(t, out.Intersection, 1)
		hit := out.Intersection[0]
		assert.Equal(t, "String", hit.Symbol)
		assert.Equal(t, "B", hit.Receiver, "the receiver tells the two String methods apart")
		assert.Equal(t, code.ChangeBody, hit.Change, "B.String changed its body; it was not removed")
		assert.Equal(t, code.MatchSymbol, hit.Match)

		for _, row := range out.Changed {
			if row.Change == code.ChangeRemoved {
				assert.Zero(t, row.Line, "a removal never points at a line of the release tree")
			}
		}
		gone := find(out.Changed, "internal/rpc/names.go", func(r ChangedSymbol) bool { return r.Symbol == "Gone" })
		require.NotNil(t, gone, "the deleted function is named, read from the previous tree")
		assert.Equal(t, code.ChangeRemoved, gone.Change)
		assert.Zero(t, gone.Line, "and carries no line: there is none in the release tree")
		assert.Nil(t, find(out.Changed, "internal/rpc/names.go", func(r ChangedSymbol) bool { return r.Kind == code.KindFile }),
			"the deletion is explained by the symbol, so no file-level row tags along")
	})

	// Two String methods, two rows — the second used to be folded into the
	// first, and a frame in it fell through to the file.
	t.Run("methods of one name in one file are two rows", func(t *testing.T) {
		out := ask(t, "internal/rpc/names.go:12")
		a := find(out.Changed, "internal/rpc/names.go", func(r ChangedSymbol) bool { return r.Receiver == "A" })
		assert.Nil(t, a, "A.String did not change")
		b := find(out.Changed, "internal/rpc/names.go", func(r ChangedSymbol) bool { return r.Receiver == "B" })
		require.NotNil(t, b)
		assert.Equal(t, "String", b.Symbol)
	})

	// A file that only moved has no changed lines. Without rename detection it
	// was a deletion plus an addition, and every function in it read as new.
	t.Run("a moved file is renamed, not added whole", func(t *testing.T) {
		out := ask(t, "internal/rpc/moved.go:3")
		require.Len(t, out.Intersection, 1)
		hit := out.Intersection[0]
		assert.Equal(t, code.KindFile, hit.Kind)
		assert.Equal(t, code.ChangeRenamed, hit.Change)
		assert.Equal(t, code.MatchFile, hit.Match, "the frame line did not change, so this is the weakest match")
		assert.Empty(t, hit.Symbol)
		assert.Nil(t, find(out.Changed, "internal/rpc/moved.go", func(r ChangedSymbol) bool { return r.Symbol == "Moved" }),
			"Moved was not added by this release")
		assert.Equal(t, 1, out.Counts.WeakMatches)
		assert.Contains(t, out.Note, "слабые")
	})

	// Two tasks landed in one file. The symbol of the first used to carry the
	// key of the second, because the task came from the file's last commit.
	t.Run("the task comes from the hunk, not from the file's last commit", func(t *testing.T) {
		out := ask(t, "internal/rpc/two.go:4", "internal/rpc/two.go:8")
		require.Len(t, out.Intersection, 2)
		byName := map[string]ChangedSymbol{}
		for _, r := range out.Intersection {
			byName[r.Symbol] = r
		}
		require.NotNil(t, byName["First"].Task)
		assert.Equal(t, "ABC-2", *byName["First"].Task)
		require.NotNil(t, byName["Second"].Task)
		assert.Equal(t, "ABC-3", *byName["Second"].Task)
	})

	// The release changed a constant the frame's function reads, but not the
	// function. The row is still reported — the constant may be the cause —
	// graded so it cannot be mistaken for a symbol hit.
	t.Run("a frame in an unchanged function of a changed file is a weak match", func(t *testing.T) {
		out := ask(t, "internal/rpc/weak.go:6")
		require.Len(t, out.Intersection, 1)
		assert.Equal(t, code.MatchFile, out.Intersection[0].Match)
		assert.Equal(t, code.KindFile, out.Intersection[0].Kind)
		assert.Equal(t, 1, out.Counts.WeakMatches)
		assert.Contains(t, out.Note, "слабые")

		// The constant itself is a hit on a line no symbol owns.
		out = ask(t, "internal/rpc/weak.go:3")
		require.Len(t, out.Intersection, 1)
		assert.Equal(t, code.MatchHunk, out.Intersection[0].Match)
		assert.Zero(t, out.Counts.WeakMatches)
		assert.NotContains(t, out.Note, "слабые")
	})
}
