package files

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vmkteam/mcpkit/mcp"
	"golang.org/x/text/encoding/charmap"
	textunicode "golang.org/x/text/encoding/unicode"
)

// Sizes of the text answer.
const (
	// maxLineBytes cuts a line longer than this: one minified JSON or a CSV
	// without newlines would otherwise be the whole answer as one line.
	maxLineBytes = 2048
	// binaryProbe is how much of the start is searched for a NUL byte.
	binaryProbe = 8 << 10
	// minTextBytes is the room lines get however small the budget.
	minTextBytes = 1024
	// textSlack is kept free of lines for the item around the answer in a
	// batch: its index, target, status and duration.
	textSlack = 128
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// textOf turns the downloaded bytes into the answer: decoded, split into lines,
// the window or the matches, cut to the budget. The cut is made here, line by
// line, rather than left to the size limit after it: that one cuts the JSON
// as a string, and the model would get half a document.
//
// The file stays bytes throughout: of a file of megabytes only the lines that
// go into the answer become strings.
func textOf(meta Meta, data []byte, p params, budget int, table bool) (*Text, error) {
	text, encoding, err := decodeText(data)
	if err != nil {
		name := meta.FileType
		if meta.Entry != "" {
			name = meta.Entry
		}
		return nil, refused(CodeUnsupported, "file %s (%s) is not text: %s", meta.ID, name, err)
	}
	out := &Text{Meta: meta, Encoding: encoding}
	out.fill(text, p, budget, table)
	return out, nil
}

// fill puts the lines of text, valid UTF-8, into the answer. Whatever the
// answer already carries counts against the budget. A table read from the
// middle, or through a grep, keeps its column names.
func (t *Text) fill(text []byte, p params, budget int, table bool) {
	t.TotalLines, t.Grep, t.Lines = countLines(text), p.Grep, []string{}
	if table && t.Header == "" && t.TotalLines > 0 && (p.Offset > 0 || p.Grep != "") {
		first, _, _ := bytes.Cut(text, []byte("\n"))
		t.Header = cutLine(bytes.TrimSuffix(first, []byte("\r")), nil, nil)
	}
	room := max(budget-jsonLen(t.widest(p.Grep != ""))-textSlack, minTextBytes)
	t.collect(text, p, room)
}

// widest is the answer with no lines and every field set after them at its
// widest: what the lines are left with is measured against it.
func (t *Text) widest(grep bool) Text {
	w, n := *t, t.TotalLines
	w.LineFrom, w.LineTo, w.NextOffset, w.Truncated = n, n, n, true
	if grep {
		w.Matched = &n
	}
	return w
}

// collect returns the lines from offset on — every one, or with grep the ones
// holding it — as many as limit and the room allow. A grep goes on past the
// last line returned, to count all the matches.
func (t *Text) collect(text []byte, p params, room int) {
	var needle, lower []byte
	if p.Grep != "" {
		needle = appendLower(nil, []byte(p.Grep))
	}
	matched, used, stopped := 0, 0, false
	for i, line := range lines(text) {
		if i < p.Offset {
			continue
		}
		if needle != nil {
			lower = appendLower(lower[:0], line)
			if !bytes.Contains(lower, needle) {
				continue
			}
		}
		matched++
		if stopped {
			if needle == nil {
				break // a window has nothing left to count
			}
			continue
		}
		if len(t.Lines) == p.Limit {
			stopped, t.NextOffset = true, i
			continue
		}
		entry := numbered(i, cutLine(line, lower, needle))
		cost := jsonLen(entry) + 1
		if used+cost > room && len(t.Lines) > 0 {
			stopped, t.Truncated, t.NextOffset = true, true, i
			continue
		}
		t.add(i, entry)
		used += cost
	}
	if needle != nil {
		t.Matched = &matched
	}
}

// add appends the line at index i, numbered from 1.
func (t *Text) add(i int, entry string) {
	if len(t.Lines) == 0 {
		t.LineFrom = i + 1
	}
	t.LineTo = i + 1
	t.Lines = append(t.Lines, entry)
}

func numbered(i int, line string) string {
	return fmt.Sprintf("%d: %s", i+1, line)
}

// decodeText reads the bytes as text: UTF-8 with or without a BOM, UTF-16 with
// one, and anything else that is not valid UTF-8 as windows-1251 — the
// encoding of a CSV out of 1C — unless it is UTF-8 with a few broken bytes,
// which become U+FFFD. A NUL near the start means it is not text. What comes
// back is always valid UTF-8; for a valid UTF-8 file it is the bytes it was
// given.
func decodeText(data []byte) (text []byte, encoding string, err error) {
	switch {
	case bytes.HasPrefix(data, utf8BOM):
		data = data[len(utf8BOM):]
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		decoded, decErr := textunicode.UTF16(textunicode.LittleEndian, textunicode.ExpectBOM).NewDecoder().Bytes(data)
		if decErr != nil {
			return nil, "", fmt.Errorf("it is not valid UTF-16: %w", decErr)
		}
		return decoded, "utf-16", nil
	}
	if bytes.IndexByte(data[:min(len(data), binaryProbe)], 0) >= 0 {
		return nil, "", errors.New("it holds NUL bytes")
	}
	if utf8.Valid(data) {
		return data, "", nil
	}
	if mostlyUTF8(data) {
		return bytes.ToValidUTF8(data, []byte("\uFFFD")), "", nil
	}
	out, err := charmap.Windows1251.NewDecoder().Bytes(data)
	if err != nil {
		return nil, "", fmt.Errorf("it is neither UTF-8 nor windows-1251: %w", err)
	}
	return out, "windows-1251", nil
}

// mostlyUTF8 tells UTF-8 with a stray byte from windows-1251. In Russian
// windows-1251 nearly every byte past ASCII breaks UTF-8; in UTF-8 with a
// broken byte nearly none does, and reading the whole file as windows-1251
// would turn each of its letters into two.
func mostlyUTF8(data []byte) bool {
	valid, broken := 0, 0
	for len(data) > 0 {
		r, n := utf8.DecodeRune(data)
		switch {
		case r == utf8.RuneError && n == 1:
			broken++
		case n > 1:
			valid++
		}
		data = data[n:]
	}
	return valid >= 10*broken
}

// lines yields the lines of text with their index: split on \n, the \r of a
// \r\n dropped, and a final newline does not make an empty last line.
func lines(text []byte) iter.Seq2[int, []byte] {
	return func(yield func(int, []byte) bool) {
		i := 0
		for line := range bytes.Lines(text) {
			line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			if !yield(i, line) {
				return
			}
			i++
		}
	}
}

// countLines is how many lines lines yields.
func countLines(text []byte) int {
	n := bytes.Count(text, []byte("\n"))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		n++
	}
	return n
}

