package code

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
)

// Limits. A 300-commit range means 1876 changed files and tens of seconds of
// git diff: the cap is what keeps the tool from being the reason an incident
// call stalls.
const (
	MaxRangeCommits   = 200
	defaultMaxSymbols = 50
	// DefaultChangedShow caps the context list that follows the answer. It is
	// capped harder than the intersection because it is there for when the
	// intersection is empty, not to be read through.
	DefaultChangedShow = 30
)

// How a symbol was changed. Added comes from a hunk that covers the whole
// symbol; body and signature from where the diff landed inside it. Removed
// names a symbol of the previous tree that the release tree no longer has — or,
// for a file no parser reads, the file itself. Renamed is file-level only: a
// moved file with no content change has no hunks at all.
const (
	ChangeAdded     = "added"
	ChangeRemoved   = "removed"
	ChangeBody      = "body"
	ChangeSignature = "signature"
	ChangeRenamed   = "renamed"
)

// KindFile marks a row that names a file rather than a symbol.
const KindFile = "file"

// How strongly an intersection row is tied to its frame. In a control group,
// three of ten releases unrelated to the failure still produced a non-empty
// intersection, every time through a file-level row. Such rows are still
// reported — a package-level constant in the same file can be the cause — but
// they must not read like a symbol hit.
const (
	// MatchSymbol: the frame line lies inside a symbol the release changed.
	MatchSymbol = "symbol"
	// MatchHunk: no symbol owns the line, but a changed hunk covers it — a
	// constant, a var block, a struct tag.
	MatchHunk = "hunk"
	// MatchFile: the file changed, but neither a changed symbol nor a changed
	// hunk covers the frame line. The weakest answer the tool gives.
	MatchFile = "file"
)

// BlastQuery is what blast_radius asks for.
type BlastQuery struct {
	Release string
	Prev    string
	// Frames are the stack frames as "path:line". This is the whole point of the
	// tool: without them the answer is a list of everything that changed.
	Frames     []string
	MaxSymbols int
}

// Symbol is one symbol the release touched. An empty Name with KindFile means a
// change no symbol owns — a package-level constant, a data table, imports.
type Symbol struct {
	Path string
	Name string
	// Receiver is the type a method belongs to, read from the signature. Two
	// types in one file may both have a String; the name alone would merge them.
	Receiver string
	Kind     string
	Line     int
	EndLine  int
	Change   string
	Layer    string
	// Frame is the stack frame this symbol was matched against; only set in the
	// intersection, where it is the reason the symbol is in the answer.
	Frame string
	// Match says how the frame and the row are related; only set in the
	// intersection.
	Match string
	// Task is the issue key of the commit that produced this change, empty when
	// the commit followed no convention.
	Task string
}

// Blast is the answer of blast_radius. The intersection is the answer and
// everything after it is context for when the answer is empty.
type Blast struct {
	Release string
	Prev    string
	Counts  Counts
	// Intersection holds the symbols the release changed that the stack trace
	// also names. Empty with frames given is itself an answer: this release did
	// not touch the code that broke.
	Intersection []Symbol
	Changed      []Symbol
	// EmptyRange says the two commits are the same or nothing lies between them:
	// a known release with nothing in it is a different answer from an unknown
	// release, and both are useful.
	EmptyRange bool
}

// Counts is the shape of the range before anything is read.
type Counts struct {
	Commits int
	Files   int
	// Symbols counts what was resolved, not what changed: with frames given only
	// the files a frame points into are resolved to symbols. Files is the honest
	// "how big was this release" number.
	Symbols      int
	Intersection int
	Frames       int
	// UnmatchedFrames counts frames that named no symbol in this repository —
	// usually a path from another service or a build path. Without this number
	// an empty intersection reads as "the release is innocent" when it may only
	// mean the frames never matched anything.
	UnmatchedFrames int
	// WeakMatches counts intersection rows with MatchFile: the file changed, the
	// frame line did not. An intersection made only of these is not a cause, and
	// the answer says so.
	WeakMatches int
}

