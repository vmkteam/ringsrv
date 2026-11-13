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
	// Sheet picks a sheet of a workbook, by name or by number from 1.
	Sheet string
	// Entry picks a file inside an archive by its full name there.
	Entry string
}

// Bounds of the parameters. maxLimit agrees with the catalogue's limit<=1000.
const (
	defaultLimit = 200
	maxLimit     = 1000
	maxGrep      = 200
	maxSheet     = 100
	maxEntry     = 1024
)

// fileID is what a provider's file ids look like. Nothing but letters and
// digits passes: a URL never does, which is what keeps the route from being a
// way to fetch an address.
type fileID struct {
	re *regexp.Regexp
	// hint says what an id is, in a refusal.
	hint string
}

// fileIDs are the ids of each provider.
var fileIDs = map[string]fileID{
	target.FilesSlack: {regexp.MustCompile(`^F[A-Z0-9]{6,20}$`), "F followed by capital letters and digits, from files[] of a message"},
}

// parseParams reads the route's parameters through the splitter the allowlist
// used, so the route acts on what was checked. A parameter given twice is
// refused rather than resolved: which one wins would be a guess.
func parseParams(rawPath string, id fileID) (params, error) {
	p := params{Limit: defaultLimit}
	seen := map[string]bool{}
	for _, pair := range target.QueryPairs(rawPath) {
		if seen[pair.Key] {
			return p, fmt.Errorf("%w: %q is given twice", ErrBadParams, pair.Key)
		}
		seen[pair.Key] = true
		if err := p.set(pair.Key, pair.Value, id); err != nil {
			return p, err
		}
	}
	if p.File == "" {
		return p, fmt.Errorf("%w: file is required — the id of a file, %s", ErrBadParams, id.hint)
	}
	return p, nil
}

func (p *params) set(key, value string, id fileID) error {
	var err error
	switch key {
	case "file":
		if !id.re.MatchString(value) {
			return fmt.Errorf("%w: file %q is not a file id — %s", ErrBadParams, value, id.hint)
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
	case "sheet":
		if value == "" || utf8.RuneCountInString(value) > maxSheet {
			return fmt.Errorf("%w: sheet must be 1..%d characters — a sheet's name or its number from 1", ErrBadParams, maxSheet)
		}
		p.Sheet = value
	case "entry":
		if value == "" || utf8.RuneCountInString(value) > maxEntry {
			return fmt.Errorf("%w: entry must be 1..%d characters — a name from the archive's table of contents", ErrBadParams, maxEntry)
		}
		p.Entry = value
	default:
		// The allowlist decides which names reach here; one this route does
		// not read is an upstream parameter that has no upstream to go to.
		return fmt.Errorf("%w: %q means nothing on this path — it takes file, offset, limit, grep, sheet and entry", ErrBadParams, key)
	}
	return err
}

// fits refuses a parameter the file has nothing to apply to: a sheet of a file
// that is not a workbook, a file inside one that is not an archive. Ignoring it
// would answer a question that was not asked.
func (p *params) fits(kind string, inArchive bool) error {
	switch {
	case p.Entry != "" && kind != kindArchive && !inArchive:
		return fmt.Errorf("%w: entry is for an archive, and this file is not one", ErrBadParams)
	case p.Sheet != "" && kind != kindSheet && (kind != kindArchive || p.Entry == ""):
		return fmt.Errorf("%w: sheet is for a workbook (xlsx), and this file is not one", ErrBadParams)
	}
	return nil
}

func bounded(key, value string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%w: %s must be an integer in %d..%d", ErrBadParams, key, lo, hi)
	}
	return n, nil
}
