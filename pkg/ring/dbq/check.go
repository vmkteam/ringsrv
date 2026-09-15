package dbq

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/target"
)

// Leading words a query may start with, per driver. The list is the first line
// of defence and only that: it makes the refusal readable before the server is
// asked, and the server's read-only guarantee is the actual protection.
var leadingWords = map[string][]string{
	target.DriverPostgres:   {wordSelect, wordWith, wordExplain, "SHOW", wordTable, wordValues},
	target.DriverClickHouse: {wordSelect, wordWith, wordExplain, "SHOW", "DESCRIBE", "DESC", "EXISTS"},
}

const (
	wordSelect  = "SELECT"
	wordWith    = "WITH"
	wordExplain = "EXPLAIN"
	wordTable   = "TABLE"
	wordValues  = "VALUES"
	wordAnalyze = "ANALYZE"
)

// wrapWords are the statements WrapLimit puts under a LIMIT. The others —
// EXPLAIN, SHOW, DESCRIBE — answer small and go as they are.
var wrapWords = []string{wordSelect, wordWith, wordTable, wordValues}

var (
	leadingWordRe = regexp.MustCompile(`^[A-Za-z]+`)
	// pgExplainOptsRe finds ANALYZE — or its British spelling, which
	// PostgreSQL takes too — inside EXPLAIN (…): it runs the query.
	pgExplainOptsRe = regexp.MustCompile(`(?i)\bANALY[SZ]E\b`)
	// chFormatRe is a trailing FORMAT clause: the server would answer in that
	// format while the client waits for native.
	chFormatRe  = regexp.MustCompile(`(?i)\bFORMAT\s+[A-Za-z]+\s*$`)
	chOutfileRe = regexp.MustCompile(`(?i)\bOUTFILE\b`)
)

// CheckSQL refuses what is obviously not a read before the server sees it: an
// empty text, a second statement, a leading word outside the driver's list, and
// the three constructs that look like a read and are not — EXPLAIN ANALYZE, INTO
// OUTFILE and a trailing FORMAT.
func CheckSQL(driver, sql string) error {
	words, ok := leadingWords[driver]
	if !ok {
		return fmt.Errorf("unknown driver %q", driver)
	}
	masked := mask(sql, driver)
	body := strings.TrimSpace(masked)
	if body == "" {
		return errors.New("empty query")
	}
	if i := strings.Index(body, ";"); i >= 0 && strings.TrimSpace(body[i+1:]) != "" {
		return errors.New("one statement per call: a second one follows the ';'")
	}

	word := strings.ToUpper(leadingWordRe.FindString(body))
	if word == "" || !slices.Contains(words, word) {
		return fmt.Errorf("a query starts with one of %s; got %q", strings.Join(words, ", "), firstWord(body))
	}

	switch driver {
	case target.DriverPostgres:
		if word == wordExplain && pgExplainAnalyze(body[len(word):]) {
			return errors.New("EXPLAIN ANALYZE runs the query; use EXPLAIN without ANALYZE")
		}
	case target.DriverClickHouse:
		if chOutfileRe.MatchString(body) {
			return errors.New("INTO OUTFILE writes a file on the server; the rows come back in the answer")
		}
		if chFormatRe.MatchString(strings.TrimSuffix(body, ";")) {
			return errors.New("drop the FORMAT clause: the answer is already structured (columns and rows)")
		}
	}
	return nil
}

// pgExplainAnalyze reads the options after EXPLAIN: the parenthesised list
// (ANALYZE, BUFFERS) or the bare keywords ANALYZE and VERBOSE that may
// precede the statement.
func pgExplainAnalyze(rest string) bool {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") {
		end := strings.Index(rest, ")")
		if end < 0 {
			end = len(rest)
		}
		return pgExplainOptsRe.MatchString(rest[:end])
	}
	for {
		w := strings.ToUpper(leadingWordRe.FindString(rest))
		switch w {
		case wordAnalyze, "ANALYSE":
			return true
		case "VERBOSE":
			rest = strings.TrimSpace(rest[len(w):])
		default:
			return false
		}
	}
}

// WrapLimit puts a SELECT-like statement under the given LIMIT. The newline
// before the closing bracket keeps a trailing "--" comment from eating it, and a
// trailing ";" is dropped because it may not sit inside a subquery. EXPLAIN,
// SHOW and the like are returned as they are.
func WrapLimit(driver, sql string, limit int) string {
	masked := mask(sql, driver)
	body := strings.TrimSpace(masked)
	word := strings.ToUpper(leadingWordRe.FindString(body))
	if !slices.Contains(wrapWords, word) {
		return sql
	}
	// Cut at the trailing ';' of the masked text, which is the same
	// position in the original: masking keeps lengths.
	cut := len(sql)
	if i := strings.LastIndex(masked, ";"); i >= 0 && strings.TrimSpace(masked[i+1:]) == "" {
		cut = i
	}
	return fmt.Sprintf("SELECT * FROM (\n%s\n) AS ringsrv_q LIMIT %d", strings.TrimSpace(sql[:cut]), limit)
}

