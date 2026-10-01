package files

import (
	"fmt"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/vmkteam/ringsrv/pkg/ring/target"
)

// params are the route's query parameters.
type params struct {
	// File is the provider's file id.
	File string
	// Offset is how many lines to skip; numbering starts at 1 after them.
	Offset int
	// Limit is how many lines to return at most.
	Limit int
	// Grep keeps only the lines holding it, compared without case.
	Grep string
}

// Bounds of the parameters. maxLimit agrees with the catalogue's limit<=1000.
const (
	defaultLimit = 200
	maxLimit     = 1000
	maxGrep      = 200
)

// fileIDRe is a file id: letters and digits after an F. A URL never passes,
// which is what keeps the route from being a way to fetch an address.
var fileIDRe = regexp.MustCompile(`^F[A-Z0-9]{6,20}$`)

// parseParams reads the route's parameters through the splitter the allowlist
// used, so the route acts on what was checked. A parameter given twice is
// refused rather than resolved: which one wins would be a guess.
func parseParams(rawPath string) (params, error) {
	p := params{Limit: defaultLimit}
	seen := map[string]bool{}
	for _, pair := range target.QueryPairs(rawPath) {
		if seen[pair.Key] {
			return p, fmt.Errorf("%w: %q is given twice", ErrBadParams, pair.Key)
		}
		seen[pair.Key] = true
		if err := p.set(pair.Key, pair.Value); err != nil {
			return p, err
		}
	}
	if p.File == "" {
		return p, fmt.Errorf("%w: file is required — the id of a file, F…, from files[] of a message", ErrBadParams)
	}
	return p, nil
}

func (p *params) set(key, value string) error {
	var err error
	switch key {
	case "file":
		if !fileIDRe.MatchString(value) {
			return fmt.Errorf("%w: file %q is not a file id — F followed by capital letters and digits", ErrBadParams, value)
		}
		p.File = value
	case "offset":
		p.Offset, err = bounded(key, value, 0, 1<<31-1)
	case "limit":
		p.Limit, err = bounded(key, value, 1, maxLimit)
	case "grep":
		if value == "" || utf8.RuneCountInString(value) > maxGrep {
			return fmt.Errorf("%w: grep must be 1..%d characters", ErrBadParams, maxGrep)
		}
		p.Grep = value
	default:
		// The allowlist decides which names reach here; one this route does
		// not read is an upstream parameter that has no upstream to go to.
		return fmt.Errorf("%w: %q means nothing on this path — it takes file, offset, limit and grep", ErrBadParams, key)
	}
	return err
}

func bounded(key, value string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%w: %s must be an integer in %d..%d", ErrBadParams, key, lo, hi)
	}
	return n, nil
}
