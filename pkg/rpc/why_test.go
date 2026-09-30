package rpc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/code"
	"github.com/vmkteam/ringsrv/pkg/ring/code/gittest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/mcp"
)

// whyFixture builds a repository whose history is the chain why walks: a
// commit with an issue key, then a formatting pass over the same lines, plus a
// stand-in issue tracker.
func whyFixture(t *testing.T, tracker *httptest.Server, issueTarget, redact string) (svc ToolsService, sha string) {
	t.Helper()

	origin := t.TempDir()
	gittest.Git(t, origin, "init", "--initial-branch=master")
	write := func(name, body string) { gittest.Write(t, origin, name, body) }

	write("internal/rpc/order.go", "package rpc\n\nfunc Create() error {\n\treturn nil\n}\n")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "ABC-42 add order creation")

	// A formatting pass rewrites the same lines: blame will now name this
	// commit, which explains nothing about the behaviour.
	write("internal/rpc/order.go", "package rpc\n\nfunc Create() error {\n\treturn nil // nolint\n}\n")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "gofmt and lint fixes")

	write("internal/rpc/plain.go", "package rpc\n\nfunc Plain() {}\n")
	gittest.Git(t, origin, "add", ".")
	gittest.Git(t, origin, "commit", "-m", "no key in this subject")
	sha = gittest.Git(t, origin, "rev-parse", "HEAD")

	trackerURL := "https://tracker.example.com"
	if tracker != nil {
		trackerURL = tracker.URL
	}
	root := t.TempDir()
	cat, err := target.Parse([]byte(`
Env = "dev"

[Profiles.youtrack]
Description  = "dev · YouTrack: задачи"
BaseURL      = "` + trackerURL + `"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/issues(/[A-Z]+-\\d+)?$"]
Redact       = "` + redact + `"

[Roles.developer]
Description = "reads code and issues"
Groups      = ["ringsrv-developers"]
Tools       = ["why"]
Targets     = ["youtrack"]
Repos       = ["*"]

[Roles.coder]
Description = "reads code only"
Groups      = ["ringsrv-coders"]
Tools       = ["why"]
Targets     = []
Repos       = ["*"]

[Repos.apisrv]
Description   = "test repo"
CloneURL      = "file://` + origin + `"
DefaultBranch = "master"
TaskIDRegexp  = "^([A-Z]+-\\d+)"
IssueTarget   = "` + issueTarget + `"
`))
	require.NoError(t, err)

	return NewToolsService(ToolsDeps{
		Targets:  cat,
		Upstream: upstream.New(upstream.Options{Timeout: 5 * time.Second}),
		Repos: git.New(git.Options{
			ReposDir:     filepath.Join(root, "repos"),
			WorktreesDir: filepath.Join(root, "worktrees"),
			Timeout:      time.Minute,
		}),
		MaxBytes: 32768,
		Sessions: ring.NewSessions(time.Hour, nil),
		Logger:   embedlog.Logger{},
	}), sha
}

// youtrack answers like the real one: nothing useful unless fields are asked
// for, which is why the tool always lists them.
func youtrack(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fields") == "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"$type":"Issue"}`))
			return
		}
		if r.URL.Path != "/api/issues/ABC-42" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"summary": "Создание заказа",
			"description": "Заказ создаётся идемпотентно",
			"comments": [
				{"text": "research: docs/llm/research/03-orders.md"},
				{"text": "спека тут docs/llm/spec/02-orders.md, обсудили на созвоне"}
			]
		}`))
	}))
}

func TestWhy(t *testing.T) { //nolint:tparallel // subtests share the fixture
	t.Parallel()
	tracker := youtrack(t)
	t.Cleanup(tracker.Close)
	s, sha := whyFixture(t, tracker, "youtrack", "")
	ctx := ctxWithGroups("ringsrv-developers")

	ask := func(t *testing.T, args map[string]any) WhyResult {
		t.Helper()
		res, err := s.Call(ctx, ToolWhy, args)
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		var out WhyResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		return out
	}

	t.Run("the whole chain: line, commit, task, artefacts", func(t *testing.T) {
		out := ask(t, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "line": 4,
		})

		// Without the reference the answer cannot be checked against anything.
		assert.Equal(t, "apisrv internal/rpc/order.go:4@"+sha, out.Reference)

		// blame names the formatting commit; the answer keeps it but goes on to
		// the one that actually wrote the line.
		assert.Contains(t, out.Commit.Subject, "gofmt")
		require.NotNil(t, out.Previous)
		assert.Contains(t, out.Previous.Subject, "ABC-42")

		require.NotNil(t, out.Task)
		assert.Equal(t, "ABC-42", out.Task.ID)
		assert.Equal(t, enrichOK, out.Enrichment)
		assert.Equal(t, "Создание заказа", out.Task.Summary)
		assert.ElementsMatch(t,
			[]string{"docs/llm/research/03-orders.md", "docs/llm/spec/02-orders.md"},
			out.Task.Artifacts, "the links a /solve leaves in comments")
	})

	// A commit that follows no convention is a valid answer, not an error.
	t.Run("a commit without a key answers task: null", func(t *testing.T) {
		res, err := s.Call(ctx, ToolWhy, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/plain.go", "line": 3,
		})
		require.NoError(t, err)
		assert.Contains(t, res.Content[0].Text, `"task":null`)

		var out WhyResult
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
		assert.Nil(t, out.Task)
		assert.Equal(t, enrichNoTask, out.Enrichment)
	})

	t.Run("line or symbol is required", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolWhy, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go",
		})
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		assert.Contains(t, e.Message, "symbol")
	})

	t.Run("an unknown file is a clear error", func(t *testing.T) {
		res, _ := s.Call(ctx, ToolWhy, map[string]any{
			"repo": "apisrv", "ref": sha, "path": "internal/rpc/nope.go", "line": 1,
		})
		assert.Equal(t, ErrCodeRefUnknown, decodeErr(t, res).Code)
	})
}

