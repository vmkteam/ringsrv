package upstream

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// buildURL glues the target's base and the caller's path.
//
// The query is taken as the caller wrote it, because the query is the point:
// PromQL and LogQL live there, and re-encoding through url.Values would reorder
// keys and escape braces the upstream expects raw. The path is not taken on
// trust — CleanPath decides what may become a request, and the parse below is
// the last word on where that request goes.
func buildURL(baseURL, rawPath string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("upstream: base url %q: %w", baseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("upstream: base url %q has no scheme or host", baseURL)
	}

	path, ok := CleanPath(rawPath)
	if !ok {
		return "", fmt.Errorf("upstream: bad path %q", rawPath)
	}
	_, query, _ := strings.Cut(rawPath, "?")
	// The one byte that cannot travel as written: a space ends the request
	// line, so the upstream reads `query=up offset 5m` as a malformed request
	// — or a proxy in between reads it as a shorter one, without whatever
	// the profile appended. Percent-encoded it means the same thing to
	// every server. Control characters are refused by the parse below.
	query = strings.ReplaceAll(query, " ", "%20")

	// Built by hand rather than through URL.String(): the query must survive
	// byte for byte, and JoinPath would normalise away things the upstream may
	// legitimately expect.
	out := strings.TrimRight(base.Scheme+"://"+base.Host, "/") + path
	if query != "" {
		out += "?" + query
	}

	// Parse back as the last word on where this request goes: if the result is
	// not the host we started from, something in the path escaped the checks.
	final, err := url.Parse(out)
	if err != nil {
		return "", fmt.Errorf("upstream: built url %q: %w", out, err)
	}
	if final.Host != base.Host || final.Scheme != base.Scheme {
		return "", fmt.Errorf("upstream: path %q escapes the target host", rawPath)
	}
	return out, nil
}

// downloadURL checks an absolute URL an answer named before anything is sent to
// it: the target's own scheme and host, or https on one of hosts exactly. No
// user info — "files.example.com@evil.example" is a host named evil.example —
// and no port on a listed host, which would be another server.
//
// The path is checked by its segments rather than by CleanPath: it ends in the
// file's name, and "report..csv" is a name, where ".." alone is a step up.
func downloadURL(baseURL, rawURL string, hosts []string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return "", fmt.Errorf("upstream: base url %q is not usable", baseURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("upstream: download url: %w", err)
	}
	switch {
	case u.User != nil:
		return "", fmt.Errorf("upstream: download url %q carries user info", rawURL)
	case u.Host == base.Host && u.Scheme == base.Scheme:
	case u.Scheme == "https" && u.Port() == "" && slices.Contains(hosts, u.Hostname()):
	default:
		return "", fmt.Errorf("upstream: download url %q is not on the target's host or an allowed one", rawURL)
	}
	if !filePath(u.Path) {
		return "", fmt.Errorf("upstream: download url %q has a bad path", rawURL)
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

// filePath accepts a decoded path of a file URL: absolute, every segment a
// name — not empty, not "." or ".." — and no control character or backslash
// anywhere.
func filePath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return false
	}
	for seg := range strings.SplitSeq(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
