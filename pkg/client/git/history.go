package git

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Commit is one entry of history.
type Commit struct {
	SHA     string   `json:"sha"`
	Short   string   `json:"short"`
	Subject string   `json:"subject"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	Files   []string `json:"files,omitempty"`
}

// logFormat is parsed rather than pretty-printed for a human: a fixed
// separator that cannot appear in a subject keeps the parser trivial.
const (
	logSep    = "\x1f"
	logFormat = "%H" + logSep + "%h" + logSep + "%s" + logSep + "%an" + logSep + "%aI"
)

// LogLines answers "who touched these lines" — git log -L, 0.31s on the measured
// repository. It is the one call that follows a range of lines through renames
// and rewrites, which is what a stack frame needs.
func (s *Store) LogLines(ctx context.Context, repo, sha, path string, from, to, limit int) ([]Commit, error) {
	if err := ValidateRef(sha); err != nil {
		return nil, err
	}
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	if from < 1 || to < from {
		return nil, fmt.Errorf("%w: line range %d,%d", ErrBadPath, from, to)
	}

	// -L takes "a,b:path" as one argument — the only place where a path and a
	// number are glued together, which is why both are checked above.
	arg := fmt.Sprintf("%d,%d:%s", from, to, path)
	out, err := s.run(ctx, s.ClonePath(repo),
		"log", "--no-patch", "--format="+logFormat, "--max-count="+strconv.Itoa(LimitOr(limit, 20)),
		"-L", arg, "--end-of-options", sha)
	if err != nil {
		return nil, classify(err, path)
	}
	return parseCommits(string(out)), nil
}

// Blame answers "who wrote this line" — 0.16s.
func (s *Store) Blame(ctx context.Context, repo, sha, path string, line int) (*Commit, error) {
	return s.blame(ctx, repo, sha, path, line, true)
}

// BlameExact is Blame without move detection: the commit that last wrote the
// line as it stands here, even if it only moved it. blast_radius attributes a
// hunk to the commit of the release that produced it, so a move inside the
// release is the right answer there.
func (s *Store) BlameExact(ctx context.Context, repo, sha, path string, line int) (*Commit, error) {
	return s.blame(ctx, repo, sha, path, line, false)
}

// blame runs one-line blame. With follow, -w -M -C skip whitespace-only rewrites
// and follow lines moved within or across files: "who wrote this" is about the
// logic, and the answer used to be whoever ran gofmt last. The three flags cost
// nothing measurable for a single line.
func (s *Store) blame(ctx context.Context, repo, sha, path string, line int, follow bool) (*Commit, error) {
	if err := ValidateRef(sha); err != nil {
		return nil, err
	}
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	if line < 1 {
		return nil, fmt.Errorf("%w: line %d", ErrBadPath, line)
	}

	args := []string{"blame", "--porcelain"}
	if follow {
		args = append(args, "-w", "-M", "-C")
	}
	// No "--" before the path: blame reads everything after --end-of-options as
	// a revision, the separator included. ValidatePath already rejects anything
	// that could pass for an option.
	arg := strconv.Itoa(line) + "," + strconv.Itoa(line)
	args = append(args, "-L", arg, "--end-of-options", sha, path)
	out, err := s.run(ctx, s.ClonePath(repo), args...)
	if err != nil {
		return nil, classify(err, path)
	}
	c := parseBlame(string(out))
	if c == nil {
		// A file that does not exist at this commit blames as emptiness rather
		// than as an error, and the two must not read the same.
		return nil, fmt.Errorf("%w: %s:%d at %s", ErrNotFound, path, line, sha)
	}
	return c, nil
}

// Contains answers "where did this commit end up" — branches, and tags when a
// repository has them; ours do not, so the caller says the list is branches only
// rather than pretending it is complete.
//
// Branches are read from both refs/heads and refs/remotes/origin: fetches update
// only the latter, so a commit pushed after the clone is on origin/master and
// nowhere else.
func (s *Store) Contains(ctx context.Context, repo, sha string) ([]string, []string, error) {
	if err := ValidateRef(sha); err != nil {
		return nil, nil, err
	}
	bOut, err := s.run(ctx, s.ClonePath(repo), "branch", "--all", "--contains", sha, "--format=%(refname)")
	if err != nil {
		return nil, nil, classify(err, sha)
	}
	tOut, err := s.run(ctx, s.ClonePath(repo), "tag", "--contains", sha)
	if err != nil {
		return nil, nil, classify(err, sha)
	}
	return branchNames(splitLines(string(bOut))), splitLines(string(tOut)), nil
}

// branchNames turns full ref names into the names a person uses, merging the
// clone's stale heads with the remote-tracking refs and dropping origin/HEAD.
func branchNames(refs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range refs {
		name := ref
		switch {
		case strings.HasPrefix(ref, "refs/remotes/origin/"):
			name = strings.TrimPrefix(ref, "refs/remotes/origin/")
		case strings.HasPrefix(ref, "refs/heads/"):
			name = strings.TrimPrefix(ref, "refs/heads/")
		}
		if name == "" || name == "HEAD" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// RangeOptions are the inputs of Range.
type RangeOptions struct {
	From string
	To   string
	Path string
	// Files adds the changed paths to every commit. Off by default: a release of
	// fifty commits carries hundreds of them.
	Files bool
	Max   int
}

// Range answers "what went into this release" — 0.011s for a range, which is
// why a range is always cheaper than asking about history at large.
func (s *Store) Range(ctx context.Context, repo string, opts RangeOptions) ([]Commit, error) {
	if err := ValidateRef(opts.From); err != nil {
		return nil, err
	}
	if err := ValidateRef(opts.To); err != nil {
		return nil, err
	}

	args := []string{"log", "--format=" + logFormat, "--max-count=" + strconv.Itoa(LimitOr(opts.Max, 50))}
	if opts.Files {
		args = append(args, "--name-only")
	} else {
		args = append(args, "--no-patch")
	}
	args = append(args, "--end-of-options", opts.From+".."+opts.To)
	if opts.Path != "" {
		if err := ValidatePath(opts.Path); err != nil {
			return nil, err
		}
		args = append(args, "--", opts.Path)
	}
	out, err := s.run(ctx, s.ClonePath(repo), args...)
	if err != nil {
		return nil, classify(err, opts.Path)
	}
	if opts.Files {
		return parseCommitsWithFiles(string(out)), nil
	}
	return parseCommits(string(out)), nil
}

// Pickaxe answers "when did this string appear or disappear". The range is not
// optional: bounded it costs 0.47s, over the whole history 24s — the single
// widest gap between a good and a bad call in this package.
func (s *Store) Pickaxe(ctx context.Context, repo, from, to, needle string, limit int) ([]Commit, error) {
	if err := ValidateRef(from); err != nil {
		return nil, err
	}
	if err := ValidateRef(to); err != nil {
		return nil, err
	}
	if needle == "" {
		return nil, fmt.Errorf("%w: empty search string", ErrBadPath)
	}

	out, err := s.run(ctx, s.ClonePath(repo),
		"log", "--format="+logFormat, "--max-count="+strconv.Itoa(LimitOr(limit, 20)),
		"-S", needle, "--end-of-options", from+".."+to)
	if err != nil {
		return nil, classify(err, needle)
	}
	return parseCommits(string(out)), nil
}

// parseCommitsWithFiles reads the --name-only shape: a commit line, then the
// paths it touched, until the next commit line.
func parseCommitsWithFiles(out string) []Commit {
	var res []Commit
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if c, ok := parseCommitLine(line); ok {
			res = append(res, c)
			continue
		}
		if line != "" && len(res) > 0 {
			last := &res[len(res)-1]
			last.Files = append(last.Files, line)
		}
	}
	return res
}

func parseCommits(out string) []Commit {
	var res []Commit
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if c, ok := parseCommitLine(line); ok {
			res = append(res, c)
		}
	}
	return res
}

// parseCommitLine recognises a formatted commit and nothing else: -L
// interleaves diff hunks and --name-only interleaves paths, and neither is a
// commit.
func parseCommitLine(line string) (Commit, bool) {
	parts := strings.Split(line, logSep)
	if len(parts) != 5 {
		return Commit{}, false
	}
	return Commit{
		SHA: parts[0], Short: parts[1], Subject: parts[2], Author: parts[3], Date: parts[4],
	}, true
}

// parseBlame reads the porcelain header, which is the only stable shape blame
// offers: "<sha> <orig-line> <final-line>" and then "key value" lines.
func parseBlame(out string) *Commit {
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil
	}
	sha, _, ok := strings.Cut(lines[0], " ")
	if !ok || len(sha) < 7 {
		return nil
	}

	c := &Commit{SHA: sha, Short: sha[:7]}
	var unix, tz string
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "author":
			c.Author = value
		case "summary":
			c.Subject = value
		case "author-time":
			unix = value
		case "author-tz":
			tz = value
		}
	}
	// Porcelain reports a unix timestamp and an offset where log reports
	// ISO-8601: a caller comparing a date to a Sentry timestamp must not have to
	// know which mode produced it.
	c.Date = blameDate(unix, tz)
	return c
}

func blameDate(unix, tz string) string {
	sec, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return unix
	}
	t := time.Unix(sec, 0).UTC()
	if off, err := time.Parse("-0700", tz); err == nil {
		_, offset := off.Zone()
		t = t.In(time.FixedZone(tz, offset))
	}
	return t.Format(time.RFC3339)
}

func splitLines(out string) []string {
	var res []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			res = append(res, line)
		}
	}
	return res
}

// LimitOr is n when positive and the default otherwise. Exported because the
// domain asks the same question about its own limits.
func LimitOr(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// Hunk is one changed range. Start and Count describe the new file; Count is
// zero for a deletion, which is how "this code is gone" stays distinguishable
// from "this code changed" — Start then names the line the removed block used
// to follow. OldStart and OldCount describe the same hunk in the old file.
type Hunk struct {
	Start    int
	Count    int
	OldStart int
	OldCount int
}

// Covers reports whether a line of the new file lies inside the hunk.
func (h Hunk) Covers(line int) bool {
	return h.Count > 0 && line >= h.Start && line < h.Start+h.Count
}

// Diff is what a range of commits changed, per file of the new tree.
type Diff struct {
	// Hunks are the changed ranges per path. A deleted file is keyed by its
	// old path and carries one hunk with Count zero.
	Hunks map[string][]Hunk
	// Renamed maps a new path to its old one for files git recognised as moved;
	// a move without a content change has an entry here and no hunks. Without
	// rename detection a moved file is a deletion plus an addition, and every
	// function in it reads as new.
	Renamed map[string]string
}

// ChangedLines returns, per file, the line ranges a range of commits changed.
// --unified=0 asks git for exactly the changed lines and nothing around them:
// context lines would widen every hunk into neighbouring symbols.
func (s *Store) ChangedLines(ctx context.Context, repo, from, to string) (*Diff, error) {
	if err := ValidateRef(from); err != nil {
		return nil, err
	}
	if err := ValidateRef(to); err != nil {
		return nil, err
	}

	// core.quotePath off: git otherwise prints a non-ASCII path as
	// "\303\274.go" and the parser below would have to undo it for every file.
	// Paths with a tab or a quote are still quoted, and diffPath handles those.
	out, err := s.runEnv(ctx, s.ClonePath(repo), []string{
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.quotePath", "GIT_CONFIG_VALUE_0=false",
	}, "diff", "--unified=0", "--no-color", "--find-renames", "--end-of-options", from+".."+to)
	if err != nil {
		return nil, classify(err, from+".."+to)
	}
	return parseDiff(string(out)), nil
}

// diffHunkRe reads the one line of a unified diff that carries numbers:
// @@ -12,3 +14,5 @@ — old side first, new side second.
var diffHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseDiff(out string) *Diff {
	d := &Diff{Hunks: map[string][]Hunk{}, Renamed: map[string]string{}}
	current, old := "", ""

	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			current, old = "", ""
		case strings.HasPrefix(line, "rename from "):
			old = diffPath(strings.TrimPrefix(line, "rename from "), "")
		case strings.HasPrefix(line, "rename to "):
			current = diffPath(strings.TrimPrefix(line, "rename to "), "")
			d.Renamed[current] = old
		case strings.HasPrefix(line, "--- "):
			if p := diffPath(strings.TrimPrefix(line, "--- "), "a/"); p != "" {
				old = p
			}
		case strings.HasPrefix(line, "+++ "):
			// A deleted file has no new side; keyed by its old path, so it is
			// listed as gone rather than dropped from the answer.
			if p := diffPath(strings.TrimPrefix(line, "+++ "), "b/"); p != "" {
				current = p
			} else {
				current = old
			}
		case strings.HasPrefix(line, "@@") && current != "":
			m := diffHunkRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			d.Hunks[current] = append(d.Hunks[current], Hunk{
				OldStart: atoiOr(m[1], 0), OldCount: atoiOr(m[2], 1),
				Start: atoiOr(m[3], 0), Count: atoiOr(m[4], 1),
			})
		}
	}
	return d
}

// diffPath reads a path from a diff header line, undoing git's C-style quoting
// and stripping the a/ or b/ prefix. /dev/null comes back empty.
func diffPath(raw, prefix string) string {
	raw = strings.TrimRight(raw, "\t ")
	if strings.HasPrefix(raw, "\"") {
		if u, err := strconv.Unquote(raw); err == nil {
			raw = u
		}
	}
	if raw == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(raw, prefix)
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
