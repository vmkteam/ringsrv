// Package target loads and validates targets.toml — the catalogue of upstream
// profiles, RBAC roles and repositories. Everything it can get wrong is checked
// at load time and refused loudly. Credentials arrive already substituted by the
// Nomad template, so this package never resolves anything.
package target

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"

	"github.com/BurntSushi/toml"
)

// Contours a catalogue can describe. The contour belongs to the catalogue, never
// to a call.
const (
	EnvDev  = "dev"
	EnvProd = "prod"
)

// Profile is one upstream API the api_call tool can reach.
type Profile struct {
	// Description is shown to the model and starts with the environment
	// ("prod · Grafana").
	Description  string
	BaseURL      string
	Headers      []string
	AllowMethods []string
	AllowPaths   []string
	DefaultJQ    string
	MaxBytes     int

	// TimeParams lists the query parameters where now-1h style values are
	// expanded, TimeFormat says into what.
	TimeParams []string
	TimeFormat string
	// TimeBodyParams does the same inside a JSON body, by dotted path
	// ("params.start"). An entry may name its own format after a colon —
	// "params.filter.from:rfc3339" — for an upstream whose windows disagree.
	TimeBodyParams []string

	// AllowRPCMethods names the JSON-RPC methods a body may carry, for an API
	// that lives behind one path where AllowPaths cannot tell alert.silence from
	// alert.list. Matching is case-insensitive.
	AllowRPCMethods []string

	// AllowQueryParams names the query parameters a call may set, each with an
	// optional bound: "query", "limit<=100", "step>=15s". Empty means any —
	// GitLab and Sentry take hundreds nobody can enumerate. A value is read as a
	// number or as a duration in seconds.
	AllowQueryParams []string

	// Query lists "key=value" pairs the server puts on every request, over
	// whatever the model wrote. It pins a shared upstream to this contour.
	Query []string

	// Redact turns PII redaction on for this target; off by default.
	Redact      string
	RedactRules []string

	// SkipDefaultHeaders drops the catalogue-wide headers for a target reachable
	// without the contour's forward-auth.
	SkipDefaultHeaders bool

	// AllowHeaders names the request headers a call may set. Empty means none: a
	// header the model can write is a header a prompt injection can write. A name
	// here may not be one the profile already sends, since the profile wins.
	AllowHeaders []string

	// Write marks a profile that can change upstream state. Required for any
	// method other than GET; roles without AllowWrite never see it.
	Write bool
	// ReadOnlyPost is the escape hatch for APIs that read via POST. Exactly one
	// of Write/ReadOnlyPost must be set when AllowMethods goes beyond GET.
	ReadOnlyPost bool

	name string
	// Derived from the public fields by validate(), so per-call lookups repeat
	// no work whose answer cannot change.
	headers        []string // Headers merged with the catalogue defaults
	allow          []allowRule
	allowHeaders   map[string]bool // AllowHeaders by normalised name
	rpcMethods     map[string]bool // AllowRPCMethods lowercased
	bodyTimeParams []bodyTimeParam
	queryRules     map[string]queryRule
}

// allowRule is one compiled AllowPaths entry. "POST ^/api/ds/query$" binds to
// that method alone; a bare pattern applies to every allowed method, except on a
// ReadOnlyPost profile, where it is GET-only.
type allowRule struct {
	method string
	re     *regexp.Regexp
}

// covers reports whether the rule applies to a method on this profile.
func (r allowRule) covers(method string, readOnlyPost bool) bool {
	switch {
	case r.method != "":
		return strings.EqualFold(r.method, method)
	case readOnlyPost:
		return strings.EqualFold(method, http.MethodGet)
	default:
		return true
	}
}

// methodPrefixRe is the shape of a method prefix in AllowPaths.
var methodPrefixRe = regexp.MustCompile(`^[A-Z]+$`)

