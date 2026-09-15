package ring

import "slices"

// HasAny reports whether any element of want is in got. The catalogue asks it
// about a role's groups against the caller's, the OIDC verifier about the
// required roles against the token's; each used to carry its own copy.
func HasAny(got, want []string) bool {
	for _, w := range want {
		if slices.Contains(got, w) {
			return true
		}
	}
	return false
}

// Missing lists the elements of want that got does not have, in want's order.
// The database targets ask it in both directions — what the catalogue declared
// and the role cannot read, and what the role can read and the catalogue never
// declared — and the two answers have to be derived the same way, or a schema
// counts as drift in one report and as a missing grant in the other.
func Missing(want, got []string) []string {
	var out []string
	for _, w := range want {
		if !slices.Contains(got, w) {
			out = append(out, w)
		}
	}
	return out
}
