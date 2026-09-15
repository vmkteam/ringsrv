package code

import (
	"errors"
	"fmt"
)

// The refusals this package makes on its own. Client errors travel unwrapped
// from git and codegraph, so the layer above maps them by errors.Is on the
// client's own sentinels rather than on copies made here.
var (
	// ErrRefRequired guards the mistake the code tools exist to prevent:
	// reading a branch instead of the deployed commit.
	ErrRefRequired = errors.New("ref is required")

	// ErrPathDenied is a path the catalogue keeps out of answers.
	ErrPathDenied = errors.New("path is excluded for this repository")

	// ErrNoEngine is a symbol-level question asked of an instance that runs
	// without the AST engine.
	ErrNoEngine = errors.New("this instance runs without the AST engine")

	// ErrLineOrSymbol says why is asked without saying about what.
	ErrLineOrSymbol = errors.New("either line or symbol is required")

	// ErrPrevRequired is blast_radius without the previously deployed commit.
	// Asking Sentry for it would be a call on the caller's behalf against a
	// target they may not be allowed to read.
	ErrPrevRequired = errors.New("prev is required")

	// ErrPickaxeRange is a pickaxe over the whole history: bounded it costs half
	// a second, unbounded twenty-four.
	ErrPickaxeRange = errors.New("pickaxe requires from and to")

	// ErrPickaxeQuery is a pickaxe without the string to look for.
	ErrPickaxeQuery = errors.New("pickaxe requires query")

	// ErrNoTracker means this repository names no issue tracker, or the profile
	// does not allow the request the issue key would need.
	ErrNoTracker = errors.New("no issue tracker for this repository")

	// ErrTrackerDown is a tracker that was asked and did not answer usefully.
	// It is a missing field in the answer, never a failed call.
	ErrTrackerDown = errors.New("issue tracker did not answer")

	// ErrImplementors is the honest refusal of kind=implementors: on Go the
	// engine matches interface implementations by name, which produces
	// plausible wrong answers rather than empty ones.
	ErrImplementors = errors.New("implementors are not supported")
)

// ModeError is a history mode this package does not have. It carries the
// available ones, so the answer lists them instead of leaving the caller to
// guess.
type ModeError struct {
	Mode  string
	Modes []string
}

func (e *ModeError) Error() string { return fmt.Sprintf("unknown mode %q", e.Mode) }

// KindError is an unknown kind of reference, with the same reasoning.
type KindError struct {
	Kind  string
	Kinds []string
}

func (e *KindError) Error() string { return fmt.Sprintf("unknown kind %q", e.Kind) }

// RangeTooBigError is a release too large to diff. The numbers travel with it
// because "narrow the range" without saying how far is not advice.
type RangeTooBigError struct {
	Commits int
	Max     int
}

func (e *RangeTooBigError) Error() string {
	return fmt.Sprintf("%d commits between these two, more than the %d this tool reads", e.Commits, e.Max)
}

// SymbolNotFoundError is a symbol that is not in this file at this commit.
type SymbolNotFoundError struct {
	Symbol string
	Path   string
	Ref    string
}

func (e *SymbolNotFoundError) Error() string {
	return fmt.Sprintf("%q not found in %s at %s", e.Symbol, e.Path, e.Ref)
}