// splitPattern separates an optional method prefix from the expression:
// "POST ^/x$" → ("POST", "^/x$"), "^/x$" → ("", "^/x$").
func splitPattern(pat string) (method, expr string) {
	m, rest, ok := strings.Cut(pat, " ")
	if ok && methodPrefixRe.MatchString(m) && strings.HasPrefix(rest, "^") {
		return m, rest
	}
	return "", pat
}

// Name returns the catalogue key of the profile.
func (p *Profile) Name() string { return p.name }

// HeaderAllowed reports whether a call may set this header on this profile.
func (p *Profile) HeaderAllowed(name string) bool { return p.allowHeaders[headerKey(name)] }

// AllowsAnyHeader reports whether this profile lets a call set any header at
// all. tools/list asks it, so a role whose targets all say no pays no schema
// bytes for the argument.
func (p *Profile) AllowsAnyHeader() bool { return len(p.allowHeaders) > 0 }

// Upstream converts the profile into what the HTTP client needs.
func (p *Profile) Upstream() upstream.Target {
	return upstream.Target{
		Name:    p.name,
		BaseURL: p.BaseURL,
		Headers: p.headers,
	}
}

// mergeHeaders puts the catalogue defaults under the profile's own; a profile
// that names the same header wins.
func mergeHeaders(defaults, own []string, skip bool) []string {
	if skip || len(defaults) == 0 {
		return own
	}

	taken := headerNames(own)

	out := make([]string, 0, len(defaults)+len(own))
	for _, h := range defaults {
		name, _, ok := strings.Cut(h, ":")
		if ok && taken[headerKey(name)] {
			continue
		}
		out = append(out, h)
	}
	return append(out, own...)
}

// headerKey normalises a header name: "x-authentik-token" must not slip past
// "X-Authentik-Token".
func headerKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// Allows reports whether method+path passes the profile's allowlist.
//
// The path is matched without its query string (CheckQuery reads that
// separately) and with percent-escapes intact, because GitLab reads a file path
// as one segment only when its slashes are encoded. CleanPath checks the decoded
// form.
func (p *Profile) Allows(method, path string) bool {
	if !p.AllowsMethod(method) {
		return false
	}
	clean, ok := upstream.CleanPath(path)
	if !ok {
		return false
	}
	for _, r := range p.allow {
		if r.covers(method, p.ReadOnlyPost) && r.re.MatchString(clean) {
			return true
		}
	}
	return false
}

// AllowsMethod reports whether the method is in the profile's allowlist.
// Separate from Allows because the two refusals name different lists.
func (p *Profile) AllowsMethod(method string) bool {
	return containsFold(p.AllowMethods, method)
}

// AllowsPath reports whether some pattern matches the path, whatever method it
// is bound to — the question a cheat sheet asks, not the one a call asks.
func (p *Profile) AllowsPath(path string) bool {
	clean, ok := upstream.CleanPath(path)
	if !ok {
		return false
	}
	for _, r := range p.allow {
		if r.re.MatchString(clean) {
			return true
		}
	}
	return false
}

// Drivers a database target can name.
const (
	DriverPostgres   = "postgres"
	DriverClickHouse = "clickhouse"
)

// Database is one read-only database the db_query and db_introspect tools can
// reach. A section of its own rather than a Kind on Profile: not one rule of an
// HTTP profile applies to it. The name shares a namespace with the profiles.
//
// There is no write flag and never will be: the user is read-only on the DBMS
// side, and the server proves it before the first query.
type Database struct {
	// Description is what the model reads: the contour, the dialect in one
	// example, and where the enums live in the code.
	Description string
	Driver      string
	// Addr is host:port of the native protocol, no scheme and no TLS.
	Addr     string
	Database string
	// User is this target's read-only user; the caller is attributed by the
	// audit record, not by the DBMS.
	User string
	// Password arrives substituted by the Nomad template; empty is an error,
	// because "authentication failed" reads like a network fault.
	Password string
	// MaxRows caps one answer; 0 falls back to Limits.DBMaxRows.
	MaxRows int
	// Timeout bounds the query; 0 falls back to Limits.DBTimeout.
	Timeout time.Duration
	// MaxConcurrent is the pool size; 0 means dbq.DefaultPoolSize.
	MaxConcurrent int
	// MaxBytes caps the answer after jq; 0 falls back to Limits.MaxBytes.
	MaxBytes int
	// Redact is the redaction mode. For postgres it has to be named, off
	// included: a raw production database holds PII.
	Redact      string
	RedactRules []string
	// Schemas, postgres only, are what db_introspect shows and what the
	// search_path is set to. Empty means public, which Validate writes down.
	Schemas []string
	// Repo names the repository whose db layer describes these tables; a base
	// several services write takes a list.
	Repo RepoList
	// SchemaRepos, postgres only, overrides Repo per schema. A schema absent
	// from the map falls back to Repo.
	SchemaRepos map[string][]string

	// Repo and SchemaRepos resolved to repository and layer by Validate.
	codeRefs    map[string][]CodeRef
	defaultRefs []CodeRef
}

