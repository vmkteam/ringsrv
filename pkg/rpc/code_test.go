package rpc

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/code/gittest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
)

// codeFixture builds a repository with two commits and a service wired to it.
// The tests run against real git, because that is what the tools run against.
func codeFixture(t *testing.T) (svc ToolsService, sha string) {
	t.Helper()
	return codeFixtureLog(t, embedlog.Logger{})
}

// codeFixtureLog is codeFixture with a logger of the caller's choosing, for
// the tests that read the audit record.
func codeFixtureLog(t *testing.T, l embedlog.Logger) (svc ToolsService, sha string) {
	t.Helper()

	origin := t.TempDir()
	gittest.Git(t, origin, "init", "--initial-branch=master")
	write := func(name, body string) { gittest.Write(t, origin, name, body) }
	write("internal/rpc/order.go", "package rpc\n\nfunc Create() error {\n\treturn nil\n}\n\nfunc Cancel() error {\n\treturn nil\n}\n")
	write("vendor/lib/lib.go", "package lib\n\nfunc Create() {}\n")
	write("secrets/server.pem", "-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----\n")
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
Tools       = ["api_call", "code_read", "code_search"]
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
Exclude       = ["vendor/", "*.pem"]
`))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets: cat,
		Repos: git.New(git.Options{
			ReposDir:     filepath.Join(root, "repos"),
			WorktreesDir: filepath.Join(root, "worktrees"),
			Timeout:      time.Minute,
		}),
		Sessions: ring.NewSessions(time.Hour, nil),
		Logger:   l,
	}), sha
}

// The subtests share one fixture and run in order: cloning a repository per
// case would triple the runtime for nothing.
func TestCodeRead(t *testing.T) { //nolint:tparallel // subtests share the fixture
	t.Parallel()
	s, sha := codeFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	t.Run("reads the file at that commit, numbered", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go",
		})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)

		var out CodeReadResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Equal(t, sha, out.Ref, "the answer says which commit it is about")
		assert.Equal(t, 1, out.LineFrom)
		require.NotEmpty(t, out.Lines)
		// Line numbers are not decoration: without them the reference
		// path:line@sha cannot be assembled.
		assert.True(t, strings.HasPrefix(out.Lines[0], "1: "), "got %q", out.Lines[0])
	})

	t.Run("short sha works", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha[:8], "path": "internal/rpc/order.go",
		})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)

		var out CodeReadResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Equal(t, sha, out.Ref, "the short form is resolved once and the full one travels on")
	})

	t.Run("line window and max_lines", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go",
			"line_from": 3, "line_to": 5, "max_lines": 2,
		})
		require.NoError(t, err)
		var out CodeReadResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Len(t, out.Lines, 2)
		assert.True(t, out.Truncated)
		assert.True(t, strings.HasPrefix(out.Lines[0], "3: "))
	})

	// Exclude does two jobs, and this is the one that matters: a key must not
	// leave the disk even when the path is spelled out.
	t.Run("excluded path is refused", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "secrets/server.pem",
		})
		require.NoError(t, err)
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodePathDenied, e.Code)
	})

	t.Run("unknown ref and unknown path are different errors", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": "0000000000000000000000000000000000000000", "path": "internal/rpc/order.go",
		})
		assert.Equal(t, ErrCodeRefUnknown, decodeErr(t, res).Code)

		res, _ = s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/nope.go",
		})
		assert.Equal(t, ErrCodeRefUnknown, decodeErr(t, res).Code, "missing file at a known commit")
	})

	t.Run("ref is required", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRead, map[string]any{"repo": "apisrv", "path": "internal/rpc/order.go"})
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		assert.Contains(t, e.Message, "deployed commit")
	})

	t.Run("a role without the tool is refused", func(t *testing.T) {
		res, _ := s.Call(ctxWithGroups("ringsrv-users"), ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go",
		})
		assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)
	})
}

func TestCodeSearch(t *testing.T) { //nolint:tparallel // subtests share the fixture
	t.Parallel()
	s, sha := codeFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	search := func(t *testing.T, args map[string]any) CodeSearchResult {
		t.Helper()
		args["ref"] = sha
		res, err := s.Call(ctx, ToolCodeSearch, args)
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out CodeSearchResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		return out
	}

	t.Run("literal search finds the line", func(t *testing.T) {
		out := search(t, map[string]any{"query": "func Create"})
		require.NotEmpty(t, out.Matches)
		assert.Contains(t, out.Matches[0], "apisrv internal/rpc/order.go:3:")
	})

	// Vendored code would drown the answer: three quarters of the measured
	// index was constants from vendor/.
	t.Run("excluded paths never appear", func(t *testing.T) {
		out := search(t, map[string]any{"query": "func Create"})
		for _, m := range out.Matches {
			assert.NotContains(t, m, "vendor/")
		}
	})

	t.Run("regexp search", func(t *testing.T) {
		out := search(t, map[string]any{"query": "func (Create|Cancel)", "regex": true})
		assert.Len(t, out.Matches, 2)
	})

	t.Run("path glob narrows", func(t *testing.T) {
		out := search(t, map[string]any{"query": "package", "path_glob": "internal/rpc/order.go"})
		require.NotEmpty(t, out.Matches)
		for _, m := range out.Matches {
			assert.Contains(t, m, "internal/rpc/order.go")
		}
	})

	// Nothing found is an answer — "no such string in this commit" — not a
	// failure to report as one.
	t.Run("no match is an empty answer, not an error", func(t *testing.T) {
		out := search(t, map[string]any{"query": "ThisStringDoesNotExistAnywhere"})
		assert.Empty(t, out.Matches)
	})

	t.Run("max_matches caps and says so", func(t *testing.T) {
		out := search(t, map[string]any{"query": "func", "max_matches": 1})
		assert.Len(t, out.Matches, 1)
		assert.True(t, out.Truncated)
	})

	// Context is what makes a follow-up code_read unnecessary — as long as
	// the answer says which line is the match and the cap counts matches.
	t.Run("context lines are marked like git grep and do not eat the cap", func(t *testing.T) {
		out := search(t, map[string]any{"query": "func Cancel", "context": 1})
		require.Len(t, out.Matches, 3)
		assert.Equal(t, "apisrv internal/rpc/order.go-6- ", out.Matches[0])
		assert.Equal(t, "apisrv internal/rpc/order.go:7: func Cancel() error {", out.Matches[1])
		assert.Equal(t, "apisrv internal/rpc/order.go-8- \treturn nil", out.Matches[2])

		out = search(t, map[string]any{"query": "func", "context": 1, "max_matches": 1})
		hits := 0
		for _, m := range out.Matches {
			if strings.Contains(m, ".go:") {
				hits++
			}
		}
		assert.Equal(t, 1, hits)
		assert.True(t, out.Truncated)
	})
}

// Both tools show up only when the role has repositories and the instance has
// git — the same condition on which they refuse a direct call.
func TestList_CodeTools(t *testing.T) {
	t.Parallel()
	s, _ := codeFixture(t)

	dev, err := s.List(ctxWithGroups("ringsrv-developers"), "")
	require.NoError(t, err)
	names := make([]string, 0, len(dev.Tools))
	for _, tool := range dev.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, ToolCodeRead)
	assert.Contains(t, names, ToolCodeSearch)

	viewer, err := s.List(ctxWithGroups("ringsrv-users"), "")
	require.NoError(t, err)
	for _, tool := range viewer.Tools {
		assert.NotEqual(t, ToolCodeRead, tool.Name, "a role without the tool never sees it")
	}
}

// searchFixture is two repositories, a and b, each with a commit of its own:
// one SHA lives in one repository, and a search over both finds it in a alone.
func searchFixture(t *testing.T) (svc ToolsService, shaA string) {
	t.Helper()
	newOrigin := func(file, body, subject string) (string, string) {
		origin := t.TempDir()
		gittest.Git(t, origin, "init", "--initial-branch=master")
		gittest.Write(t, origin, file, body)
		gittest.Git(t, origin, "add", ".")
		gittest.Git(t, origin, "commit", "-m", subject)
		return origin, gittest.Git(t, origin, "rev-parse", "HEAD")
	}
	originA, shaA := newOrigin("a.go", "package a\n\nfunc Needle() {}\n", "PLF-1 a")
	originB, _ := newOrigin("b.go", "package b\n\nfunc Needle() {}\n", "PLF-2 b")

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
Tools       = ["api_call", "code_search"]
Targets     = ["prom"]
Repos       = ["*"]

[Repos.a]
Description   = "repo a"
CloneURL      = "file://` + originA + `"
DefaultBranch = "master"

[Repos.b]
Description   = "repo b"
CloneURL      = "file://` + originB + `"
DefaultBranch = "master"
`))
	require.NoError(t, err)
	return NewToolsService(ToolsDeps{
		Targets: cat,
		Repos: git.New(git.Options{
			ReposDir:     filepath.Join(root, "repos"),
			WorktreesDir: filepath.Join(root, "worktrees"),
			Timeout:      time.Minute,
		}),
		Sessions: ring.NewSessions(time.Hour, nil),
		Logger:   embedlog.Logger{},
	}), shaA
}

