package target

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/gitlab"
	"github.com/vmkteam/ringsrv/pkg/ring"

	"github.com/vmkteam/mcpkit/redact"
)

// KnownTools is the closed set of tool names a role may reference. A typo in a
// role must fail the catalogue rather than silently narrow somebody's access.
var KnownTools = []string{
	"api_call",
	"blast_radius",
	"code_read",
	"code_search",
	"code_history",
	"code_refs",
	"db_introspect",
	"db_query",
	"repo_map",
	"why",
}

// repoNameRe is what a repository may be called: the name is used as a
// directory for its mirror and its worktrees.
var repoNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// identRe is a bare SQL identifier: what a schema or a table name may be,
// because a schema goes into search_path as written.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsIdent reports whether s is a bare SQL identifier.
func IsIdent(s string) bool { return identRe.MatchString(s) }

// DefaultSchema is what a postgres entry without Schemas reads. Exported
// because the domain normalises a hand-built target the same way (dbq.Add).
const DefaultSchema = "public"

// Wildcard grants every tool, target or repo in the catalogue.
const Wildcard = "*"

const schemeHTTPS = "https"

// MaxDescriptionsBytes caps the total size of profile descriptions: they are
// inlined into the api_call tool description on every request, and the whole
// tool surface has a ~2k token budget.
const MaxDescriptionsBytes = 4096

// forbiddenCalls are requests no combination of flags may ever permit. Merge,
// delete and deploy are not expressible as a "write profile" — they are simply
// absent from every allowlist, and this check proves it after each edit.
//
// The Grafana rows came from a review: a profile with POST and a prefix pattern
// let a viewer create annotations and reach any datasource through Grafana's
// proxy, around the allowlist of that datasource's own profile. The last row is
// a percent-encoded "..": it has to be refused before any pattern is consulted.
var forbiddenCalls = []struct {
	method string
	path   string
}{
	{http.MethodPut, "/api/v4/projects/42/merge_requests/7/merge"},
	{http.MethodPost, "/api/v4/projects/42/merge_requests/7/merge"},
	{http.MethodDelete, "/api/v4/projects/42"},
	{http.MethodPost, "/v1/job/apisrv/dispatch"},
	{http.MethodDelete, "/v1/job/apisrv"},
	{http.MethodPost, "/api/annotations"},
	{http.MethodPost, "/api/datasources"},
	{http.MethodPost, "/api/dashboards/db"},
	{http.MethodDelete, "/api/annotations/1"},
	{http.MethodGet, "/api/datasources/proxy/uid/abc/api/v1/query"},
	{http.MethodPost, "/api/datasources/proxy/uid/abc/api/v1/admin/tsdb/delete_series"},
	{http.MethodPost, "/api/datasources/uid/abc/resources/api/v1/admin/tsdb/delete_series"},
	{http.MethodGet, "/api/v4/projects/42/pipelines/%2e%2e/%2e%2e/%2e%2e/users"},
}

// Validate checks the whole catalogue. Every finding is an error: the file is
// validated before the port opens, and a half-valid catalogue is worse than a
// refused start.
func (c *Catalog) Validate() error {
	var errs []error

	switch c.Env {
	case EnvDev, EnvProd:
	default:
		errs = append(errs, fmt.Errorf("field Env must be %s or %s, got %q", EnvDev, EnvProd, c.Env))
	}
	if len(c.Profiles) == 0 {
		errs = append(errs, errors.New("no [Profiles.*] defined"))
	}
	for _, h := range c.Defaults.Headers {
		if err := validateHeader(h); err != nil {
			errs = append(errs, fmt.Errorf("[Defaults]: %w", err))
		}
	}
	for _, name := range sortedKeys(c.Profiles) {
		if err := c.Profiles[name].validate(c.Env); err != nil {
			errs = append(errs, fmt.Errorf("[Profiles.%s]: %w", name, err))
		}
	}
	if err := c.validateForbidden(); err != nil {
		errs = append(errs, err)
	}
	for _, name := range sortedKeys(c.Databases) {
		if err := c.validateDatabase(name, c.Databases[name]); err != nil {
			errs = append(errs, fmt.Errorf("[Databases.%s]: %w", name, err))
		}
	}
	for _, name := range sortedKeys(c.Roles) {
		if err := c.validateRole(c.Roles[name]); err != nil {
			errs = append(errs, fmt.Errorf("[Roles.%s]: %w", name, err))
		}
	}
	for _, name := range sortedKeys(c.Repos) {
		// The name becomes a directory under Storage.ReposDir, so a slash here
		// would put a clone somewhere nobody looks for it.
		if !repoNameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("[Repos.%s]: name must match %s — it becomes a directory", name, repoNameRe))
		}
		if err := c.Repos[name].validate(); err != nil {
			errs = append(errs, fmt.Errorf("[Repos.%s]: %w", name, err))
		}
		if err := c.resolveCloneTarget(c.Repos[name]); err != nil {
			errs = append(errs, fmt.Errorf("[Repos.%s]: %w", name, err))
		}
		// A typo here would silently cost every `why` its enrichment, and
		// nothing else would ever complain about it.
		if t := c.Repos[name].IssueTarget; t != "" {
			p, ok := c.Profiles[t]
			if !ok {
				errs = append(errs, fmt.Errorf("[Repos.%s]: IssueTarget %q is not a profile", name, t))
				continue
			}
			// The request has to pass that profile's own allowlist, or every
			// `why` would answer "tracker unavailable". Checked with a key
			// shaped like the ones this repository produces.
			if req := c.Repos[name].IssueRequest("ABC-123"); !p.Allows(http.MethodGet, req) || p.CheckQuery(req) != nil {
				errs = append(errs, fmt.Errorf("[Repos.%s]: IssuePath %q is not allowed by profile %q (tried %s)",
					name, c.Repos[name].IssueRequest("{id}"), t, req))
			}
		}
	}
	if n := c.descriptionsSize(); n > MaxDescriptionsBytes {
		errs = append(errs, fmt.Errorf("profile and database descriptions are %d bytes, budget is %d: they are inlined into every tools/list", n, MaxDescriptionsBytes))
	}

	return errors.Join(errs...)
}