// RepoList is one or more repository names in a field that reads as one:
// Repo = "apisrv" or Repo = ["statsrv", "asksrv"].
type RepoList []string

// UnmarshalTOML accepts both forms.
func (l *RepoList) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case string:
		*l = RepoList{val}
	case []any:
		out := make(RepoList, len(val))
		for i, item := range val {
			name, ok := item.(string)
			if !ok {
				return fmt.Errorf("field Repo: element %d is %T, want a repository name", i+1, item)
			}
			out[i] = name
		}
		*l = out
	default:
		return fmt.Errorf("field Repo is %T, want a repository name or a list of them", v)
	}
	return nil
}

// CodeRef is one pointer from a table to the code: the repository and the db
// layer where the models and the enums live.
type CodeRef struct {
	Repo  string
	Layer string
}

// CodeRefs is where the models of a table in this schema live; an empty schema
// reads the base's own Repo.
func (d *Database) CodeRefs(schema string) []CodeRef {
	if refs, ok := d.codeRefs[schema]; ok {
		return refs
	}
	return d.defaultRefs
}

// Role binds IdP groups to a set of tools and targets.
type Role struct {
	Description string
	Groups      []string
	Tools       []string
	Targets     []string
	Repos       []string
	AllowWrite  bool
	// MaxWrites caps write calls per session; zero means unlimited.
	MaxWrites int
}

// Repo is one repository ringsrv knows about: where to clone it from, how it
// maps onto the other targets, and a hand-written architecture note.
type Repo struct {
	GitLabProject int
	Description   string
	CloneURL      string
	// CloneTarget names the profile whose token the clone uses, so one GitLab
	// credential lives in one place. A credential source only, never a role check.
	CloneTarget string
	// CloneToken is the alternative for a repository whose host has no profile.
	// Exactly one of the two may be set.
	CloneToken    string
	DefaultBranch string
	Refspec       []string
	Tags          bool
	SentrySlug    string
	// IssueTarget names the profile to ask about a TASK_ID. A profile name
	// rather than a URL, so a user without that target simply gets no enrichment.
	IssueTarget string
	// IssuePath is the request that fetches one issue, {id} standing for the
	// key. Empty means YouTrack.
	IssuePath    string
	NomadJob     string
	PromJob      string
	Layers       map[string]string
	Exclude      []string
	TaskIDRegexp string

	taskRe *regexp.Regexp // TaskIDRegexp compiled by validate()
	// cloneToken is what git gets: CloneToken as written, or the token resolved
	// from CloneTarget's headers.
	cloneToken string
}

// DefaultIssuePath is YouTrack. Fields are listed explicitly because YouTrack
// without them answers with an almost empty object.
const DefaultIssuePath = "/api/issues/{id}?fields=summary,description,comments(text)"

// IssueRequest is the path that fetches one issue by key.
func (r *Repo) IssueRequest(id string) string {
	path := r.IssuePath
	if path == "" {
		path = DefaultIssuePath
	}
	return strings.ReplaceAll(path, "{id}", id)
}

