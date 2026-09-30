package rpc

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/code"
	"github.com/vmkteam/ringsrv/pkg/ring/code/gittest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
)

// historyFixture builds a repository with three commits — two carrying an
// issue key and one without — and a service wired to it. Real git again: the
// modes are argv, and a fake would test the fake.
func historyFixture(t *testing.T) (svc ToolsService, first, second, third string) {
	t.Helper()

	origin := t.TempDir()
	gittest.Git(t, origin, "init", "--initial-branch=master")
	write := func(name, body string) { gittest.Write(t, origin, name, body) }

	write("internal/rpc/order.go", "package rpc\n\nfunc Create() error {\n\treturn nil\n}\n")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-1 initial")
	first = gittest.Git(t, origin, "rev-parse", "HEAD")

	write("internal/rpc/order.go", "package rpc\n\nfunc Create() error {\n\tctxTimeout := 5\n\t_ = ctxTimeout\n\treturn nil\n}\n")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-2 add timeout")
	second = gittest.Git(t, origin, "rev-parse", "HEAD")

	write("README.md", "# apisrv\n")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "readme")
	third = gittest.Git(t, origin, "rev-parse", "HEAD")

	return newHistoryService(t, origin, ""), first, second, third
}

// newHistoryService wires a service to origin. gitBin overrides the binary,
// which is how a test proves a call never reached git.
func newHistoryService(t *testing.T, origin, gitBin string) ToolsService {
	t.Helper()
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
Tools       = ["code_history"]
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
Exclude       = ["*.pem"]
TaskIDRegexp  = "^([A-Z]+-\\d+)"
`))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets: cat,
		Repos: git.New(git.Options{
			Git:          gitBin,
			ReposDir:     filepath.Join(root, "repos"),
			WorktreesDir: filepath.Join(root, "worktrees"),
			Timeout:      time.Minute,
		}),
		Sessions: ring.NewSessions(time.Hour, nil),
		Logger:   embedlog.Logger{},
	})
}

func TestCodeHistory(t *testing.T) { //nolint:tparallel // subtests share the fixture
	t.Parallel()
	s, first, second, third := historyFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	ask := func(t *testing.T, args map[string]any) CodeHistoryResult {
		t.Helper()
		res, err := s.Call(ctx, ToolCodeHistory, args)
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out CodeHistoryResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Equal(t, args["mode"], out.Mode)
		return out
	}

	// The mode the whole tool exists for: the stack trace points at lines, and
	// the answer has to be about those lines rather than about the file.
	t.Run("lines: who touched this range", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "mode": code.ModeLines, "ref": third,
			"path": "internal/rpc/order.go", "line_from": 3, "line_to": 5,
		})
		require.Len(t, out.Commits, 2, "both commits touched the body of Create")
		assert.Equal(t, second, out.Commits[0].SHA, "newest first")
		require.NotNil(t, out.Commits[0].Task)
		assert.Equal(t, "ABC-2", *out.Commits[0].Task)
	})

	t.Run("blame: who wrote this line", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "mode": code.ModeBlame, "ref": third,
			"path": "internal/rpc/order.go", "line": 4,
		})
		require.Len(t, out.Commits, 1)
		assert.Equal(t, second, out.Commits[0].SHA)
		assert.Equal(t, "t", out.Commits[0].Author)
		// blame reports the date as a unix timestamp; every mode must answer in
		// one shape, or a caller comparing it to a Sentry timestamp has to know
		// which mode produced it.
		_, err := time.Parse(time.RFC3339, out.Commits[0].Date)
		assert.NoError(t, err, "date %q", out.Commits[0].Date)
	})

	t.Run("contains: which branches hold the commit", func(t *testing.T) {
		out := ask(t, map[string]any{"repo": "apisrv", "mode": code.ModeContains, "sha": second})
		assert.Contains(t, out.Branches, "master")
		// D10: an empty tag list must not read as "released nowhere".
		assert.NotEmpty(t, out.Note, "the answer says why there are no tags")
	})

	t.Run("range: what went in between two commits", func(t *testing.T) {
		out := ask(t, map[string]any{"repo": "apisrv", "mode": code.ModeRange, "from": first, "to": third})
		require.Len(t, out.Commits, 2)
		assert.Equal(t, third, out.Commits[0].SHA)

		withFiles := ask(t, map[string]any{
			"repo": "apisrv", "mode": code.ModeRange, "from": first, "to": third, "with_files": true,
		})
		require.Len(t, withFiles.Commits, 2)
		assert.Equal(t, []string{"README.md"}, withFiles.Commits[0].Files)
		assert.Equal(t, []string{"internal/rpc/order.go"}, withFiles.Commits[1].Files)

		narrowed := ask(t, map[string]any{
			"repo": "apisrv", "mode": code.ModeRange, "from": first, "to": third,
			"path": "internal/rpc/order.go",
		})
		require.Len(t, narrowed.Commits, 1, "a path narrows the range to that file")
		assert.Equal(t, second, narrowed.Commits[0].SHA)
	})

	t.Run("pickaxe: when the string appeared", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "mode": code.ModePickaxe, "from": first, "to": third, "query": "ctxTimeout",
		})
		require.Len(t, out.Commits, 1)
		assert.Equal(t, second, out.Commits[0].SHA)
	})

	// A commit that follows no convention has no task, and the answer says so
	// rather than leaving the field out for the model to fill in.
	t.Run("a commit without a key gives task: null", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeHistory, map[string]any{
			"repo": "apisrv", "mode": code.ModeRange, "from": second, "to": third,
		})
		require.NoError(t, err)
		assert.Contains(t, res.Content[0].Text, `"task":null`)

		var out CodeHistoryResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		require.Len(t, out.Commits, 1)
		assert.Nil(t, out.Commits[0].Task)
	})

	t.Run("short sha works and the full one comes back", func(t *testing.T) {
		out := ask(t, map[string]any{"repo": "apisrv", "mode": code.ModeContains, "sha": second[:8]})
		assert.Equal(t, second, out.Ref)
	})

	t.Run("unknown mode lists the available ones", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{"repo": "apisrv", "mode": "log"})
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		for _, mode := range code.Modes {
			assert.Contains(t, e.Message, mode)
		}
	})

	// blame of a file that does not exist at this commit answers with emptiness
	// rather than an error, and the two must not read the same.
	t.Run("a file missing at that commit is an error, not an empty answer", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{
			"repo": "apisrv", "mode": code.ModeBlame, "ref": first, "path": "README.md", "line": 1,
		})
		assert.Equal(t, ErrCodeRefUnknown, decodeErr(t, res).Code)
	})

	t.Run("an excluded path is refused", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{
			"repo": "apisrv", "mode": code.ModeBlame, "ref": third, "path": "secrets/server.pem", "line": 1,
		})
		assert.Equal(t, ErrCodePathDenied, decodeErr(t, res).Code)
	})

	t.Run("a role without the tool is refused", func(t *testing.T) {
		res, _ := s.Call(ctxWithGroups("ringsrv-users"), ToolCodeHistory, map[string]any{
			"repo": "apisrv", "mode": code.ModeContains, "sha": second,
		})
		assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)
	})
}

// Nothing the model writes becomes an argument of git. The mode picks
// the command, and both the ref and the path are validated before they are
// spelled onto a command line.
func TestCodeHistory_NoArgumentReachesArgv(t *testing.T) { //nolint:tparallel // subtests share the fixture
	t.Parallel()
	s, _, _, third := historyFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	refs := []string{"--exec=/bin/sh", "-c", "../../etc/passwd", "master master", "master; rm -rf /", "--upload-pack=sh"}
	for _, ref := range refs {
		t.Run("ref/"+ref, func(t *testing.T) {
			res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{
				"repo": "apisrv", "mode": code.ModeBlame, "ref": ref, "path": "internal/rpc/order.go", "line": 1,
			})
			e := decodeErr(t, res)
			assert.Contains(t, []string{ErrCodeBadArgs, ErrCodeRefUnknown}, e.Code, "message: %s", e.Message)
		})
	}

	paths := []string{"--output=/tmp/x", "/etc/passwd", "../secrets", "internal/../../etc"}
	for _, p := range paths {
		t.Run("path/"+p, func(t *testing.T) {
			res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{
				"repo": "apisrv", "mode": code.ModeLines, "ref": third, "path": p, "line_from": 1, "line_to": 2,
			})
			e := decodeErr(t, res)
			assert.Equal(t, ErrCodePathDenied, e.Code, "message: %s", e.Message)
		})
	}

	// -L glues the range and the path into one argument, which is the one place
	// a number could carry something else.
	t.Run("a line range outside 1..n is refused", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{
			"repo": "apisrv", "mode": code.ModeLines, "ref": third,
			"path": "internal/rpc/order.go", "line_from": 9, "line_to": 2,
		})
		assert.Equal(t, ErrCodePathDenied, decodeErr(t, res).Code)
	})

	t.Run("contains still needs a resolvable sha", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{
			"repo": "apisrv", "mode": code.ModeContains, "sha": "0000000000000000000000000000000000000000",
		})
		assert.Equal(t, ErrCodeRefUnknown, decodeErr(t, res).Code)
	})
}

// Pickaxe over the whole history costs tens of seconds against half a second
// for a range, so the range is checked before git is called —
// and this test proves it by pointing the service at a git that does not exist.
func TestCodeHistory_PickaxeNeedsRangeBeforeGit(t *testing.T) {
	t.Parallel()
	s := newHistoryService(t, t.TempDir(), "git-that-does-not-exist")
	ctx := ctxWithGroups("ringsrv-developers")

	res, _ := s.Call(ctx, ToolCodeHistory, map[string]any{
		"repo": "apisrv", "mode": code.ModePickaxe, "query": "ctxTimeout",
	})
	e := decodeErr(t, res)
	assert.Equal(t, ErrCodeBadArgs, e.Code)
	assert.Contains(t, e.Message, "from and to")

	// The same call with a range does reach git — otherwise the check above
	// would pass for the wrong reason.
	res, _ = s.Call(ctx, ToolCodeHistory, map[string]any{
		"repo": "apisrv", "mode": code.ModePickaxe, "query": "ctxTimeout", "from": "master~1", "to": "master",
	})
	assert.Equal(t, ErrCodeUpstream, decodeErr(t, res).Code)
}

func TestList_CodeHistory(t *testing.T) {
	t.Parallel()
	s, _, _, _ := historyFixture(t)

	tools, err := s.List(ctxWithGroups("ringsrv-developers"), "")
	require.NoError(t, err)
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, ToolCodeHistory)
}