// Blast intersects what a release changed with the frames of a stack trace.
func (s *Manager) Blast(ctx context.Context, repo Repo, q BlastQuery) (*Blast, error) {
	if !s.HasEngine() {
		return nil, ErrNoEngine
	}
	if q.Prev == "" {
		return nil, ErrPrevRequired
	}

	shas, err := s.resolveAll(ctx, repo, q.Prev, q.Release)
	if err != nil {
		return nil, err
	}
	prev, release := shas[0], shas[1]

	commits, err := s.repos.Range(ctx, repo.Name, git.RangeOptions{
		From: prev, To: release, Files: true, Max: MaxRangeCommits + 1,
	})
	if err != nil {
		return nil, err
	}
	if len(commits) > MaxRangeCommits {
		return nil, &RangeTooBigError{Commits: len(commits), Max: MaxRangeCommits}
	}

	out := &Blast{
		Release: release, Prev: prev,
		Intersection: []Symbol{},
		Counts:       Counts{Commits: len(commits), Frames: len(q.Frames)},
	}
	if len(commits) == 0 {
		out.EmptyRange = true
		return out, nil
	}
	return s.buildBlast(ctx, repo, q, out, commits)
}

// blastScope is everything the per-file work needs, bundled so the functions
// below do not carry seven parameters each.
type blastScope struct {
	repo    Repo
	prev    string
	release string
	dir     string
	diff    *git.Diff
	// tasks is the fallback attribution: the last commit of the range that
	// touched the file. Rows in files a frame points into are attributed more
	// precisely, by blaming the hunk itself.
	tasks map[string]string
	// precise turns on per-hunk attribution. It is on only with frames: it costs
	// one blame per row, and without frames every file of the release would pay
	// it for a list nobody reads to the end.
	precise bool
}

// buildBlast does the work once the range is known to be sane.
func (s *Manager) buildBlast(ctx context.Context, repo Repo, q BlastQuery, out *Blast, commits []git.Commit) (*Blast, error) {
	diff, err := s.repos.ChangedLines(ctx, repo.Name, out.Prev, out.Release)
	if err != nil {
		return nil, err
	}
	files := changedFiles(diff, repo)
	out.Counts.Files = len(files)

	dir, release, err := s.indexedTree(ctx, repo.Name, out.Release)
	if err != nil {
		return nil, err
	}
	defer release()

	scope := blastScope{
		repo: repo, prev: out.Prev, release: out.Release, dir: dir, diff: diff,
		tasks: commitTasks(repo, commits), precise: len(q.Frames) > 0,
	}

	// The engine is asked only about the files a frame points into: it costs one
	// process per file, and the others produce rows intersect() throws away.
	// Without frames there is nothing to narrow by, and the caller gets the
	// file-level list.
	symbolFiles := files
	if len(q.Frames) > 0 {
		symbolFiles = framedFiles(files, q.Frames)
	}
	changed := s.changedSymbols(ctx, scope, symbolFiles)
	if len(symbolFiles) != len(files) {
		changed = append(changed, fileEntries(scope, without(files, symbolFiles))...)
	}
	out.Counts.Symbols = len(changed)

	if len(q.Frames) > 0 {
		out.Intersection, out.Counts.UnmatchedFrames = intersect(changed, q.Frames, diff.Hunks)
		out.Counts.Intersection = len(out.Intersection)
		for _, c := range out.Intersection {
			if c.Match == MatchFile {
				out.Counts.WeakMatches++
			}
		}
		if limit := git.LimitOr(q.MaxSymbols, defaultMaxSymbols); len(out.Intersection) > limit {
			out.Intersection = out.Intersection[:limit]
		}
	}

	if len(changed) > DefaultChangedShow {
		changed = changed[:DefaultChangedShow]
	}
	out.Changed = changed
	return out, nil
}

// changedFiles lists the paths the range touched, excluded ones dropped: the
// files with hunks plus the ones that only moved.
func changedFiles(diff *git.Diff, repo Repo) []string {
	seen := map[string]bool{}
	var files []string
	add := func(path string) {
		if !seen[path] && !repo.Excluded(path) {
			seen[path] = true
			files = append(files, path)
		}
	}
	for path := range diff.Hunks {
		add(path)
	}
	for path := range diff.Renamed {
		add(path)
	}
	sort.Strings(files)
	return files
}

// framedFiles keeps the files some frame points into.
func framedFiles(files, frames []string) []string {
	res := make([]string, 0, len(frames))
	for _, f := range files {
		for _, frame := range frames {
			path, _, ok := ParseFrame(frame)
			if ok && SameFile(f, path) {
				res = append(res, f)
				break
			}
		}
	}
	return res
}