// validateDatabase checks one database entry. Every rule here is a mistake that
// would not fail on its own: it would fail the first query, hours later, as an
// error that reads like a network fault.
func (c *Catalog) validateDatabase(name string, d *Database) error {
	var errs []error

	// Roles.*.Targets lists profiles and databases in one list, and the model
	// sees one list; a name that is both would be granted as one and called as
	// the other.
	if _, clash := c.Profiles[name]; clash {
		errs = append(errs, errors.New("name is also a profile — targets share one namespace"))
	}
	switch {
	case d.Description == "":
		errs = append(errs, errors.New("field Description is required — it is what the model reads"))
	case !strings.HasPrefix(d.Description, c.Env+" "):
		errs = append(errs, fmt.Errorf("field Description must start with %q — the contour comes first", c.Env))
	}
	switch d.Driver {
	case DriverPostgres, DriverClickHouse:
	default:
		errs = append(errs, fmt.Errorf("field Driver %q: want %s or %s", d.Driver, DriverPostgres, DriverClickHouse))
	}
	if err := validateAddr(d.Addr); err != nil {
		errs = append(errs, err)
	}
	for _, f := range []struct{ name, value string }{{"Database", d.Database}, {"User", d.User}, {"Password", d.Password}} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("field %s is required", f.name))
		}
	}
	for _, f := range []struct {
		name  string
		value int
	}{{"MaxRows", d.MaxRows}, {"MaxConcurrent", d.MaxConcurrent}, {"MaxBytes", d.MaxBytes}} {
		if f.value < 0 {
			errs = append(errs, fmt.Errorf("%s %d must be >= 0", f.name, f.value))
		}
	}
	// TOML decodes a bare integer into nanoseconds, and a 15 ns timeout refuses
	// every query on the spot; a real one is written "15s".
	switch {
	case d.Timeout < 0:
		errs = append(errs, fmt.Errorf("field Timeout %s must be >= 0", d.Timeout))
	case d.Timeout > 0 && d.Timeout < time.Second:
		errs = append(errs, fmt.Errorf("field Timeout %s is under a second — write it as a duration string, like \"15s\"", d.Timeout))
	}
	// A raw production database holds PII; the choice has to be written down,
	// off included, the way Write and ReadOnlyPost are on a profile.
	if d.Driver == DriverPostgres && d.Redact == "" {
		errs = append(errs, fmt.Errorf("field Redact is required for %s — name the choice, %s included", DriverPostgres, redact.ModeOff))
	}
	if err := validateRedact(&d.Redact, d.RedactRules); err != nil {
		errs = append(errs, err)
	}
	if len(d.Schemas) > 0 && d.Driver != DriverPostgres {
		errs = append(errs, fmt.Errorf("field Schemas is for %s only", DriverPostgres))
	}
	for _, s := range d.Schemas {
		if !IsIdent(s) {
			errs = append(errs, fmt.Errorf("field Schemas: %q is not an identifier — it goes into search_path as written", s))
		}
	}
	if d.Driver == DriverPostgres && len(d.Schemas) == 0 {
		d.Schemas = []string{DefaultSchema}
	}
	// An empty list is written, not omitted: it reads as "the owner is
	// recorded" and hands out no pointer at all.
	if d.Repo != nil && len(d.Repo) == 0 {
		errs = append(errs, errors.New("field Repo names no repository — drop the line or name one"))
	}
	if len(d.Repo) > 0 {
		refs, err := c.codeRefs("field Repo", d.Repo)
		if err != nil {
			errs = append(errs, err)
		} else {
			d.defaultRefs = refs
		}
	}
	if err := c.validateSchemaRepos(d); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// validateSchemaRepos resolves the per-schema pointers to the code. A schema
