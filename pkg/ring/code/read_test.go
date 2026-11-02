package code

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// sliceLines clamps a window to the file and returns one it cannot satisfy empty.
func TestSliceLines(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c", "d", "e"}

	cases := []struct {
		name     string
		w        Window
		wantFrom int
		wantTo   int
		want     []string
	}{
		{"zero window is the whole file", Window{}, 1, 5, lines},
		{"window inside the file", Window{From: 2, To: 3}, 2, 3, []string{"b", "c"}},
		{"end past the file is clamped", Window{From: 4, To: 99}, 4, 5, []string{"d", "e"}},
		{"start past the file is empty", Window{From: 9, To: 12}, 9, 9, nil},
		{"end before start is empty", Window{From: 4, To: 2}, 4, 4, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sliceLines(lines, tc.w)
			assert.Equal(t, tc.wantFrom, got.From)
			assert.Equal(t, tc.wantTo, got.To)
			assert.Equal(t, len(lines), got.Total)
			assert.Equal(t, tc.want, got.Lines)
		})
	}
}
