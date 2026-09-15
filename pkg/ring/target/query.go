package target

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// ForceQuery puts the profile's fixed query parameters on the path, dropping
// what the caller wrote for the same key: which contour a shared upstream
// answers for is the catalogue's decision. Everything else keeps its order and
// encoding, as in ExpandPath.
func (p *Profile) ForceQuery(rawPath string) string {
	if len(p.Query) == 0 {
		return rawPath
	}
	path, query, _ := strings.Cut(rawPath, "?")
	out := make([]string, 0, strings.Count(query, "&")+1+len(p.Query))
	if query != "" {
		for part := range strings.SplitSeq(query, "&") {
			key, _, _ := strings.Cut(part, "=")
			if !p.forcesKey(unescaped(key)) {
				out = append(out, part)
			}
		}
	}
	out = append(out, p.Query...)
	return path + "?" + strings.Join(out, "&")
}

// forcesKey reports whether the profile pins this query parameter. The key
// arrives decoded: the pin has to recognise what the upstream will read.
func (p *Profile) forcesKey(key string) bool {
	for _, q := range p.Query {
		if k, _, _ := strings.Cut(q, "="); unescaped(k) == key {
			return true
		}
	}
	return false
}

// unescaped is a query key or value as the upstream reads it: percent-escapes
// decoded, + as a space. What does not decode is kept as written — no rule
// matches it, and the upstream says what is wrong with it.
func unescaped(raw string) string {
	if s, err := url.QueryUnescape(raw); err == nil {
		return s
	}
	return raw
}

// deniedQueryParams are names no catalogue may let a call write, listed in
// AllowQueryParams or not: each is the query-string form of a credential or of
// impersonation. A call may not say who it is through a parameter any more than
// through a header. Exact names, because a suffix rule would catch next_token,
// which is how Nomad pages.
var deniedQueryParams = map[string]bool{
	"token":         true,
	"access_token":  true,
	"private_token": true,
	"job_token":     true,
	"auth_token":    true,
	"refresh_token": true,
	"id_token":      true,
	"api_key":       true,
	"apikey":        true,
	"api-key":       true,
	"x-api-key":     true,
	"client_secret": true,
	"secret":        true,
	"password":      true,
	"auth":          true,
	"authorization": true,
	"bearer":        true,
	"sudo":          true,
}

// queryDenied reports whether the name is one no call may write. Case, the
// array suffix and whitespace do not matter: a lenient server reads Token and
// token[] as token.
func queryDenied(name string) bool {
	name = strings.TrimSpace(strings.ToLower(name))
	return deniedQueryParams[strings.TrimSuffix(name, "[]")]
}

// CheckBody refuses a JSON body whose top-level keys carry a denied name: GitLab
// reads the body into the same params as the query string, so {"sudo":"root"}
// says who the caller is as much as ?sudo=root does. Only the top level, where a
// framework looks for its own parameters; a nested token is the upstream's data.
// Anything that is not a JSON object or an array of them is left to the upstream.
func CheckBody(body string) error {
	for _, obj := range bodyObjects(strings.TrimSpace(body)) {
		for key := range obj {
			if queryDenied(key) {
				return fmt.Errorf("body key %q is never accepted: it says who the caller is", key)
			}
		}
	}
	return nil
}

// bodyObjects is the top level of a JSON body as objects: one for an
// object, each element for a batch, none for anything else.
func bodyObjects(raw string) []map[string]json.RawMessage {
	if raw == "" {
		return nil
	}
	switch raw[0] {
	case '{':
		var one map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &one); err == nil {
			return []map[string]json.RawMessage{one}
		}
	case '[':
		var many []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &many); err == nil {
			return many
		}
	}
	return nil
}

// queryRule is one compiled AllowQueryParams entry: a name, and for
// "limit<=100" or "step>=15s" the bound on its value.
type queryRule struct {
	// spec is the entry as written: it is what the refusal quotes.
	spec string
	// op is "", "<=" or ">=".
	op string
	// bound is the number, or the seconds of a duration.
	bound float64
}