// nobody introspects and a repository nobody clones are both a line that looks
// like it works.
func (c *Catalog) validateSchemaRepos(d *Database) error {
	if len(d.SchemaRepos) == 0 {
		return nil
	}
	if d.Driver != DriverPostgres {
		return fmt.Errorf("field SchemaRepos is for %s only", DriverPostgres)
	}
	var errs []error
	// The map is walked in order, so two bad lines read the same way twice.
	for _, schema := range sortedKeys(d.SchemaRepos) {
		field := fmt.Sprintf("field SchemaRepos.%s", schema)
		repos := d.SchemaRepos[schema]
		switch {
		case !slices.Contains(d.Schemas, schema):
			errs = append(errs, fmt.Errorf("%s: not in Schemas — db_introspect never shows this schema", field))
			continue
		case len(repos) == 0:
			errs = append(errs, fmt.Errorf("%s: names no repository — drop the line or name one", field))
			continue
		}
		refs, err := c.codeRefs(field, repos)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if d.codeRefs == nil {
			d.codeRefs = make(map[string][]CodeRef, len(d.SchemaRepos))
		}
		d.codeRefs[schema] = refs
	}
	return errors.Join(errs...)
}

// codeRefs turns repository names into the pointer db_introspect hands out. The
// pointer has to point somewhere: an unknown repository, or one with no layer
// holding table models, would send the model to a directory that is not there.
func (c *Catalog) codeRefs(field string, repos []string) ([]CodeRef, error) {
	var (
		errs []error
		out  []CodeRef
	)
	for _, name := range repos {
		r, ok := c.Repos[name]
		layer := modelLayer(r)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s %q is not a repository", field, name))
		case layer == "":
			errs = append(errs, fmt.Errorf("%s %q has no Layers.db or Layers.domain — nothing to point db_introspect at", field, name))
		case slices.ContainsFunc(out, func(ref CodeRef) bool { return ref.Repo == name }):
			errs = append(errs, fmt.Errorf("%s names %q twice", field, name))
		default:
			out = append(out, CodeRef{Repo: name, Layer: layer})
		}
	}
	return out, errors.Join(errs...)
}

// modelLayer is where a repository's table models and enums live: the db layer,
// and without it the domain one — a service that never grew a separate db
// package keeps the row types in its domain.
func modelLayer(r *Repo) string {
	if r == nil {
		return ""
	}
	if l := r.Layers["db"]; l != "" {
		return l
	}
	return r.Layers["domain"]
}

