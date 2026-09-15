package code

import (
	"context"

	"github.com/vmkteam/ringsrv/pkg/client/git"
)

// Kinds of reference. Implementors is named and refused on purpose: an unknown
// kind would be answered with "unknown kind", which reads as a typo rather than
// as a capability we do not have.
const (
	KindCallers      = "callers"
	KindImplementors = "implementors"
	KindBoth         = "both"
)

// Kinds lists them for the refusal that names the alternatives.
var Kinds = []string{KindCallers, KindImplementors, KindBoth}

// defaultMaxRefs caps the answer. Forty call sites is already more than anyone
// reads; past that the answer is a list, not an answer.
const defaultMaxRefs = 40

// RefsQuery is what code_refs asks for.
type RefsQuery struct {
	Ref    string
	Symbol string
	// Path narrows an ambiguous name. Needed only when the engine refuses to
	// guess between two symbols that share it.
	Path string
	Kind string
	Max  int
}

// Ref is one site that references the symbol.
type Ref struct {
	Path   string
	Line   int
	Symbol string
	// Layer comes from the repository's Layers map: it costs nothing and
	// immediately answers "did this cross a layer boundary".
	Layer string
	// Confidence is the engine's own word: "inferred" means the edge was matched
	// by name, which is where Go methods of one name get confused.
	Confidence string
	Relation   string
}

// Refs is the answer of code_refs.
type Refs struct {
	Ref     string
	Kind    string
	Callers []Ref
}

// Refs answers "who calls this symbol" at a commit. Kind is validated here
// rather than by the caller: the refusal of implementors is a property of the
// engine, not of the protocol.
func (s *Manager) Refs(ctx context.Context, repo Repo, q RefsQuery) (*Refs, error) {
	switch q.Kind {
	case KindImplementors:
		// A refusal, not an empty list: an empty list reads as "nothing
		// implements this interface", which is a different and wrong answer.
		return nil, ErrImplementors
	case KindCallers, KindBoth:
	default:
		return nil, &KindError{Kind: q.Kind, Kinds: Kinds}
	}
	if !s.HasEngine() {
		return nil, ErrNoEngine
	}

	sha, err := s.resolve(ctx, repo, q.Ref)
	if err != nil {
		return nil, err
	}

	dir, release, err := s.indexedTree(ctx, repo.Name, sha)
	if err != nil {
		return nil, err
	}
	defer release()

	found, err := s.graph.References(ctx, dir, q.Symbol, q.Path, git.LimitOr(q.Max, defaultMaxRefs))
	if err != nil {
		return nil, err
	}

	out := &Refs{Ref: sha, Kind: q.Kind, Callers: []Ref{}}
	for _, r := range found {
		if repo.Excluded(r.Path) {
			continue
		}
		out.Callers = append(out.Callers, Ref{
			Path: r.Path, Line: r.StartLine, Symbol: r.Name,
			Layer: repo.Cfg.Layer(r.Path), Confidence: r.Confidence, Relation: r.Relation,
		})
	}
	return out, nil
}