// without returns the files that are not in keep.
func without(files, keep []string) []string {
	skip := make(map[string]bool, len(keep))
	for _, f := range keep {
		skip[f] = true
	}
	res := make([]string, 0, len(files)-len(keep))
	for _, f := range files {
		if !skip[f] {
			res = append(res, f)
		}
	}
	return res
}

// fileEntry describes a change no symbol owns — a file the engine could not
// read, a file no frame pointed into, lines between symbols, a file that only
// moved. The empty Name is what says so, and KindFile is what makes that
// readable instead of looking like a symbol whose name went missing.
func (s *Manager) fileEntry(ctx context.Context, sc blastScope, path, change string) Symbol {
	return Symbol{
		Path: path, Kind: KindFile, Change: change,
		Layer: sc.repo.Cfg.Layer(path),
		Task:  s.taskFor(ctx, sc, path, sc.diff.Hunks[path]),
	}
}

// fileChange says what happened to a file no symbol was resolved for.
func fileChange(sc blastScope, path string) string {
	hunks := sc.diff.Hunks[path]
	if len(hunks) == 0 {
		if _, moved := sc.diff.Renamed[path]; moved {
			return ChangeRenamed
		}
	}
	for _, h := range hunks {
		if h.Count > 0 {
			return ChangeBody
		}
	}
	return ChangeRemoved
}

// fileEntries builds the rows for the unframed rest of the release: those rows
// are context, and the cheap attribution is enough for them.
func fileEntries(sc blastScope, paths []string) []Symbol {
	res := make([]Symbol, 0, len(paths))
	for _, p := range paths {
		res = append(res, Symbol{
			Path: p, Kind: KindFile, Change: fileChange(sc, p),
			Layer: sc.repo.Cfg.Layer(p), Task: sc.tasks[p],
		})
	}
	return res
}

// taskFor names the issue behind a change. With frames given the hunk itself is
// blamed at the release commit: the file's last commit in the range was the
// wrong answer whenever two tasks landed in one file. Without frames, or for a
// pure deletion that has no line left to blame, the file's last commit is it.
func (s *Manager) taskFor(ctx context.Context, sc blastScope, path string, hunks []git.Hunk) string {
	if sc.precise {
		for _, h := range hunks {
			if h.Count == 0 {
				continue
			}
			if c, err := s.repos.BlameExact(ctx, sc.repo.Name, sc.release, path, h.Start); err == nil {
				return sc.repo.Cfg.TaskID(c.Subject)
			}
			break
		}
	}
	return sc.tasks[path]
}

// changedSymbols maps changed line ranges onto symbols. This is where symbol
// boundaries earn their keep: a diff hunk either falls inside a symbol or it
// does not, with no "last symbol above the line" guessing.
func (s *Manager) changedSymbols(ctx context.Context, sc blastScope, files []string) []Symbol {
	var res []Symbol
	for _, path := range files {
		hunks := sc.diff.Hunks[path]
		if len(hunks) == 0 {
			// Only moved, or nothing we can read: a file-level row.
			res = append(res, s.fileEntry(ctx, sc, path, fileChange(sc, path)))
			continue
		}
		nodes, err := s.graph.FileSymbols(ctx, sc.dir, path)
		if err != nil {
			// A file the engine cannot read — deleted in the release, or a
			// language it does not parse — still counts as changed, so it shows
			// up as a file-level entry rather than vanishing.
			res = append(res, s.fileEntry(ctx, sc, path, fileChange(sc, path)))
			continue
		}
		res = append(res, s.fileSymbols(ctx, sc, path, nodes, hunks)...)
	}
	return res
}

// oldGoFile is a Go file as it was in prev: its symbols and its lines. It is
// what makes deletions readable. A deletion has no line in the release tree, and
// git merges a deleted function into the hunk of the edit just above it whenever
// the two are adjacent — so the new side alone attributed the whole hunk to the
// function that survived.
type oldGoFile struct {
	nodes []codegraph.Node
	lines []string
}