// ValidTable reports whether a table argument is an identifier with an optional
// schema prefix. The name is bound as a parameter anyway; the refusal is just
// clearer here than from the server.
func ValidTable(name string) bool {
	schema, table, ok := strings.Cut(name, ".")
	if !ok {
		return target.IsIdent(name)
	}
	return target.IsIdent(schema) && target.IsIdent(table)
}

// mask blanks out string literals and comments, keeping every other byte and
// every length: what is left can be searched for a ';' or a keyword without a
// literal 'a;b' or a comment "-- select" fooling the search, and every index
// still points into the original text.
//
// Single-quoted literals double their quotes; a backslash escapes inside them
// for ClickHouse and for PostgreSQL's E'…' strings. A line comment ends at a
// carriage return as well as a newline — PostgreSQL's lexer stops at either, and
// a "\r" only the server reads as the end is a way to hide a ";" from this
// check. ClickHouse also opens a line comment with "#".
func mask(sql, driver string) string {
	out := []byte(sql)
	n := len(out)
	backslash := driver == target.DriverClickHouse
	for i := 0; i < n; {
		switch c := out[i]; {
		case c == '-' && i+1 < n && out[i+1] == '-', c == '#' && backslash:
			i = blankLine(out, i)
		case c == '/' && i+1 < n && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i = blankUntil(out, i+2, "*/")
		case c == '\'':
			i = blankQuoted(out, i, '\'', backslash || isEString(out, i))
		case c == '"':
			i = blankQuoted(out, i, '"', false)
		case c == '`' && backslash:
			i = blankQuoted(out, i, '`', true)
		case c == '$' && driver == target.DriverPostgres:
			if tag := dollarTag(out[i:]); tag != "" {
				opener := tag + "$"
				i = blankUntil(out, i+len(opener), opener)
				continue
			}
			i++
		default:
			i++
		}
	}
	return string(out)
}

// blankQuoted blanks a quoted run starting at the quote at out[i], keeping
// the quotes themselves, and returns the index after the closing quote.
func blankQuoted(out []byte, i int, q byte, backslashEscapes bool) int {
	n := len(out)
	j := i + 1
	for j < n {
		switch {
		case backslashEscapes && out[j] == '\\' && j+1 < n:
			out[j], out[j+1] = ' ', ' '
			j += 2
		case out[j] == q && j+1 < n && out[j+1] == q:
			out[j], out[j+1] = ' ', ' '
			j += 2
		case out[j] == q:
			return j + 1
		default:
			if out[j] != '\n' {
				out[j] = ' '
			}
			j++
		}
	}
	return n
}

// isEString reports whether the quote at out[i] opens PostgreSQL's E'…'
// literal, where a backslash escapes the next byte.
func isEString(out []byte, i int) bool {
	if i == 0 || (out[i-1] != 'E' && out[i-1] != 'e') {
		return false
	}
	return i == 1 || !isWordByte(out[i-2])
}

// blankLine blanks a line comment from i to the end of the line, exclusive
// of the line break itself, which may be "\n" or "\r".
func blankLine(out []byte, i int) int {
	j := i
	for j < len(out) && out[j] != '\n' && out[j] != '\r' {
		out[j] = ' '
		j++
	}
	return j
}

// blankUntil blanks from i up to and including the marker, or to the end of
// the input when the marker never arrives — an unterminated comment or
// heredoc swallows the rest of the statement, which is what the server would
// do with it too.
//
// bytes.Index, not strings.Index over a conversion: converting out[i:] copies
// the whole remainder on every comment and every heredoc, which made masking
// quadratic in the length of the query.
func blankUntil(out []byte, i int, marker string) int {
	end := bytes.Index(out[i:], []byte(marker))
	if end < 0 {
		end = len(out) - i
	} else {
		end += len(marker)
	}
	for j := i; j < i+end; j++ {
		if out[j] != '\n' {
			out[j] = ' '
		}
	}
	return i + end
}

// dollarTag reads a PostgreSQL dollar-quote opener at the start of b:
// "$$" gives "$", "$fn$" gives "$fn"; anything else — a positional
// parameter, a lone dollar — gives "". A tag is letters, digits and
// underscores, where "letters" includes every byte of a non-ASCII
// character, the way PostgreSQL's lexer reads them.
func dollarTag(b []byte) string {
	j := 1
	for j < len(b) && (isWordByte(b[j]) || b[j] >= 0x80) {
		j++
	}
	if j < len(b) && b[j] == '$' && (j == 1 || b[1] < '0' || b[1] > '9') {
		return string(b[:j])
	}
	return ""
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}
