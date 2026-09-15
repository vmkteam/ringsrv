package codegraph

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/code/gittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixture writes a small Go package with a call chain worth asking about.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) { gittest.Write(t, dir, name, body) }

	write("go.mod", "module example\n\ngo 1.25\n")
	// Two methods of one name in different types: the normal case in Go, and
	// the one where guessing produces a wrong answer instead of no answer.
	write("cache.go", `package example

type Cache struct{}

func (c Cache) OrderByID(id int) int {
	return id
}
`)
	write("order.go", `package example

// Repo reads orders.
type Repo struct{}

func (r Repo) OrderByID(id int) int {
	return id
}

func (r Repo) FullOrder(id int) int {
	return r.OrderByID(id)
}

func Handler(id int) int {
	var r Repo
	return r.FullOrder(id)
}
`)
	return dir
}

func newClient(t *testing.T) *Client {
	t.Helper()
	return New(Options{Bin: gittest.EngineOrSkip(t), Timeout: 2 * time.Minute})
}

func TestVersion(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	v, err := c.Version(t.Context())
	require.NoError(t, err)
	assert.Contains(t, v, "code-graph-mcp")

	missing := New(Options{Bin: "code-graph-that-does-not-exist", Timeout: time.Second})
	_, err = missing.Version(t.Context())
	require.Error(t, err, "a missing engine must be a clear boot error")
}

func TestEnsureAndQuery(t *testing.T) { //nolint:tparallel // subtests share one index
	t.Parallel()
	c := newClient(t)
	dir := fixture(t)

	assert.False(t, c.Indexed(dir))
	require.NoError(t, c.Ensure(t.Context(), dir))
	assert.True(t, c.Indexed(dir))
	assert.FileExists(t, indexPath(dir), "the index lives inside the tree it indexed")

	t.Run("node carries boundaries and body", func(t *testing.T) {
		n, err := c.Node(t.Context(), dir, "OrderByID", "order.go")
		require.NoError(t, err)
		assert.Equal(t, "order.go", n.Path)
		// Boundaries are why this engine was chosen: without an end line a
		// stack frame maps onto a symbol only by approximation.
		assert.Positive(t, n.StartLine)
		assert.Greater(t, n.EndLine, n.StartLine)
		assert.Contains(t, n.Body, "func (r Repo) OrderByID")
	})

	t.Run("references find the caller", func(t *testing.T) {
		refs, err := c.References(t.Context(), dir, "OrderByID", "order.go", 0)
		require.NoError(t, err)
		require.NotEmpty(t, refs)

		names := make([]string, 0, len(refs))
		for _, r := range refs {
			names = append(names, r.Name)
			assert.NotEmpty(t, r.Path)
		}
		assert.Contains(t, names, "FullOrder")
	})

	t.Run("limit caps the answer", func(t *testing.T) {
		refs, err := c.References(t.Context(), dir, "OrderByID", "order.go", 1)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(refs), 1)
	})

	t.Run("file symbols come back sorted, with boundaries", func(t *testing.T) {
		nodes, err := c.FileSymbols(t.Context(), dir, "order.go")
		require.NoError(t, err)
		require.NotEmpty(t, nodes)

		for i := 1; i < len(nodes); i++ {
			assert.LessOrEqual(t, nodes[i-1].StartLine, nodes[i].StartLine)
		}

		// A line inside a function resolves to it; a line in the package
		// comment belongs to no symbol, and saying so is the point.
		n, err := c.Node(t.Context(), dir, "FullOrder", "")
		require.NoError(t, err)
		got := SymbolAt(nodes, n.StartLine+1)
		require.NotNil(t, got)
		assert.Equal(t, "FullOrder", got.Name)
		assert.Nil(t, SymbolAt(nodes, 1), "line 1 is the package clause, not a symbol")

		// Handler is three lines long and nothing calls it: with the engine's
		// default dead_min_lines of five it vanished into inactive_summary
		// without line numbers, and a frame in it resolved to no symbol.
		names := make([]string, 0, len(nodes))
		for _, n := range nodes {
			names = append(names, n.Name)
		}
		assert.Contains(t, names, "Handler", "a short uncalled function still has boundaries")
	})

	// The engine reports an ambiguous name inside a successful response, with no
	// error flag. Decoded naively it becomes an empty list of references —
	// "nobody calls this" — so it has to surface as an error with the candidates.
	t.Run("an ambiguous name is an error with candidates", func(t *testing.T) {
		_, err := c.References(t.Context(), dir, "OrderByID", "", 0)
		var amb *AmbiguousError
		require.ErrorAs(t, err, &amb)
		assert.Len(t, amb.Candidates, 2)

		// Naming the file resolves it, and the answer is about that one symbol.
		refs, err := c.References(t.Context(), dir, "OrderByID", "order.go", 0)
		require.NoError(t, err)
		require.NotEmpty(t, refs)
		names := make([]string, 0, len(refs))
		for _, r := range refs {
			names = append(names, r.Name)
		}
		assert.Contains(t, names, "FullOrder")
	})

	t.Run("an unknown symbol is not found, not an empty answer", func(t *testing.T) {
		_, err := c.Node(t.Context(), dir, "NoSuchSymbolHere", "")
		require.Error(t, err)
	})

	t.Run("a junk symbol never reaches the engine", func(t *testing.T) {
		for _, bad := range []string{"", "--flag", "a b", "x;rm -rf /", "../etc"} {
			_, err := c.Node(t.Context(), dir, bad, "")
			require.ErrorIs(t, err, ErrBadSymbol, "symbol %q", bad)
		}
	})
}