// oldGoFile reads the previous version of a Go file straight from the mirror.
// Nil for a file that is not Go, that did not exist in prev, or that does not
// parse: the new-side logic then does what it can.
func (s *Manager) oldGoFile(ctx context.Context, sc blastScope, path string) *oldGoFile {
	if !strings.HasSuffix(path, ".go") {
		return nil
	}
	oldPath := path
	if from, moved := sc.diff.Renamed[path]; moved {
		oldPath = from
	}
	src, err := s.repos.Blob(ctx, sc.repo.Name, sc.prev, oldPath)
	if err != nil {
		return nil
	}
	nodes, err := codegraph.GoSymbolsFromSource(oldPath, src)
	if err != nil {
		return nil
	}
	return &oldGoFile{nodes: nodes, lines: strings.Split(string(src), "\n")}
}

// trivial reports whether an old line carried nothing worth a row of its own:
// blank, a comment, a lone closing bracket.
func (o *oldGoFile) trivial(line int) bool {
	if line < 1 || line > len(o.lines) {
		return true
	}
	t := strings.TrimSpace(o.lines[line-1])
	return t == "" || t == "}" || t == ")" || strings.HasPrefix(t, "//")
}

// symbolKey tells two symbols apart the way a reader would: receiver and name.
func symbolKey(n *codegraph.Node) string { return n.Receiver + "." + n.Name }

// fileSymbols resolves the hunks of one file against its symbols.
func (s *Manager) fileSymbols(ctx context.Context, sc blastScope, path string, nodes []codegraph.Node, hunks []git.Hunk) []Symbol {
	var res []Symbol
	layer := sc.repo.Cfg.Layer(path)
	// Keyed by receiver and name, not by name: two types in one file may both
	// have a String, and the second used to be folded into the first.
	seen := map[string]bool{}
	byKey := make(map[string]*codegraph.Node, len(nodes))
	for i := range nodes {
		byKey[symbolKey(&nodes[i])] = &nodes[i]
	}
	outside, removedOutside := false, false

	add := func(n *codegraph.Node, h git.Hunk, change string) {
		key := symbolKey(n)
		if seen[key] {
			return
		}
		seen[key] = true
		res = append(res, Symbol{
			Path: path, Name: n.Name, Receiver: n.Receiver, Kind: n.Type,
			Line: n.StartLine, EndLine: n.EndLine, Change: change,
			Layer: layer, Task: s.taskFor(ctx, sc, path, []git.Hunk{h}),
		})
	}
	addRemoved := func(n *codegraph.Node) {
		key := symbolKey(n)
		if seen[key] {
			return
		}
		seen[key] = true
		// No line: the symbol is not in the release tree. The task is the file's
		// last commit in the range — there is nothing left to blame.
		res = append(res, Symbol{
			Path: path, Name: n.Name, Receiver: n.Receiver, Kind: n.Type,
			Change: ChangeRemoved, Layer: layer, Task: sc.tasks[path],
		})
	}

	old := s.oldGoFile(ctx, sc, path)
	for _, h := range hunks {
		for line := h.Start; line < h.Start+h.Count; line++ {
			n := codegraph.SymbolAt(nodes, line)
			if n == nil {
				outside = true
				continue
			}
			add(n, h, changeAt(h, n))
		}

		switch {
		case old != nil:
			// The old side of the hunk, line by line: inside a symbol that is
			// gone — that symbol was removed; inside one that survived — its body
			// changed; inside nothing — package-level code went away.
			for line := h.OldStart; line < h.OldStart+h.OldCount; line++ {
				if old.trivial(line) {
					continue
				}
				on := codegraph.SymbolAt(old.nodes, line)
				switch {
				case on == nil:
					removedOutside = true
				case byKey[symbolKey(on)] == nil:
					addRemoved(on)
				default:
					add(byKey[symbolKey(on)], h, ChangeBody)
				}
			}
		case h.Count == 0:
			// No old tree to read: a deletion hunk names the line the removed
			// block used to follow. Inside a symbol's body that means the body
			// lost lines; anywhere else only the file row can say that something
			// went away. Attributing it to whatever symbol owned that line named
			// the function *before* the deleted one.
			if n := codegraph.SymbolAt(nodes, h.Start); n != nil && h.Start >= n.StartLine && h.Start < n.EndLine {
				add(n, h, ChangeBody)
			} else {
				removedOutside = true
			}
		}
	}

	// Lines that fall between symbols — imports, package-level consts and vars —
	// are still a change to the file, even when other lines in the same file did
	// land inside functions: a release that changed both a function and a
	// package-level SQL constant used to report only the function. "This release
	// did not touch the code that broke" is the one answer this tool must never
	// give wrongly.
	switch {
	case outside:
		res = append(res, s.fileEntry(ctx, sc, path, ChangeBody))
	case removedOutside:
		res = append(res, s.fileEntry(ctx, sc, path, ChangeRemoved))
	}
	return res
}

