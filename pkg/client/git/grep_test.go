package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The output is what git prints with --line-number --null: the tree, a colon,
// the path, \0, the line number, \0, the text. Match and context lines look
// the same, so the pattern tells them apart.
func TestParseGrep(t *testing.T) {
	t.Parallel()
	const sha = "564ad90f955fe68dd273fc097556e9651ba4c094"
	out := sha + ":cmd/sast-scansrv/main.go\x0040\x00\n" +
		sha + ":cmd/sast-scansrv/main.go\x0041\x00func main() {\n" +
		sha + ":cmd/sast-scansrv/main.go\x0042\x00\tx := y // a: b\n" +
		"--\n" +
		sha + ":docs/2024-01-15-notes.md\x007\x00see main\n" +
		sha + ":docs/2024-01-15-notes.md\x008\x00func main() again\n"

	t.Run("context lines are marked, paths with dashes survive", func(t *testing.T) {
		t.Parallel()
		got, truncated := parseGrep(out, sha, GrepOptions{Pattern: "func main", Context: 1, Max: 50})
		require.Len(t, got, 5)
		assert.False(t, truncated)
		assert.Equal(t, Match{Path: "cmd/sast-scansrv/main.go", Line: 40, Text: "", Context: true}, got[0])
		assert.Equal(t, Match{Path: "cmd/sast-scansrv/main.go", Line: 41, Text: "func main() {"}, got[1])
		assert.Equal(t, Match{Path: "cmd/sast-scansrv/main.go", Line: 42, Text: "\tx := y // a: b", Context: true}, got[2])
		assert.Equal(t, Match{Path: "docs/2024-01-15-notes.md", Line: 7, Text: "see main", Context: true}, got[3])
		assert.False(t, got[4].Context)
	})

	t.Run("the limit counts matches, not lines", func(t *testing.T) {
		t.Parallel()
		got, truncated := parseGrep(out, sha, GrepOptions{Pattern: "func main", Context: 1, Max: 1})
		assert.True(t, truncated)
		matches := 0
		for _, m := range got {
			if !m.Context {
				matches++
				assert.Equal(t, 41, m.Line)
			}
		}
		assert.Equal(t, 1, matches)
	})

	t.Run("without context every line is a match", func(t *testing.T) {
		t.Parallel()
		got, _ := parseGrep(out, sha, GrepOptions{Pattern: "nothing", Max: 50})
		for _, m := range got {
			assert.False(t, m.Context)
		}
	})

	t.Run("regexp decides like git does", func(t *testing.T) {
		t.Parallel()
		got, _ := parseGrep(out, sha, GrepOptions{Pattern: "^func (main|other)", Regexp: true, Context: 1, Max: 50})
		assert.False(t, got[1].Context)
		assert.True(t, got[2].Context)
		assert.False(t, got[4].Context)
	})
}
