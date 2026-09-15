package code

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseFrame(t *testing.T) {
	t.Parallel()
	cases := []struct {
		frame string
		path  string
		line  int
		ok    bool
	}{
		{"pkg/acme/product_list.go:95", "pkg/acme/product_list.go", 95, true},
		{"pkg/acme/product_list.go", "pkg/acme/product_list.go", 0, true},
		{"/builds/x/pkg/a.go:12", "/builds/x/pkg/a.go", 12, true},
		{"", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.frame, func(t *testing.T) {
			t.Parallel()
			path, line, ok := ParseFrame(c.frame)
			assert.Equal(t, c.ok, ok)
			if c.ok {
				assert.Equal(t, c.path, path)
				assert.Equal(t, c.line, line)
			}
		})
	}
}

func TestSameFile(t *testing.T) {
	t.Parallel()
	assert.True(t, SameFile("pkg/a/b.go", "pkg/a/b.go"))
	assert.True(t, SameFile("pkg/a/b.go", "/builds/svc/pkg/a/b.go"), "Sentry reports build paths")
	assert.True(t, SameFile("pkg/a/b.go", "a/b.go"), "a shorter frame path still matches")
	assert.False(t, SameFile("pkg/a/b.go", "pkg/a/c.go"))
	// A tail match must be on a path boundary, or "order.go" would match
	// "reorder.go".
	assert.False(t, SameFile("pkg/a/order.go", "pkg/a/reorder.go"))
}

// The narrowing is what makes blast_radius cheap: the engine is asked about
// the files a frame points into and nothing else. If this picked the wrong
// files, the answer would be wrong rather than slow.
func TestFramedFiles(t *testing.T) {
	t.Parallel()
	files := []string{"pkg/a/one.go", "pkg/b/two.go", "pkg/c/three.go"}

	got := framedFiles(files, []string{"pkg/b/two.go:42"})
	assert.Equal(t, []string{"pkg/b/two.go"}, got)

	// A build path from Sentry still names the same file.
	got = framedFiles(files, []string{"/builds/svc/pkg/c/three.go:7", "pkg/a/one.go"})
	assert.ElementsMatch(t, []string{"pkg/a/one.go", "pkg/c/three.go"}, got)

	// A frame from another service narrows to nothing rather than to
	// everything: an empty answer is checked against unmatched_frames.
	assert.Empty(t, framedFiles(files, []string{"vendor/other/x.go:1"}))

	// Two frames in one file ask about that file once.
	assert.Len(t, framedFiles(files, []string{"pkg/a/one.go:1", "pkg/a/one.go:99"}), 1)
}

func TestWithout(t *testing.T) {
	t.Parallel()
	files := []string{"a", "b", "c"}
	assert.Equal(t, []string{"a", "c"}, without(files, []string{"b"}))
	assert.Equal(t, files, without(files, nil))
	assert.Empty(t, without(files, files))
}