// appendLower appends s lowercased to dst, rune by rune as strings.ToLower
// does it. The caller reuses dst: a grep lowers every line of the file, and a
// new string per line would be a file's worth of garbage.
func appendLower(dst, s []byte) []byte {
	for len(s) > 0 {
		if c := s[0]; c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			dst = append(dst, c)
			s = s[1:]
			continue
		}
		r, n := utf8.DecodeRune(s)
		dst = utf8.AppendRune(dst, unicode.ToLower(r))
		s = s[n:]
	}
	return dst
}

// cutLine shortens a line over maxLineBytes. With a needle the kept part is
// the one around its first occurrence in lower — the line lowercased — so a
// match deep in a long line is seen.
func cutLine(line, lower, needle []byte) string {
	if len(line) <= maxLineBytes {
		return string(line)
	}
	s := string(line)
	start := 0
	// Only where lowering kept the byte offsets: elsewhere the index would
	// point into a different string.
	if len(needle) > 0 && len(lower) == len(line) {
		if at := bytes.Index(lower, needle); at > maxLineBytes/2 {
			head, _ := mcp.CutBytes(s, at-maxLineBytes/2)
			start = len(head)
		}
	}
	kept, _ := mcp.CutBytes(s[start:], maxLineBytes)
	end := start + len(kept)
	var b strings.Builder
	if start > 0 {
		fmt.Fprintf(&b, "…[-%d bytes]", start)
	}
	b.WriteString(kept)
	if end < len(s) {
		fmt.Fprintf(&b, "…[+%d bytes]", len(s)-end)
	}
	return b.String()
}

// jsonLen is what v costs in the answer.
func jsonLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}
