package target

import (
	"slices"

	"github.com/vmkteam/ringsrv/pkg/ring"
)

// Access is what one caller may do: the union of every role their IdP groups map
// onto. Empty Roles means no role matched — the caller gets a 403 with a
// readable reason, not an empty catalogue that looks like a broken server.
type Access struct {
	Roles   []string
	Tools   []string
	Targets []string
	// Databases are the database targets named by the roles, kept apart from
	// Targets: different tools call them, and the wildcard never reaches them.
	Databases  []string
	Repos      []string
	AllowWrite bool
	// MaxWrites is the most permissive limit among the matched roles; 0 means
	// unlimited.
	MaxWrites int
}

// Empty reports whether the caller matched no role at all.
func (a Access) Empty() bool { return len(a.Roles) == 0 }

// HasTool reports whether the tool is visible to the caller.
func (a Access) HasTool(name string) bool { return slices.Contains(a.Tools, name) }

// HasTarget reports whether the target is visible to the caller.
func (a Access) HasTarget(name string) bool { return slices.Contains(a.Targets, name) }

// HasDatabase reports whether the database target is visible to the caller.
func (a Access) HasDatabase(name string) bool { return slices.Contains(a.Databases, name) }

// Resolve maps IdP groups onto access. Grants are unioned across roles, but a
// write profile is granted only by a role that both lists it and sets
// AllowWrite: a viewer role naming youtrack-rw by mistake cannot borrow the
// write bit from an on-call role the same user happens to hold.
func (c *Catalog) Resolve(groups []string) Access {
	var a Access
	granted := map[string]bool{}
	databases := map[string]bool{}
	tools := map[string]bool{}
	repos := map[string]bool{}
	unlimitedWrites := false

	for _, name := range sortedKeys(c.Roles) {
		r := c.Roles[name]
		if !ring.HasAny(groups, r.Groups) {
			continue
		}
		a.Roles = append(a.Roles, name)
		if r.AllowWrite {
			a.AllowWrite = true
			// Roles are additive, so the most permissive cap is the effective
			// one, and a role with no cap at all lifts the limit entirely.
			switch {
			case unlimitedWrites:
			case r.MaxWrites == 0:
				unlimitedWrites = true
				a.MaxWrites = 0
			case r.MaxWrites > a.MaxWrites:
				a.MaxWrites = r.MaxWrites
			}
		}

		for _, t := range expand(r.Tools, KnownTools) {
			tools[t] = true
		}
		// The wildcard expands to the profiles alone: it came into the catalogue
		// for metrics and logs, and production data must not follow it at the
		// next edit. A database is granted by its name.
		grantProfile := func(t string) {
			if p, ok := c.Profiles[t]; ok && p.Write && !r.AllowWrite {
				return
			}
			granted[t] = true
		}
		for _, t := range r.Targets {
			switch {
			case t == Wildcard:
				for _, name := range sortedKeys(c.Profiles) {
					grantProfile(name)
				}
			case c.Databases[t] != nil:
				databases[t] = true
			default:
				grantProfile(t)
			}
		}
		for _, rp := range expand(r.Repos, sortedKeys(c.Repos)) {
			repos[rp] = true
		}
	}

	a.Tools = sortedKeys(tools)
	a.Targets = sortedKeys(granted)
	a.Databases = sortedKeys(databases)
	a.Repos = sortedKeys(repos)
	return a
}

// expand replaces the wildcard with everything the catalogue knows.
func expand(list, all []string) []string {
	if slices.Contains(list, Wildcard) {
		return all
	}
	return list
}