// validateAddr accepts host:port and nothing else: a scheme, a path or a
// credential in the string is a BaseURL habit, and the native protocol does not
// speak it.
func validateAddr(addr string) error {
	if addr == "" {
		return errors.New("field Addr is required")
	}
	if strings.ContainsAny(addr, "/@?#") {
		return fmt.Errorf("field Addr %q must be host:port — no scheme, path or credentials", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("field Addr %q must be host:port: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("field Addr %q has no host", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("field Addr %q: port must be 1..65535", addr)
	}
	return nil
}

func (p *Profile) validate(env string) error {
	var errs []error

	if p.Description == "" {
		errs = append(errs, errors.New("field Description is required — it is what the model reads"))
	}
	// The contour has to be the first thing read about a target, and it has to
	// agree with the catalogue it lives in: a prod host described as dev is how
	// an answer about the wrong contour gets believed.
	if p.Description != "" && !strings.HasPrefix(p.Description, env+" ") {
		errs = append(errs, fmt.Errorf("field Description must start with %q — the contour comes first", env))
	}
	if err := validateBaseURL(p.BaseURL); err != nil {
		errs = append(errs, err)
	}
	for _, h := range p.Headers {
		if err := validateHeader(h); err != nil {
			errs = append(errs, err)
		}
	}

	errs = append(errs, p.validateAllowHeaders()...)

	errs = append(errs, p.validateMethods()...)

	if len(p.AllowPaths) == 0 {
		errs = append(errs, errors.New("AllowPaths is empty — everything would be denied"))
	}
	rules, postBound, pathErrs := compileAllowPaths(p.AllowPaths, p.AllowMethods)
	p.allow = rules
	errs = append(errs, pathErrs...)
	// On a ReadOnlyPost profile a bare pattern is GET-only, so the POST that
	// justifies the flag has to be named — otherwise the flag allows nothing.
	if p.ReadOnlyPost && !postBound {
		errs = append(errs, errors.New("ReadOnlyPost is set but no pattern is prefixed with POST — bare patterns are GET-only on such a profile"))
	}

	if err := validateRedact(&p.Redact, p.RedactRules); err != nil {
		errs = append(errs, err)
	}
	if err := validateTimeFormat(p.TimeFormat, p.TimeParams, p.TimeBodyParams); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, p.validateTimeBodyParams()...)
	errs = append(errs, p.validateRPCMethods()...)
	for _, q := range p.Query {
		if err := validateQueryPair(q); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, p.validateQueryParams()...)
	if p.MaxBytes < 0 {
		errs = append(errs, fmt.Errorf("MaxBytes %d must be >= 0", p.MaxBytes))
	}
	// Every call without a filter of its own trusts DefaultJQ, so a broken one
	// fails them all — better here, once, than as JQFailed on each.
	if err := ring.CompileJQ(p.DefaultJQ); err != nil {
		errs = append(errs, fmt.Errorf("DefaultJQ: %w", err))
	}

	return errors.Join(errs...)
}

// validateMethods checks AllowMethods against the two flags that say what a
// non-GET method means on this profile.
func (p *Profile) validateMethods() []error {
	var errs []error
	if len(p.AllowMethods) == 0 {
		errs = append(errs, errors.New("AllowMethods is empty — the profile can do nothing"))
	}
	for _, m := range p.AllowMethods {
		if m != strings.ToUpper(m) {
			errs = append(errs, fmt.Errorf("AllowMethods: %q must be upper case", m))
		}
	}
	// Write must never appear by accident: anything beyond GET has to be
	// declared as either a write profile or an API that reads over POST.
	if hasNonGET(p.AllowMethods) {
		switch {
		case p.Write && p.ReadOnlyPost:
			errs = append(errs, errors.New("flags Write and ReadOnlyPost are mutually exclusive"))
		case !p.Write && !p.ReadOnlyPost:
			errs = append(errs, fmt.Errorf("AllowMethods %v needs Write = true or ReadOnlyPost = true", p.AllowMethods))
		}
	}
	if p.ReadOnlyPost && !containsFold(p.AllowMethods, http.MethodPost) {
		errs = append(errs, errors.New("ReadOnlyPost is set but POST is not in AllowMethods"))
	}
	// ReadOnlyPost names one escape hatch, not a licence for every other method:
	// anything beyond GET and POST on such a profile is a write in disguise.
	if p.ReadOnlyPost {
		for _, m := range p.AllowMethods {
			if !strings.EqualFold(m, http.MethodGet) && !strings.EqualFold(m, http.MethodPost) {
				errs = append(errs, fmt.Errorf("AllowMethods: %q is not allowed on a ReadOnlyPost profile — only GET and POST are", m))
			}
		}
	}
	return errs
}

// compileAllowPaths turns the patterns into rules, reporting whether any of them
// is bound to POST. Every pattern is checked for the three things an author gets
// wrong: a missing "^", a missing end, and a method prefix the profile does not
// allow.
func compileAllowPaths(patterns, methods []string) (rules []allowRule, postBound bool, errs []error) {
	for _, pat := range patterns {
		method, expr := splitPattern(pat)
		switch {
		case method != "" && !containsFold(methods, method):
			errs = append(errs, fmt.Errorf("AllowPaths %q names method %s, which AllowMethods does not allow", pat, method))
			continue
		// An unanchored pattern permits more than its author reads into it:
		// "/api/v1/query" matches "/anything/api/v1/query" too.
		case !strings.HasPrefix(expr, "^"):
			errs = append(errs, fmt.Errorf("AllowPaths %q must be anchored at ^", pat))
			continue
		// The end has to be said out loud as well: "$" for exactly this path,
		// "/" for a subtree. "^/api/annotations" without either matched
		// "/api/annotations/graphite" and everything else starting the same way.
		case !strings.HasSuffix(expr, "$") && !strings.HasSuffix(expr, "/"):
			errs = append(errs, fmt.Errorf("AllowPaths %q must end with $ (exact path) or / (subtree)", pat))
			continue
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			errs = append(errs, fmt.Errorf("AllowPaths %q: %w", pat, err))
			continue
		}
		if strings.EqualFold(method, http.MethodPost) {
			postBound = true
		}
		rules = append(rules, allowRule{method: method, re: re})
	}
	return rules, postBound, errs
}

// validateBaseURL guards the SSRF surface: https only, host only, no path
// tricks. http is tolerated inside the perimeter and nowhere else — for
// loopback, so a laptop can point at a local stand without a certificate, and
// for Consul service names, because the cluster's own Prometheus and Nomad
// answer plain HTTP on the datacenter network.
func validateBaseURL(raw string) error {
	if raw == "" {
		return errors.New("BaseURL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("BaseURL %q: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("BaseURL %q has no host", raw)
	}
	if u.Scheme != schemeHTTPS && !isInternalHost(u.Hostname()) {
		return fmt.Errorf("BaseURL %q must be https (http is allowed for loopback and *%s only)", raw, consulDomain)
	}
	if u.Scheme != schemeHTTPS && u.Scheme != "http" {
		return fmt.Errorf("BaseURL %q: unsupported scheme %q", raw, u.Scheme)
	}
	if strings.Contains(raw, "..") {
		return fmt.Errorf("BaseURL %q must not contain a parent-directory reference", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("BaseURL %q must not carry a path — paths come from api_call", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("BaseURL %q must be scheme://host[:port] only", raw)
	}
	return nil
}

// consulDomain is what a service name resolves under inside the datacenter. A
// host ending in it has no public DNS to hijack, which is what makes the http
// exception narrow: the name itself says the request never leaves the perimeter.
const consulDomain = ".service.consul"

// isInternalHost reports whether plain http may be spoken to a host: the machine
// itself, or a Consul service inside the datacenter.
func isInternalHost(host string) bool {
	return isLoopback(host) || strings.HasSuffix(strings.ToLower(host), consulDomain)
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// validateHeader checks the "Name: value" shape. The value is the credential
// itself — the Nomad template has already substituted it — so the only thing
// that can be checked here is that it is not empty.
func validateHeader(h string) error {
	name, value, ok := strings.Cut(h, ":")
	if !ok || !headerNameRe.MatchString(strings.TrimSpace(name)) {
		return fmt.Errorf("header %q: want \"Name: value\" with a valid header name", h)
	}
	// The empty case keeps its own wording: an empty header is a 401 from the
	// upstream, and a 401 reads as a network problem rather than as a typo.
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("header %q: empty value", h)
	}
	if !ValidHeaderValue(value) {
		return fmt.Errorf("header %q: value must be printable and under %d bytes", h, MaxHeaderValueLen)
	}
	return nil
}

// MaxHeaderValueLen caps one header value. Long enough for any token this
// catalogue carries, short enough that a header is not a way to send a body.
const MaxHeaderValueLen = 1024

// ValidHeaderValue reports whether a value may travel in a header. It answers
// for both sides of the wire — the catalogue's own headers and the ones a call
// is allowed to set — because two answers would mean a value that loads but
// cannot be sent.
//
// Control characters are the reason it exists: a value carrying CR or LF is how
// one request becomes two.
func ValidHeaderValue(v string) bool {
	if v == "" || len(v) > MaxHeaderValueLen {
		return false
	}
	for i := range len(v) {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

// ValidHeaderName reports whether a name is a header name at all. It is the same
// token production the catalogue is checked against, so a name that may be
// listed in AllowHeaders is exactly a name a call may write.
func ValidHeaderName(name string) bool { return headerNameRe.MatchString(name) }

// headerNames indexes "Name: value" pairs by their normalised name.
func headerNames(headers []string) map[string]bool {
	out := make(map[string]bool, len(headers))
	for _, h := range headers {
		if name, _, ok := strings.Cut(h, ":"); ok {
			out[headerKey(name)] = true
		}
	}
	return out
}

// deniedHeaders are names no catalogue may hand to a call, whatever it writes in
// AllowHeaders. Some decide who the caller is to the upstream or to the proxy in
// front of it: X-Impersonate switches an RPC call to another user, and the
// X-Forwarded and X-Authentik families are what the contour's forward-auth
// believes. The rest belong to the transport.
//
// Authorization is deliberately absent: an upstream whose whole point is a
// per-caller credential has to be expressible. The catalogue still has to name
// the header explicitly and must not send its own — that pair of rules is the
// guard.
var deniedHeaders = map[string]bool{
	"host":                true,
	"cookie":              true,
	"set-cookie":          true,
	"content-length":      true,
	"x-impersonate":       true,
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,

	// These defeat a guarantee of ours rather than one of the upstream's. A
	// method override turns the POST that ReadOnlyPost exists to allow back into
	// a DELETE; a URL override is the classic way past a proxy's path ACL, which
	// is what AllowPaths is. Accept-Encoding and Range decide whether Go
	// decompresses the body at all, and an undecompressed body reaches jq and
	// the PII rules as bytes neither of them can read.
	"x-http-method-override": true,
	"x-method-override":      true,
	"x-original-url":         true,
	"x-rewrite-url":          true,
	"accept-encoding":        true,
	"range":                  true,
	"x-real-ip":              true,
	"forwarded":              true,
}

// deniedHeaderPrefixes cover the families whose members cannot be listed: every
// X-Forwarded-* and X-Authentik-* header is somebody's idea of who the caller is.
var deniedHeaderPrefixes = []string{"x-forwarded-", "x-authentik-"}

// headerNameRe is the token production of RFC 7230 §3.2.6. A name outside it
// cannot be sent at all, and net/http would either escape it or panic.
var headerNameRe = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+^_`|~.-]+$")

// validateAllowHeaders checks the names a call may set and compiles them into
// the lookup the call path uses. Every finding is an error at load time: the
// alternative is a header that is quietly dropped, which the model reports as
// the upstream refusing it.
func (p *Profile) validateAllowHeaders() []error {
	var errs []error

	// Names the profile itself sends. A call may not touch them: mergeHeaders
	// already decided the profile wins, and allowing both would mean the
	// catalogue's credential depends on what the model wrote.
	own := headerNames(p.headers)

	seen := make(map[string]bool, len(p.AllowHeaders))
	for _, raw := range p.AllowHeaders {
		name := headerKey(raw)
		switch {
		case !headerNameRe.MatchString(strings.TrimSpace(raw)):
			errs = append(errs, fmt.Errorf("AllowHeaders %q is not a valid header name", raw))
			continue
		case seen[name]:
			errs = append(errs, fmt.Errorf("AllowHeaders lists %q twice", raw))
			continue
		case deniedHeaders[name]:
			errs = append(errs, fmt.Errorf("AllowHeaders %q may never be set by a call", raw))
			continue
		case own[name]:
			errs = append(errs, fmt.Errorf("AllowHeaders %q is also sent by the profile — the profile wins, so allowing it would do nothing", raw))
			continue
		}
		if i := slices.IndexFunc(deniedHeaderPrefixes, func(pre string) bool { return strings.HasPrefix(name, pre) }); i >= 0 {
			errs = append(errs, fmt.Errorf("AllowHeaders %q may never be set by a call: %s* says who the caller is", raw, deniedHeaderPrefixes[i]))
			continue
		}
		seen[name] = true
	}

	p.allowHeaders = seen
	return errs
}

// validateRedact checks the mode and the rule names, and normalises the mode in
// place. The catalogues say "redact" and live outside this repository, so
// redact.ParseMode accepts it as the synonym it is and answers with the
// canonical name every later comparison uses. A typo still fails here rather
// than quietly switching the mask off.
func validateRedact(mode *string, ruleNames []string) error {
	canonical, ok := redact.ParseMode(*mode)
	if !ok {
		return fmt.Errorf("field Redact %q: want %s, %s or %s", *mode, redact.ModeOff, redact.ModeWarn, redact.ModeOn)
	}
	*mode = canonical
	// A misspelled rule name would silently redact nothing, and nothing is
	// exactly what a leak looks like until someone reads the answer.
	known := redact.RuleNames()
	for _, name := range ruleNames {
		if !slices.Contains(known, name) {
			return fmt.Errorf("field RedactRules: unknown rule %q, known: %s", name, strings.Join(known, ", "))
		}
	}
	return nil
}

func validateTimeFormat(format string, params, bodyParams []string) error {
	switch format {
	case "":
		// A body parameter that names its own format needs no default; one that
		// does not would expand into nothing at all.
		needsDefault := len(params) > 0
		for _, entry := range bodyParams {
			if _, f := cutTimeFormat(entry, ""); f == "" {
				needsDefault = true
			}
		}
		if needsDefault {
			return errors.New("TimeParams is set but TimeFormat is empty")
		}
	default:
		if !slices.Contains(timeFormats, format) {
			return fmt.Errorf("field TimeFormat %q: want one of %s", format, strings.Join(timeFormats, ", "))
		}
	}
	return nil
}

// validateTimeBodyParams checks the dotted paths the body expansion walks. A
// path on a GET-only profile is dead config: there is no body to expand.
func (p *Profile) validateTimeBodyParams() []error {
	if len(p.TimeBodyParams) == 0 {
		return nil
	}
	var errs []error
	if !hasNonGET(p.AllowMethods) {
		errs = append(errs, errors.New("TimeBodyParams is set but the profile allows GET only — there is no body to expand"))
	}
	params, paramErrs := compileBodyTimeParams(p.TimeBodyParams, p.TimeFormat)
	p.bodyTimeParams = params
	return append(errs, paramErrs...)
}

// forbiddenReadOnlyRPC are JSON-RPC methods that change state at the upstreams
// the catalogue knows. A ReadOnlyPost profile may not name them: the flag
// promises that its POST reads.
var forbiddenReadOnlyRPC = []string{"alert.silence"}

// validateRPCMethods checks the JSON-RPC allowlist and compiles it into the
// lowercase set CheckRPC consults.
func (p *Profile) validateRPCMethods() []error {
	if len(p.AllowRPCMethods) == 0 {
		return nil
	}
	var errs []error
	if !hasNonGET(p.AllowMethods) {
		errs = append(errs, errors.New("AllowRPCMethods is set but the profile allows GET only — a GET carries no body to check"))
	}
	p.rpcMethods = make(map[string]bool, len(p.AllowRPCMethods))
	for _, m := range p.AllowRPCMethods {
		key := strings.ToLower(strings.TrimSpace(m))
		switch {
		case key == "" || strings.ContainsAny(key, " \t"):
			errs = append(errs, fmt.Errorf("AllowRPCMethods %q is not a method name", m))
			continue
		case p.rpcMethods[key]:
			errs = append(errs, fmt.Errorf("AllowRPCMethods lists %q twice", m))
		case p.ReadOnlyPost && slices.Contains(forbiddenReadOnlyRPC, key):
			errs = append(errs, fmt.Errorf("AllowRPCMethods %q changes state — not on a ReadOnlyPost profile", m))
		}
		p.rpcMethods[key] = true
	}
	return errs
}

// validateQueryParams parses AllowQueryParams into queryRules and refuses the
// entries that would mislead: a denied name, a name Query pins (the profile
// wins), a bound on a time parameter (now-1h is not a number), and a TimeParams
// key the list leaves out, which no call could ever send.
func (p *Profile) validateQueryParams() []error {
	if len(p.AllowQueryParams) == 0 {
		return nil
	}
	var errs []error
	p.queryRules = make(map[string]queryRule, len(p.AllowQueryParams))
	for _, raw := range p.AllowQueryParams {
		name, rule, err := parseQueryRule(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("AllowQueryParams %w", err))
			continue
		}
		_, dup := p.queryRules[name]
		switch {
		case queryDenied(name):
			errs = append(errs, fmt.Errorf("AllowQueryParams %q may never be set by a call", raw))
		case p.forcesKey(name):
			errs = append(errs, fmt.Errorf("AllowQueryParams %q is also pinned by Query — the profile wins, so allowing it would do nothing", raw))
		case rule.op != "" && slices.Contains(p.TimeParams, name):
			errs = append(errs, fmt.Errorf("AllowQueryParams %q: a time parameter takes now-1h, which no bound can read", raw))
		case dup:
			errs = append(errs, fmt.Errorf("AllowQueryParams lists %q twice", name))
		default:
			p.queryRules[name] = rule
		}
	}
	for _, key := range p.TimeParams {
		if _, ok := p.queryRules[key]; !ok && !p.forcesKey(key) {
			errs = append(errs, fmt.Errorf("TimeParams %q is not in AllowQueryParams — no call could send it", key))
		}
	}
	return errs
}

// validateQueryPair accepts one "key=value" for Query: the key non-empty and
// neither side carrying a separator the join would read as a second pair.
func validateQueryPair(q string) error {
	key, value, ok := strings.Cut(q, "=")
	if !ok || key == "" {
		return fmt.Errorf("field Query %q must be key=value", q)
	}
	if strings.ContainsAny(key+value, "&?# \t") {
		return fmt.Errorf("field Query %q must not contain &, ?, # or whitespace", q)
	}
	return nil
}

// validateForbidden re-runs the forbidden samples against every profile that
// made it through. This is the one check that survives a well-meaning edit
// widening a pattern.
func (c *Catalog) validateForbidden() error {
	var errs []error
	for _, name := range sortedKeys(c.Profiles) {
		p := c.Profiles[name]
		for _, f := range forbiddenCalls {
			if p.Allows(f.method, f.path) {
				errs = append(errs, fmt.Errorf("[Profiles.%s] allows %s %s — merge, delete and deploy must never pass", name, f.method, f.path))
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Catalog) validateRole(r *Role) error {
	var errs []error

	// A role nobody can reach is dead config that reads like granted access.
	if len(r.Groups) == 0 {
		errs = append(errs, errors.New("field Groups is empty — no IdP group maps to this role"))
	}
	if len(r.Tools) == 0 {
		errs = append(errs, errors.New("field Tools is empty"))
	}
	for _, t := range r.Tools {
		if t == Wildcard {
			continue
		}
		if !slices.Contains(KnownTools, t) {
			errs = append(errs, fmt.Errorf("field Tools: unknown tool %q", t))
		}
	}
	for _, t := range r.Targets {
		if t == Wildcard {
			continue
		}
		// A database is named next to the profiles and needs no flag: it is
		// read-only by construction, and the wildcard does not reach it, so
		// every grant of data is a name in a diff.
		if _, ok := c.Databases[t]; ok {
			continue
		}
		p, ok := c.Profiles[t]
		if !ok {
			errs = append(errs, fmt.Errorf("field Targets: unknown profile or database %q", t))
			continue
		}
		// Naming a write profile in a role that cannot write is a mistake worth
		// surfacing, not something to resolve quietly at request time.
		if p.Write && !r.AllowWrite {
			errs = append(errs, fmt.Errorf("field Targets: %q is a write profile but AllowWrite is false", t))
		}
	}
	for _, rp := range r.Repos {
		if rp == Wildcard {
			continue
		}
		if _, ok := c.Repos[rp]; !ok {
			errs = append(errs, fmt.Errorf("field Repos: unknown repo %q", rp))
		}
	}
	if r.MaxWrites < 0 {
		errs = append(errs, fmt.Errorf("MaxWrites %d must be >= 0", r.MaxWrites))
	}
	if r.MaxWrites > 0 && !r.AllowWrite {
		errs = append(errs, errors.New("MaxWrites is set but AllowWrite is false"))
	}
	return errors.Join(errs...)
}

// validateCloneURL keeps clones to https, with one exception: a file:// URL,
// which is what a local stand and the tests use. A local path cannot reach
// anything the process could not already read, and it shows up in a diff
// immediately if it ever appears in a production catalogue.
func validateCloneURL(raw string) error {
	if raw == "" {
		return errors.New("CloneURL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("CloneURL %q: %w", raw, err)
	}
	switch {
	case u.Scheme == schemeHTTPS && u.Host != "":
		return nil
	case u.Scheme == "file" && u.Path != "":
		return nil
	default:
		return fmt.Errorf("CloneURL %q must be an https URL (file:// is allowed for a local stand)", raw)
	}
}

// resolveCloneTarget turns CloneTarget into the token git will use. Resolved at
// load time rather than at clone time: a mistake would otherwise surface as the
// first code call answering with a 401 from GitLab, hours after the edit.
func (c *Catalog) resolveCloneTarget(r *Repo) error {
	r.cloneToken = r.CloneToken
	if r.CloneTarget == "" {
		return nil
	}
	if r.CloneToken != "" {
		return errors.New("CloneTarget and CloneToken are mutually exclusive — the token comes from the profile or from the field, not both")
	}
	p, ok := c.Profiles[r.CloneTarget]
	if !ok {
		return fmt.Errorf("CloneTarget %q is not a profile", r.CloneTarget)
	}
	// A write token clones just as well, and that is the problem: a clone needs
	// read_repository, and write credentials stay out of every place that does
	// not write.
	if p.Write {
		return fmt.Errorf("CloneTarget %q is a write profile — clones use the read profile", r.CloneTarget)
	}
	// The token goes to whatever host CloneURL names. It was issued for the
	// profile's host and no other; a CloneURL edited to point elsewhere would
	// hand the credential to whoever answers there.
	cu, err := url.Parse(r.CloneURL)
	if err != nil {
		return fmt.Errorf("CloneURL %q: %w", r.CloneURL, err)
	}
	bu, err := url.Parse(p.BaseURL)
	if err != nil {
		return fmt.Errorf("CloneTarget %q: BaseURL %q: %w", r.CloneTarget, p.BaseURL, err)
	}
	if cu.Scheme != schemeHTTPS || !strings.EqualFold(cu.Host, bu.Host) {
		return fmt.Errorf("CloneURL %q must be an https URL on the host of CloneTarget %q (%s)", r.CloneURL, r.CloneTarget, p.BaseURL)
	}
	// The profile's own headers, not the merged ones: the catalogue-wide
	// Authentik header is a pass-through for the outpost, never a GitLab token.
	token, err := gitlab.TokenFromHeaders(p.Headers)
	if err != nil {
		return fmt.Errorf("CloneTarget %q: %w", r.CloneTarget, err)
	}
	r.cloneToken = token
	return nil
}

func (r *Repo) validate() error {
	var errs []error

	if err := validateCloneURL(r.CloneURL); err != nil {
		errs = append(errs, err)
	}
	if r.DefaultBranch == "" {
		errs = append(errs, errors.New("DefaultBranch is required — code tools never default to HEAD"))
	}
	for layer, prefix := range r.Layers {
		switch {
		case prefix == "":
			errs = append(errs, fmt.Errorf("field Layers[%s] is empty", layer))
		case strings.HasPrefix(prefix, "/"):
			errs = append(errs, fmt.Errorf("field Layers[%s] = %q must be repo-relative", layer, prefix))
		case strings.Contains(prefix, ".."):
			errs = append(errs, fmt.Errorf("field Layers[%s] = %q must not contain a parent-directory reference", layer, prefix))
		}
	}
	if r.TaskIDRegexp != "" {
		// Compiled here and kept: the alternative is compiling it again for
		// every commit subject a history answer carries.
		re, err := regexp.Compile(r.TaskIDRegexp)
		if err != nil {
			errs = append(errs, fmt.Errorf("TaskIDRegexp %q: %w", r.TaskIDRegexp, err))
		}
		r.taskRe = re
	}
	return errors.Join(errs...)
}

// descriptionsSize is what the model pays for on every request: every profile
// and every database description is inlined into a tool description.
func (c *Catalog) descriptionsSize() int {
	n := 0
	for _, p := range c.Profiles {
		n += len(p.Description)
	}
	for _, d := range c.Databases {
		n += len(d.Description)
	}
	return n
}

func hasNonGET(methods []string) bool {
	for _, m := range methods {
		if !strings.EqualFold(m, http.MethodGet) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