// The tracker is asked under the caller's role, through the catalogue: a user
// without that target gets the commit and no enrichment, not a privileged read
// done on their behalf (acceptance 5).
func TestWhy_RespectsRole(t *testing.T) {
	t.Parallel()
	tracker := youtrack(t)
	t.Cleanup(tracker.Close)
	s, sha := whyFixture(t, tracker, "youtrack", "")

	res, err := s.Call(ctxWithGroups("ringsrv-coders"), ToolWhy, map[string]any{
		"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "line": 4,
	})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)

	var out WhyResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	require.NotNil(t, out.Task)
	assert.Equal(t, "ABC-42", out.Task.ID, "the key comes from the commit, not from the tracker")
	assert.Empty(t, out.Task.Summary)
	assert.Equal(t, enrichNoAccess, out.Enrichment, "the answer says why the summary is missing")
}

// An unreachable tracker costs the summary, not the answer.
func TestWhy_TrackerDown(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	s, sha := whyFixture(t, down, "youtrack", "")

	res, err := s.Call(ctxWithGroups("ringsrv-developers"), ToolWhy, map[string]any{
		"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "line": 4,
	})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)

	var out WhyResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	require.NotNil(t, out.Task)
	assert.Equal(t, "ABC-42", out.Task.ID)
	assert.Equal(t, enrichUnavailable, out.Enrichment)
}

// A repository with no tracker configured still answers; it just cannot
// enrich, and says so.
func TestWhy_NoIssueTarget(t *testing.T) {
	t.Parallel()
	s, sha := whyFixture(t, nil, "", "")

	res, err := s.Call(ctxWithGroups("ringsrv-developers"), ToolWhy, map[string]any{
		"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "line": 4,
	})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)

	var out WhyResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	assert.Equal(t, enrichNoAccess, out.Enrichment)
}

// why(symbol) used to build a worktree and an AST index to learn one line
// number — seconds of work for a starting point of a blame. It now greps the
// bare mirror, which also means the answer works on an instance with no engine
// at all. The fixture has none.
func TestWhy_BySymbolWithoutEngine(t *testing.T) {
	t.Parallel()
	tracker := youtrack(t)
	t.Cleanup(tracker.Close)
	s, sha := whyFixture(t, tracker, "youtrack", "")

	res, err := s.Call(ctxWithGroups("ringsrv-developers"), ToolWhy, map[string]any{
		"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "symbol": "Create",
	})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)

	var out WhyResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	// Create is declared on line 3 of the fixture file.
	assert.Equal(t, "apisrv internal/rpc/order.go:3@"+sha, out.Reference)
	require.NotNil(t, out.Task)
	assert.Equal(t, "ABC-42", out.Task.ID)

	// A symbol that is not there says so, and says what to do instead.
	res, _ = s.Call(ctxWithGroups("ringsrv-developers"), ToolWhy, map[string]any{
		"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "symbol": "NoSuchThing",
	})
	e := decodeErr(t, res)
	assert.Contains(t, e.Message, "not found")
	assert.Contains(t, e.Message, "pass line")
}

// The tracker's text reaches the model like any upstream body, so it takes the
// profile's redaction and a size cap. It used to go around both: why called
// the client directly, and a description that is a pasted log of a megabyte
// arrived whole.
func TestWhy_TrackerTextIsRedactedAndCapped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("л", 10000)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"summary":"Письмо от user@example.com","description":"` + long + `"}`))
	}))
	t.Cleanup(tracker.Close)
	s, sha := whyFixture(t, tracker, "youtrack", "redact")

	res, err := s.Call(ctxWithGroups("ringsrv-developers"), ToolWhy, map[string]any{
		"repo": "apisrv", "ref": sha, "path": "internal/rpc/order.go", "line": 4,
	})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)

	var out WhyResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	require.NotNil(t, out.Task)
	assert.Equal(t, enrichOK, out.Enrichment)
	assert.Equal(t, "Письмо от u***@example.com", out.Task.Summary)
	assert.True(t, strings.HasSuffix(out.Task.Description, mcp.TruncateMarker), "the description is cut and says so")
	assert.LessOrEqual(t, len(out.Task.Description), code.IssueTextBytes+len(mcp.TruncateMarker))
	assert.NotContains(t, res.Content[0].Text, "user@example.com")
}
