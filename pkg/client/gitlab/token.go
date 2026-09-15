package gitlab

import (
	"errors"
	"fmt"
	"strings"
)

// Header names GitLab reads a personal access token from on its REST API.
const (
	HeaderPrivateToken  = "PRIVATE-TOKEN"
	HeaderAuthorization = "Authorization"
	bearerPrefix        = "Bearer "
)

// TokenFromHeaders pulls the personal access token out of a profile's own
// headers so that the same credential can serve git over HTTP. GitLab
// spells one token three ways: PRIVATE-TOKEN and "Authorization: Bearer" on
// the API, HTTP Basic "oauth2:<token>" on a clone. The catalogue writes one
// of the first two; the git client builds the third from what is returned
// here.
//
// Exactly one token header is expected. None means the profile is not a
// token profile at all, and two would leave "which one does git get" to the
// order of a list — a question the validator should refuse rather than
// answer.
func TokenFromHeaders(headers []string) (string, error) {
	var found []string
	for _, h := range headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch {
		case strings.EqualFold(name, HeaderPrivateToken):
			found = append(found, value)
		case strings.EqualFold(name, HeaderAuthorization) && hasPrefixFold(value, bearerPrefix):
			found = append(found, strings.TrimSpace(value[len(bearerPrefix):]))
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no %s or %s: Bearer header — nothing to clone with", HeaderPrivateToken, HeaderAuthorization)
	case 1:
		if found[0] == "" {
			return "", errors.New("token header has an empty value")
		}
		return found[0], nil
	default:
		return "", fmt.Errorf("%d token headers, want exactly one", len(found))
	}
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}