// Asking an unindexed tree has to say so, not answer as if the tree were empty.
func TestQueryWithoutIndex(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	_, err := c.Node(t.Context(), fixture(t), "OrderByID", "")
	require.ErrorIs(t, err, ErrNotIndex)
}

// Ten questions about one commit must build one index: the other nine would
// spend the same seconds building a copy of it.
func TestEnsure_SingleFlight(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	dir := fixture(t)

	var mu sync.Mutex
	var built int
	c.onIndex = func() { mu.Lock(); built++; mu.Unlock() }

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			assert.NoError(t, c.Ensure(t.Context(), dir))
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, built, "ten questions about one commit, one index")
}

// The engine refuses queries while it catches up on its index, in its own
// wording, inside an otherwise successful response. That is its business: a
// model told to retry asks a different tool instead, and the answer turns into
// a guess.
func TestPayloadError_IndexingIsWaitedOut(t *testing.T) {
	t.Parallel()
	busy := []byte(`{"error":"Indexing in progress — results will be available shortly. Please retry your request in a few seconds."}`)
	require.ErrorIs(t, payloadError(busy), ErrIndexing)

	// The same wording arriving as a tool error, which is the other shape the
	// engine uses for it.
	require.ErrorIs(t, toolError("find_references", "Error: Indexing in progress — retry shortly"), ErrIndexing)

	// A normal answer is not mistaken for it.
	require.NoError(t, payloadError([]byte(`{"references":[]}`)))
	assert.NotErrorIs(t, toolError("get_ast_node", "symbol not found"), ErrIndexing)
}

// SymbolAt is asked once per changed line, so it binary-searches. The edges are
// what matter: the first and last line of a symbol belong to it, the gap between
// symbols to nobody, and the file-wide "module" node is never the answer.
func TestSymbolAt(t *testing.T) {
	t.Parallel()
	nodes := []Node{
		{Name: "<module>", Type: "module", StartLine: 1, EndLine: 100},
		{Name: "First", Type: "function", StartLine: 10, EndLine: 20},
		{Name: "Second", Type: "method", StartLine: 30, EndLine: 40},
		{Name: "Third", Type: "function", StartLine: 60, EndLine: 61},
	}

	cases := map[int]string{
		10: "First", 15: "First", 20: "First",
		30: "Second", 40: "Second",
		60: "Third", 61: "Third",
	}
	for line, want := range cases {
		got := SymbolAt(nodes, line)
		require.NotNil(t, got, "line %d", line)
		assert.Equal(t, want, got.Name, "line %d", line)
	}

	// Between and outside symbols: the module node covers these lines, and
	// answering with it would put the whole file in a blast radius.
	for _, line := range []int{1, 5, 9, 21, 25, 41, 59, 62, 99, 1000} {
		assert.Nil(t, SymbolAt(nodes, line), "line %d belongs to no symbol", line)
	}
}

// An index started by a cancelled request still finishes, and the marker says
// so. Before, the work ran on the first caller's context and rebuild-index
// died with it for everybody who had joined.
func TestEnsure_FinishesAfterTheCallerLeft(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	dir := fixture(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, c.Ensure(cancelled, dir), context.Canceled)

	require.Eventually(t, func() bool { return c.Indexed(dir) },
		2*time.Minute, 100*time.Millisecond, "the index finishes without a waiter")
	assert.FileExists(t, readyPath(dir))

	// The marker, not the database, is what "indexed" means: a database left
	// by a killed rebuild must not pass for a finished index.
	require.NoError(t, os.Remove(readyPath(dir)))
	assert.FileExists(t, indexPath(dir))
	assert.False(t, c.Indexed(dir))
}

// go/parser is the source of Go symbols for blast_radius: the engine's listing
// had no receivers and folded two methods of one name into one. Both were
// real defects on real incidents.
func TestGoFileSymbols(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gittest.Write(t, dir, "names.go", `package example

type A struct{}

// String is documented.
func (a A) String() string {
	return "a"
}

type B struct{}

func (b *B) String() string {
	return "b"
}

type List[T any] struct{}

func (l *List[T]) Len() int { return 0 }

func Gone(x int) int {
	return x
}
`)
	nodes, err := GoFileSymbols(dir, "names.go")
	require.NoError(t, err)
	require.Len(t, nodes, 4)

	assert.Equal(t, "String", nodes[0].Name)
	assert.Equal(t, "A", nodes[0].Receiver)
	assert.Equal(t, NodeMethod, nodes[0].Type)
	assert.Equal(t, 6, nodes[0].StartLine, "the span starts at func, not at the doc comment")
	assert.Equal(t, 8, nodes[0].EndLine)

	assert.Equal(t, "String", nodes[1].Name)
	assert.Equal(t, "B", nodes[1].Receiver, "a pointer receiver names the type without the star")
	assert.Equal(t, 12, nodes[1].StartLine)

	assert.Equal(t, "Len", nodes[2].Name)
	assert.Equal(t, "List", nodes[2].Receiver, "a generic receiver names the type without its parameters")

	assert.Equal(t, "Gone", nodes[3].Name)
	assert.Empty(t, nodes[3].Receiver)
	assert.Equal(t, NodeFunction, nodes[3].Type)

	// The second String is the one that owns line 13.
	got := SymbolAt(nodes, 13)
	require.NotNil(t, got)
	assert.Equal(t, "B", got.Receiver)

	_, err = GoFileSymbols(dir, "missing.go")
	require.Error(t, err)
}
