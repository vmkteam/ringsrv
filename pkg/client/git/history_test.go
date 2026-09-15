package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The diff parser is what turns git's output into "which lines of which file
// changed"; every blast_radius answer starts here. The shapes below are the
// ones that used to be read wrongly: a rename came out as a
// deletion plus an addition, a deleted file vanished, and a quoted path was
// attributed to the file before it.
func TestParseDiff(t *testing.T) {
	t.Parallel()

	const out = `diff --git a/pkg/a.go b/pkg/a.go
index 1111111..2222222 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -3,2 +3,3 @@ func A() {
+	x := 1
@@ -10 +11,0 @@ func B() {
-	gone
diff --git a/pkg/old.go b/pkg/new.go
similarity index 100%
rename from pkg/old.go
rename to pkg/new.go
diff --git a/pkg/moved.go b/pkg/moved2.go
similarity index 90%
rename from pkg/moved.go
rename to pkg/moved2.go
index 3333333..4444444 100644
--- a/pkg/moved.go
+++ b/pkg/moved2.go
@@ -5 +5 @@ func M() {
-	return 1
+	return 2
diff --git a/pkg/deleted.go b/pkg/deleted.go
deleted file mode 100644
index 5555555..0000000
--- a/pkg/deleted.go
+++ /dev/null
@@ -1,7 +0,0 @@
-package pkg
diff --git a/pkg/added.go b/pkg/added.go
new file mode 100644
index 0000000..6666666
--- /dev/null
+++ b/pkg/added.go
@@ -0,0 +1,4 @@
+package pkg
diff --git "a/pkg/sp ace.go" "b/pkg/sp ace.go"
index 7777777..8888888 100644
--- "a/pkg/sp ace.go"
+++ "b/pkg/sp ace.go"
@@ -2 +2 @@
-a
+b
`
	d := parseDiff(out)

	require.Equal(t, []Hunk{
		{Start: 3, Count: 3, OldStart: 3, OldCount: 2},
		{Start: 11, Count: 0, OldStart: 10, OldCount: 1},
	}, d.Hunks["pkg/a.go"], "the new side and the old side both survive; a deletion keeps Count zero")

	_, hasOld := d.Hunks["pkg/old.go"]
	assert.False(t, hasOld, "a pure rename has no hunks under the old name")
	_, hasNew := d.Hunks["pkg/new.go"]
	assert.False(t, hasNew, "nor under the new one — nothing in it changed")
	assert.Equal(t, "pkg/old.go", d.Renamed["pkg/new.go"], "but the move is recorded")

	assert.Equal(t, "pkg/moved.go", d.Renamed["pkg/moved2.go"])
	assert.Equal(t, []Hunk{{Start: 5, Count: 1, OldStart: 5, OldCount: 1}}, d.Hunks["pkg/moved2.go"],
		"a rename with a change carries only the change, under the new name")

	assert.Equal(t, []Hunk{{Start: 0, Count: 0, OldStart: 1, OldCount: 7}}, d.Hunks["pkg/deleted.go"],
		"a deleted file is listed under its old path as gone")
	assert.Equal(t, []Hunk{{Start: 1, Count: 4, OldStart: 0, OldCount: 0}}, d.Hunks["pkg/added.go"])

	assert.Equal(t, []Hunk{{Start: 2, Count: 1, OldStart: 2, OldCount: 1}}, d.Hunks["pkg/sp ace.go"],
		"a quoted path is unquoted rather than attributed to the previous file")
	_, leaked := d.Hunks["/dev/null"]
	assert.False(t, leaked)
}

func TestHunkCovers(t *testing.T) {
	t.Parallel()
	h := Hunk{Start: 10, Count: 3}
	assert.True(t, h.Covers(10))
	assert.True(t, h.Covers(12))
	assert.False(t, h.Covers(13))
	assert.False(t, h.Covers(9))
	assert.False(t, Hunk{Start: 10, Count: 0}.Covers(10), "a deletion covers no line of the new file")
}

func TestDiffPath(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "pkg/a.go", diffPath("b/pkg/a.go", "b/"))
	assert.Equal(t, "pkg/a.go", diffPath("b/pkg/a.go\t", "b/"), "git appends a tab after paths with spaces")
	assert.Equal(t, "pkg/ü.go", diffPath(`"b/pkg/\303\274.go"`, "b/"), "C-style octal escapes decode to UTF-8")
	assert.Empty(t, diffPath("/dev/null", "b/"))
	assert.Equal(t, "x.go", diffPath("x.go", ""))
}