// changeAt says how a hunk with new lines touched a symbol. A new function
// explains a new failure mode, a changed signature explains a caller that
// stopped compiling, and a changed body explains everything else.
func changeAt(h git.Hunk, n *codegraph.Node) string {
	end := h.Start + h.Count - 1
	switch {
	case h.Start <= n.StartLine && end >= n.EndLine:
		// The hunk covers the symbol from its first line to its last: this symbol
		// did not change, it appeared.
		return ChangeAdded
	case h.Start == n.StartLine:
		return ChangeSignature
	default:
		return ChangeBody
	}
}

// commitTasks maps a file to the issue key of the last commit that touched it in
// this range: the cheap attribution, used where nobody is about to read the row
// closely.
func commitTasks(repo Repo, commits []git.Commit) map[string]string {
	res := make(map[string]string)
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		task := repo.Cfg.TaskID(c.Subject)
		for _, f := range c.Files {
			res[f] = task
		}
	}
	return res
}

// intersect keeps the symbols a frame points into. Frames that match nothing are
// counted rather than dropped: "no intersection" and "the frames never matched
// this repository" look identical in an answer and mean opposite things.
func intersect(changed []Symbol, frames []string, hunks map[string][]git.Hunk) ([]Symbol, int) {
	res := []Symbol{}
	unmatched := 0

	for _, frame := range frames {
		path, line, ok := ParseFrame(frame)
		if !ok {
			unmatched++
			continue
		}
		// A symbol is a better answer than "somewhere in this file", so the
		// file-level entries are held back and used only when no symbol in the
		// file covers the line. Reporting both would put a nameless row next to
		// every real one and undo the narrowing this tool exists for.
		var symbols, fileLevel []Symbol
		for _, c := range changed {
			if !SameFile(c.Path, path) {
				continue
			}
			switch {
			case c.Kind != KindFile && c.Line == 0:
				// A removed symbol has no line in this tree: it is context, never
				// a match.
			case c.Line == 0:
				c.Match = fileMatch(hunks[c.Path], line)
				fileLevel = append(fileLevel, c)
			case line == 0 || (c.Line <= line && line <= c.EndLine):
				c.Match = MatchSymbol
				symbols = append(symbols, c)
			}
		}
		matched := symbols
		if len(matched) == 0 {
			matched = fileLevel
		}
		for _, c := range matched {
			c.Frame = frame
			res = append(res, c)
		}
		if len(matched) == 0 {
			unmatched++
		}
	}
	return res, unmatched
}

// fileMatch grades a file-level row against the frame line: a changed hunk that
// covers the line is a real hit on code no symbol owns; anything else is the
// file having changed somewhere else.
func fileMatch(hunks []git.Hunk, line int) string {
	if line > 0 {
		for _, h := range hunks {
			if h.Covers(line) {
				return MatchHunk
			}
		}
	}
	return MatchFile
}

// ParseFrame reads "path:line", the shape Sentry and a Go panic both print.
func ParseFrame(frame string) (string, int, bool) {
	frame = strings.TrimSpace(frame)
	if frame == "" {
		return "", 0, false
	}
	path, lineStr, ok := strings.Cut(frame, ":")
	if !ok {
		return frame, 0, true
	}
	line := 0
	if _, err := fmt.Sscanf(lineStr, "%d", &line); err != nil {
		return frame, 0, true
	}
	return path, line, true
}

// SameFile compares by suffix: Sentry reports build paths, Go panics report
// module paths, and neither is repository-relative. Matching on the tail is what
// makes a frame from production line up with a file in the mirror.
func SameFile(repoPath, framePath string) bool {
	repoPath, framePath = strings.TrimPrefix(repoPath, "./"), strings.TrimPrefix(framePath, "./")
	return repoPath == framePath ||
		strings.HasSuffix(framePath, "/"+repoPath) ||
		strings.HasSuffix(repoPath, "/"+framePath)
}