// TaskID pulls the issue key out of a commit subject. No regexp, or a commit
// that does not follow it, means no task — an answer, not a gap to guess at.
func (r *Repo) TaskID(subject string) string {
	if r.taskRe == nil {
		return ""
	}
	m := r.taskRe.FindStringSubmatch(subject)
	switch {
	case m == nil:
		return ""
	case len(m) > 1:
		return m[1] // the catalogue's regexps capture the key in a group
	default:
		return m[0]
	}
}

// Spec converts the catalogue entry into what the git client needs.
func (r *Repo) Spec(name string) git.Spec {
	return git.Spec{
		Name:          name,
		CloneURL:      r.CloneURL,
		CloneToken:    r.cloneToken,
		DefaultBranch: r.DefaultBranch,
		Refspec:       r.Refspec,
		Tags:          r.Tags,
	}
}

// Excluded reports whether a path is one this repository never hands out:
// vendored code, and keys that must not leave the disk.
func (r *Repo) Excluded(path string) bool {
	for _, pattern := range r.Exclude {
		if matchExclude(pattern, path) {
			return true
		}
	}
	return false
}

// Layer names the architectural layer a path belongs to. A path outside every
// known layer gets an empty answer rather than an invented one.
func (r *Repo) Layer(path string) string {
	best, bestLen := "", 0
	for layer, prefix := range r.Layers {
		// Longest prefix wins: "internal/db" must beat "internal".
		if len(prefix) > bestLen && strings.HasPrefix(path, prefix) {
			best, bestLen = layer, len(prefix)
		}
	}
	return best
}

// matchExclude understands the two shapes the catalogue uses: a directory
// prefix ("vendor/") and a glob on the file name ("*.pem", ".env*").
func matchExclude(pattern, path string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(path, pattern) || strings.Contains(path, "/"+pattern)
	}
	if ok, err := filepath.Match(pattern, filepath.Base(path)); err == nil && ok {
		return true
	}
	ok, err := filepath.Match(pattern, path)
	return err == nil && ok
}

// Catalog is the parsed targets.toml.
type Catalog struct {
	// Env is the contour these targets belong to, "dev" or "prod". It prefixes
	// the tool description and travels in every answer.
	Env string
	// WebhookSecret authenticates GitLab push events; empty turns the endpoint
	// off entirely.
	WebhookSecret string
	// Defaults are the settings every profile inherits — today the forward-auth
	// header, which a target reached without it answers with a login page.
	Defaults  Defaults
	Profiles  map[string]*Profile
	Databases map[string]*Database
	Roles     map[string]*Role
	Repos     map[string]*Repo
}

// Defaults holds what every profile inherits.
type Defaults struct {
	// Headers are added to every request. A profile that declares a header of
	// the same name wins; SkipDefaultHeaders drops them entirely.
	Headers []string
}

// Load reads and validates a catalogue file.
func Load(path string) (*Catalog, error) {
	if path == "" {
		return nil, errors.New("catalog: empty path")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s: %w", path, err)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates a catalogue from bytes. Unknown keys are an
// error: a mistyped `AllowPath` silently drops the allowlist, and the profile
// then rejects everything.
func Parse(b []byte) (*Catalog, error) {
	var c Catalog
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return nil, err
	}
	if u := md.Undecoded(); len(u) > 0 {
		keys := make([]string, len(u))
		for i, k := range u {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	for name, p := range c.Profiles {
		p.name = name
		p.headers = mergeHeaders(c.Defaults.Headers, p.Headers, p.SkipDefaultHeaders)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// RepoByProject finds the repository a GitLab project id belongs to.
func (c *Catalog) RepoByProject(id int) (string, bool) {
	if id == 0 {
		return "", false
	}
	for name, repo := range c.Repos {
		if repo.GitLabProject == id {
			return name, true
		}
	}
	return "", false
}

// Profile returns a profile by name.
func (c *Catalog) Profile(name string) (*Profile, bool) {
	p, ok := c.Profiles[name]
	return p, ok
}

// Database returns a database target by name.
func (c *Catalog) Database(name string) (*Database, bool) {
	d, ok := c.Databases[name]
	return d, ok
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