// One SHA lives in one repository. A search asked across several must answer
// from the one that has the commit and name the ones that do not, rather than
// fail on them — with repos defaulting to "all", failing meant failing always.
func TestCodeSearch_ReposWithoutRef(t *testing.T) { //nolint:tparallel // subtests share the fixture
	t.Parallel()
	s, shaA := searchFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	t.Run("searches where the commit is, names where it is not", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeSearch, map[string]any{"query": "func Needle", "ref": shaA, "repos": []any{"a", "b"}})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out CodeSearchResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		require.Len(t, out.Matches, 1)
		assert.Contains(t, out.Matches[0], "a a.go:3:")
		assert.Equal(t, []string{"b"}, out.ReposWithoutRef)
	})

	t.Run("default repos is every repository, and still answers", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeSearch, map[string]any{"query": "func Needle", "ref": shaA})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out CodeSearchResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Len(t, out.Matches, 1)
		assert.Equal(t, []string{"b"}, out.ReposWithoutRef)
	})

	t.Run("a commit nobody has is RefUnknown naming every repository", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeSearch, map[string]any{"query": "func Needle", "ref": "0123456789abcdef0123456789abcdef01234567", "repos": []any{"a", "b"}})
		require.NoError(t, err)
		require.True(t, res.IsError)
		assert.Contains(t, res.Content[0].Text, ErrCodeRefUnknown)
		assert.Contains(t, res.Content[0].Text, "a,b")
	})
}

