package git

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Blob returns the contents of one file at one commit, read from the clone
// rather than a worktree: `git cat-file` is four milliseconds and needs no
// checkout. Worktrees are for tools that need a real directory.
func (s *Store) Blob(ctx context.Context, repo, sha, path string) ([]byte, error) {
	if err := ValidateRef(sha); err != nil {
		return nil, err
	}
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	out, err := s.run(ctx, s.ClonePath(repo), "cat-file", "blob", sha+":"+path)
	if err != nil {
		return nil, classify(err, path+" at "+sha)
	}
	return out, nil
}

// GrepOptions narrow a search. Everything here is checked before it becomes
// argv: the pattern is passed after -e so it can never be read as an option,
// and the globs are checked like paths.
type GrepOptions struct {
	Pattern string
	Regexp  bool
	Glob    string
	Context int
	Max     int
}

// Match is one line of a search result.
type Match struct {
	Path string
	Line int
	Text string
	// Context marks a line printed around a match, not a match itself. Without
	// the flag the caller cannot tell which line the search was about.
	Context bool
}

// Grep searches one commit. On the measured repository a literal search took
// 0.89s and a regexp one 1.01s, which is why the caller gets a limit and a
// timeout rather than a promise.
func (s *Store) Grep(ctx context.Context, repo, sha string, opts GrepOptions) ([]Match, bool, error) {
	if err := ValidateRef(sha); err != nil {
		return nil, false, err
	}
	if opts.Pattern == "" {
		return nil, false, fmt.Errorf("%w: empty pattern", ErrBadPath)
	}
	if opts.Glob != "" {
		if err := ValidatePath(opts.Glob); err != nil {
			return nil, false, err
		}
	}

	// --null puts \0 after the path and the line number, so a path with "-" or
	// ":" parses. The price is that git no longer marks context lines, which is
	// what parseGrep re-derives from the pattern.
	args := []string{"grep", "--line-number", "--null", "--no-color", "-I"} // -I: never search binaries
	if opts.Regexp {
		args = append(args, "--extended-regexp")
	} else {
		args = append(args, "--fixed-strings")
	}
	if opts.Context > 0 {
		args = append(args, "--context", strconv.Itoa(opts.Context))
	}
	// -e keeps a pattern starting with a dash from being read as an option,
	// and --end-of-options does the same for the ref.
	args = append(args, "-e", opts.Pattern, "--end-of-options", sha)
	if opts.Glob != "" {
		args = append(args, "--", opts.Glob)
	}

	out, err := s.run(ctx, s.ClonePath(repo), args...)
	if err != nil {
		// git grep exits 1 when nothing matched: an answer, not a failure.
		if strings.Contains(err.Error(), "exit status 1") {
			return nil, false, nil
		}
		return nil, false, err
	}
	matches, truncated := parseGrep(string(out), sha, opts)
	return matches, truncated, nil
}

// matcher tells a match from a context line by the pattern, the way git did.
// Only needed when context was asked for; a pattern Go cannot compile leaves
// every line a match.
func matcher(opts GrepOptions) func(string) bool {
	if opts.Context <= 0 {
		return func(string) bool { return true }
	}
	if !opts.Regexp {
		return func(s string) bool { return strings.Contains(s, opts.Pattern) }
	}
	re, err := regexp.CompilePOSIX(opts.Pattern)
	if err != nil {
		return func(string) bool { return true }
	}
	return re.MatchString
}

// parseGrep turns git's output into lines. Searching a commit makes git prefix
// every line with the revision — "<sha>:path:line:text" — so the prefix is
// stripped here. Context lines are kept: they carry their own line number.
func parseGrep(out, sha string, opts GrepOptions) ([]Match, bool) {
	isMatch := matcher(opts)
	var res []Match
	matches := 0
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if line == "" || line == "--" {
			continue
		}
		m, ok := parseGrepLine(strings.TrimPrefix(line, sha+":"))
		if !ok {
			continue
		}
		// The limit counts matches, not lines: context is what the caller
		// asked to see around each of them, not what they asked to cap.
		m.Context = !isMatch(m.Text)
		if !m.Context {
			if opts.Max > 0 && matches >= opts.Max {
				return res, true
			}
			matches++
		}
		res = append(res, m)
	}
	return res, false
}

// parseGrepLine reads "path:line:text", and "path-line-text" for context.
func parseGrepLine(line string) (Match, bool) {
	path, rest, ok := strings.Cut(line, "\x00")
	if !ok || path == "" {
		return Match{}, false
	}
	num, text, ok := strings.Cut(rest, "\x00")
	if !ok {
		return Match{}, false
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return Match{}, false
	}
	return Match{Path: path, Line: n, Text: text}, true
}

// Patterns for Definition. git grep speaks POSIX extended regular expressions,
// which have no \b, so word boundaries are spelled out as "not a word
// character". A language whose declarations look different falls through to the
// second pattern.
const declKeywords = `(func|type|class|def|fn|interface|struct|const|var|let)`

func declRe(name string) string {
	// keyword, then either the name directly ("func Create(") or something in
	// between ("func (r Repo) byID(").
	return declKeywords + `[^[:alnum:]_]+(.*[^[:alnum:]_])?` + name + `([^[:alnum:]_]|$)`
}

func mentionRe(name string) string {
	return `(^|[^[:alnum:]_])` + name + `([^[:alnum:]_]|$)`
}

// symbolNameRe is what may be looked up: the name goes into a regular
// expression, so it may not carry anything that would change its meaning.
var symbolNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,200}$`)

// Definition finds the line where a symbol is declared in one file, without
// checking the commit out. The AST engine answers this exactly, but a worktree
// and an index cost seconds to learn one line number the caller only needs as a
// place to start a blame. Two greps: the declaration, then any mention.
func (s *Store) Definition(ctx context.Context, repo, sha, path, symbol string) (int, error) {
	if err := ValidateRef(sha); err != nil {
		return 0, err
	}
	if err := ValidatePath(path); err != nil {
		return 0, err
	}
	if !symbolNameRe.MatchString(symbol) {
		return 0, fmt.Errorf("%w: symbol %q", ErrBadPath, symbol)
	}

	for _, pattern := range []string{declRe(symbol), mentionRe(symbol)} {
		out, err := s.run(ctx, s.ClonePath(repo),
			"grep", "--line-number", "--null", "--extended-regexp", "--no-color", "-e", pattern,
			"--end-of-options", sha, "--", path)
		if err != nil {
			// Exit 1 is "no match": the second pattern may still find it.
			if strings.Contains(err.Error(), "exit status 1") {
				continue
			}
			return 0, classify(err, symbol)
		}
		if matches, _ := parseGrep(string(out), sha, GrepOptions{Max: 1}); len(matches) > 0 {
			return matches[0].Line, nil
		}
	}
	return 0, fmt.Errorf("%w: %s in %s", ErrNotFound, symbol, path)
}