// parseQueryRule reads one AllowQueryParams entry into its name and rule.
// The name is decoded like a call's, so both sides of the lookup agree.
func parseQueryRule(raw string) (string, queryRule, error) {
	spec := strings.TrimSpace(raw)
	rule := queryRule{spec: spec}
	name, bound := spec, ""
	if i := strings.IndexAny(spec, "<>"); i >= 0 {
		if !strings.HasPrefix(spec[i+1:], "=") {
			return "", rule, fmt.Errorf("%q: a bound is written as name<=N or name>=N", raw)
		}
		name, bound, rule.op = strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i+2:]), spec[i:i+2]
	}
	name = unescaped(name)
	if name == "" || strings.ContainsAny(name, "&?#= \t") {
		return "", rule, fmt.Errorf("%q is not a parameter name", raw)
	}
	if rule.op == "" {
		return name, rule, nil
	}
	if n, err := strconv.ParseInt(bound, 10, 64); err == nil {
		if n < 0 {
			return "", rule, fmt.Errorf("%q: the bound must not be negative", raw)
		}
		rule.bound = float64(n)
		return name, rule, nil
	}
	d, ok := parseDuration(bound)
	if !ok {
		return "", rule, fmt.Errorf("%q: the bound must be an integer or a duration like 15s, 5m, 1h30m", raw)
	}
	rule.bound = d.Seconds()
	return name, rule, nil
}

// number reads a value the way a bound compares it: a number as it is, a
// duration in seconds — the way Prometheus reads step.
func number(value string) (float64, bool) {
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	d, ok := parseDuration(value)
	return d.Seconds(), ok
}

// allows reports whether n satisfies the bound. A bounded value is never
// negative: -1 is how YouTrack spells "no limit", the thing a <= bound prevents.
func (r queryRule) allows(n float64) bool {
	switch r.op {
	case "<=":
		return n >= 0 && n <= r.bound
	case ">=":
		return n >= r.bound
	}
	return true
}

// QueryGuarded reports whether the profile lists its query parameters.
func (p *Profile) QueryGuarded() bool { return len(p.queryRules) > 0 }

// errFragment is a path with a fragment: the HTTP client drops it along with
// everything ForceQuery appended after it, so the contour pin would be one #
// away from gone. Refused rather than cut, on every profile.
var errFragment = errors.New("path carries a fragment (#): nothing after it would reach the upstream")

// CheckQuery refuses a query parameter the profile does not take, a value
// outside its bound, or a name no profile accepts. It reads the path as the
// model wrote it, before the time expansion and the contour pin.
//
// Keys are compared decoded, the way the upstream reads them. Pairs are split on
// & and on ;, because Rack 2 splits on both and GitLab runs on it. A key pinned
// by Query is skipped. On a profile without AllowQueryParams the denied names
// and the fragment are the whole check.
func (p *Profile) CheckQuery(rawPath string) error {
	if strings.Contains(rawPath, "#") {
		return errFragment
	}
	_, query, _ := strings.Cut(rawPath, "?")
	for part := range strings.FieldsFuncSeq(query, func(r rune) bool { return r == '&' || r == ';' }) {
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if rawKey == "" {
			continue
		}
		key := unescaped(rawKey)
		if queryDenied(key) {
			return fmt.Errorf("query parameter %q is never accepted: it says who the caller is", key)
		}
		if !p.QueryGuarded() {
			continue
		}
		rule, ok := p.queryRules[key]
		if !ok {
			if p.forcesKey(key) {
				continue
			}
			return fmt.Errorf("query parameter %q is not allowed", key)
		}
		if rule.op == "" {
			continue
		}
		value := unescaped(rawValue)
		n, ok := number(value)
		if !ok {
			return fmt.Errorf("query parameter %s=%s: the bound %s needs a number or a duration like 15s", key, value, rule.spec)
		}
		if !rule.allows(n) {
			return fmt.Errorf("query parameter %s=%s is outside %s", key, value, rule.spec)
		}
	}
	return nil
}