// A search points at several places; reading them is one question, not three.
func TestCodeRead_Windows(t *testing.T) { //nolint:tparallel // subtests share the fixture
	t.Parallel()
	s, sha := codeFixture(t)
	ctx := ctxWithGroups("ringsrv-developers")

	t.Run("several windows in one call, in the order asked", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha,
			"windows": []any{"internal/rpc/order.go:7-9", "internal/rpc/order.go:1-1"},
		})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out CodeReadWindowsResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Equal(t, sha, out.Ref)
		require.Len(t, out.Windows, 2)
		assert.Equal(t, 7, out.Windows[0].LineFrom)
		assert.True(t, strings.HasPrefix(out.Windows[0].Lines[0], "7: "), "got %q", out.Windows[0].Lines[0])
		assert.Equal(t, []string{"1: package rpc"}, out.Windows[1].Lines)
	})

	t.Run("one refused window refuses the call", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha,
			"windows": []any{"internal/rpc/order.go", "secrets/server.pem"},
		})
		require.NoError(t, err)
		assert.Equal(t, ErrCodePathDenied, decodeErr(t, res).Code)
	})

	t.Run("path and windows together, or neither, is BadArgs", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go",
			"windows": []any{"internal/rpc/order.go"},
		})
		assert.Equal(t, ErrCodeBadArgs, decodeErr(t, res).Code)

		res, _ = s.Call(ctx, ToolCodeRead, map[string]any{"repo": "apisrv", "ref": sha})
		assert.Equal(t, ErrCodeBadArgs, decodeErr(t, res).Code)

		res, _ = s.Call(ctx, ToolCodeRead, map[string]any{"repo": "apisrv", "ref": sha, "windows": []any{"internal/rpc/order.go:x-y"}})
		assert.Equal(t, ErrCodeBadArgs, decodeErr(t, res).Code, "a malformed line suffix is refused, not read as a path")
	})

	t.Run("path:line reads from that line on", func(t *testing.T) {
		res, err := s.Call(ctx, ToolCodeRead, map[string]any{
			"repo": "apisrv", "ref": sha, "windows": []any{"internal/rpc/order.go:7"}, "max_lines": 1,
		})
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out CodeReadWindowsResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		require.Len(t, out.Windows, 1)
		assert.True(t, strings.HasPrefix(out.Windows[0].Lines[0], "7: "), "got %q", out.Windows[0].Lines[0])
	})
}

// A window asked for twice is read once, and the answer still has one entry per
// window sent: windows[i] has to keep meaning "the i-th window you asked for".
func TestCodeRead_RepeatedWindow(t *testing.T) {
	t.Parallel()
	s, sha := codeFixture(t)

	res, err := s.Call(ctxWithGroups("ringsrv-developers"), ToolCodeRead, map[string]any{
		"repo": "apisrv", "ref": sha,
		"windows": []any{"internal/rpc/order.go:1-1", "internal/rpc/order.go:7-9", "internal/rpc/order.go:1-1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)

	out := decodeText[CodeReadWindowsResult](t, res)
	require.Len(t, out.Windows, 3, "one entry per window asked, repeats included")
	assert.Equal(t, []string{"1: package rpc"}, out.Windows[0].Lines)
	assert.NotEmpty(t, out.Windows[1].Lines, "a different range is a different window")
	assert.Nil(t, out.Windows[1].DedupOf)

	assert.Empty(t, out.Windows[2].Lines, "the repeat carries no second copy of the file")
	require.NotNil(t, out.Windows[2].DedupOf)
	assert.Equal(t, 0, *out.Windows[2].DedupOf, "and says where the lines are")
	assert.Equal(t, "internal/rpc/order.go", out.Windows[2].Path)
}
