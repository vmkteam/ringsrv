package files

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

func TestDecodeText(t *testing.T) {
	t.Parallel()
	cp1251, err := charmap.Windows1251.NewEncoder().String("код;название\n1;Аспирин\n")
	require.NoError(t, err)
	utf16, err := unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewEncoder().String("строка\n")
	require.NoError(t, err)

	cases := map[string]struct {
		in, want, encoding string
	}{
		"utf-8":        {"привет\n", "привет\n", ""},
		"utf-8 bom":    {"\xEF\xBB\xBFa;b\n", "a;b\n", ""},
		"windows-1251": {cp1251, "код;название\n1;Аспирин\n", "windows-1251"},
		"utf-16 bom":   {utf16, "строка\n", "utf-16"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, enc, decErr := decodeText([]byte(tc.in))
			require.NoError(t, decErr)
			assert.Equal(t, tc.want, string(got))
			assert.Equal(t, tc.encoding, enc)
		})
	}

	_, _, err = decodeText([]byte("PK\x03\x04\x00\x00binary"))
	require.Error(t, err, "a NUL near the start is not text")

	// One broken byte in a UTF-8 file does not make the rest windows-1251.
	got, enc, err := decodeText([]byte("код;название\n1;Аспирин \xFF\n2;Бинт\n"))
	require.NoError(t, err)
	assert.Empty(t, enc)
	assert.Equal(t, "код;название\n1;Аспирин \uFFFD\n2;Бинт\n", string(got))
}

func TestLines(t *testing.T) {
	t.Parallel()
	collect := func(text string) []string {
		var out []string
		for i, line := range lines([]byte(text)) {
			assert.Len(t, out, i, "indexes count from 0 without gaps")
			out = append(out, string(line))
		}
		assert.Len(t, out, countLines([]byte(text)), "countLines agrees with lines")
		return out
	}
	assert.Equal(t, []string{"a", "b", ""}, collect("a\r\nb\r\n\r\n"))
	assert.Equal(t, []string{"a", "b"}, collect("a\nb"))
	assert.Equal(t, []string{""}, collect("\n"))
	assert.Nil(t, collect(""))
}

func TestAppendLower(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"ABC def", "АСПИРИН Детский", "ΣΊΣΥΦΟΣ", "İstanbul", ""} {
		assert.Equal(t, strings.ToLower(s), string(appendLower(nil, []byte(s))), s)
	}
}

func TestCutLine(t *testing.T) {
	t.Parallel()
	short := "короткая строка"
	assert.Equal(t, short, cutLine([]byte(short), nil, nil))

	long := strings.Repeat("я", 3000) // 6000 bytes
	cut := cutLine([]byte(long), nil, nil)
	assert.True(t, strings.HasPrefix(cut, "яяя"))
	assert.Contains(t, cut, "…[+")
	assert.LessOrEqual(t, len(cut), maxLineBytes+32)
	assert.True(t, strings.HasSuffix(cut, "bytes]"))

	// A match deep in a long line is what the kept part shows.
	deep := []byte(strings.Repeat("a", 5000) + "NEEDLE" + strings.Repeat("b", 5000))
	cut = cutLine(deep, appendLower(nil, deep), []byte("needle"))
	assert.Contains(t, cut, "NEEDLE")
	assert.True(t, strings.HasPrefix(cut, "…[-"))
	assert.Contains(t, cut, "…[+")
}

func TestTextOf_BinaryRefused(t *testing.T) {
	t.Parallel()
	_, err := textOf(Meta{ID: "F0000000001", FileType: "text"}, []byte("a\x00b"), params{Limit: 10}, 32<<10, false)
	assert.Equal(t, CodeUnsupported, refusalCode(err))
}

// Reading a table from its middle keeps its column names; reading from the
// top does not repeat them.
func TestTextOf_Header(t *testing.T) {
	t.Parallel()
	meta := Meta{ID: "F0000000001", FileType: "csv"}
	data := []byte("a;b\n1;2\n3;4\n")
	txt, err := textOf(meta, data, params{Offset: 1, Limit: 10}, 32<<10, isTable(meta))
	require.NoError(t, err)
	assert.Equal(t, "a;b", txt.Header)
	assert.Equal(t, []string{"2: 1;2", "3: 3;4"}, txt.Lines)

	txt, err = textOf(meta, data, params{Limit: 10}, 32<<10, isTable(meta))
	require.NoError(t, err)
	assert.Empty(t, txt.Header)
}

// The count of matches covers the whole file even when the answer stops early.
func TestTextOf_GrepLimit(t *testing.T) {
	t.Parallel()
	data := []byte(strings.Repeat("hit\nmiss\n", 30))
	txt, err := textOf(Meta{ID: "F0000000001", FileType: "text"}, data, params{Limit: 5, Grep: "HIT"}, 32<<10, false)
	require.NoError(t, err)
	assert.Len(t, txt.Lines, 5)
	require.NotNil(t, txt.Matched)
	assert.Equal(t, 30, *txt.Matched)
	assert.Equal(t, 10, txt.NextOffset, "the next match is line 11, offset 10")
	assert.False(t, txt.Truncated)
}
