package code

import (
	"context"

	"github.com/vmkteam/ringsrv/pkg/client/git"
)

// The five modes of history. Fixed on purpose: the mode picks the git command,
// so nothing the model writes can become an argument of one.
const (
	ModeLines    = "lines"
	ModeBlame    = "blame"
	ModeContains = "contains"
	ModeRange    = "range"
	ModePickaxe  = "pickaxe"
)

// Modes lists them in the order the tool describes them.
var Modes = []string{ModeLines, ModeBlame, ModeContains, ModeRange, ModePickaxe}

// HistoryQuery is what code_history asks for. Which fields matter depends on
// Mode; a missing one is refused by name rather than defaulted.
type HistoryQuery struct {
	Mode string

	Ref  string
	Path string

	LineFrom int
	LineTo   int
	Line     int

	SHA string

	From string
	To   string

	Query     string
	WithFiles bool
	Max       int
}

// History is the answer of every mode. Contains fills Branches and Tags instead
// of Commits — the question it answers is about refs — and the rest of the
// shape stays the same so the caller reads one envelope.
type History struct {
	Mode     string
	Ref      string
	Commits  []Commit
	Branches []string
	Tags     []string
	// NoTags says this mirror carries no tags at all (D10). Without it an empty
	// tag list reads as "this commit is in no release", which is a different
	// and wrong answer.
	NoTags bool
}

// History answers a question about the past of a line, a commit or a range.
func (s *Manager) History(ctx context.Context, repo Repo, q HistoryQuery) (*History, error) {
	if q.Path != "" && repo.Excluded(q.Path) {
		return nil, ErrPathDenied
	}

	out := &History{Mode: q.Mode, Commits: []Commit{}}
	var commits []git.Commit
	var err error

	switch q.Mode {
	case ModeLines:
		commits, err = s.historyLines(ctx, repo, q, out)
	case ModeBlame:
		commits, err = s.historyBlame(ctx, repo, q, out)
	case ModeContains:
		err = s.historyContains(ctx, repo, q, out)
	case ModeRange:
		commits, err = s.historyRange(ctx, repo, q, out)
	case ModePickaxe:
		commits, err = s.historyPickaxe(ctx, repo, q, out)
	default:
		return nil, &ModeError{Mode: q.Mode, Modes: Modes}
	}
	if err != nil {
		return nil, err
	}

	for _, c := range commits {
		out.Commits = append(out.Commits, newCommit(repo, c))
	}
	return out, nil
}

func (s *Manager) historyLines(ctx context.Context, repo Repo, q HistoryQuery, out *History) ([]git.Commit, error) {
	sha, err := s.resolve(ctx, repo, q.Ref)
	if err != nil {
		return nil, err
	}
	out.Ref = sha
	return s.repos.LogLines(ctx, repo.Name, sha, q.Path, q.LineFrom, q.LineTo, q.Max)
}

func (s *Manager) historyBlame(ctx context.Context, repo Repo, q HistoryQuery, out *History) ([]git.Commit, error) {
	sha, err := s.resolve(ctx, repo, q.Ref)
	if err != nil {
		return nil, err
	}
	out.Ref = sha
	c, err := s.repos.Blame(ctx, repo.Name, sha, q.Path, q.Line)
	if err != nil {
		return nil, err
	}
	return []git.Commit{*c}, nil
}

func (s *Manager) historyContains(ctx context.Context, repo Repo, q HistoryQuery, out *History) error {
	sha, err := s.resolve(ctx, repo, q.SHA)
	if err != nil {
		return err
	}
	out.Ref = sha

	branches, tags, err := s.repos.Contains(ctx, repo.Name, sha)
	if err != nil {
		return err
	}
	out.Branches, out.Tags, out.NoTags = branches, tags, !repo.Cfg.Tags
	return nil
}

func (s *Manager) historyRange(ctx context.Context, repo Repo, q HistoryQuery, out *History) ([]git.Commit, error) {
	shas, err := s.resolveAll(ctx, repo, q.From, q.To)
	if err != nil {
		return nil, err
	}
	out.Ref = shas[1]
	return s.repos.Range(ctx, repo.Name, git.RangeOptions{
		From:  shas[0],
		To:    shas[1],
		Path:  q.Path,
		Files: q.WithFiles,
		Max:   q.Max,
	})
}

func (s *Manager) historyPickaxe(ctx context.Context, repo Repo, q HistoryQuery, out *History) ([]git.Commit, error) {
	// The range is not optional: bounded this costs 0.47s, over the whole
	// history 24s. Refused before git is called, because the
	// point is not to pay those 24 seconds.
	if q.From == "" || q.To == "" {
		return nil, ErrPickaxeRange
	}
	if q.Query == "" {
		return nil, ErrPickaxeQuery
	}

	shas, err := s.resolveAll(ctx, repo, q.From, q.To)
	if err != nil {
		return nil, err
	}
	out.Ref = shas[1]
	return s.repos.Pickaxe(ctx, repo.Name, shas[0], shas[1], q.Query, q.Max)
}
