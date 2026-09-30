package target

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/target/targettest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shipped catalogues are the format's reference: if one stops validating,
// the example everyone copies from is wrong. All three are checked — prod and
// dev come out of the deployment file of their contour and are what the Nomad
// template renders, local is what `make init` copies.
func TestShippedCatalogsAreValid(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	t.Parallel()
	for _, c := range targettest.Catalogs(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(c.Path)
			require.NoError(t, err, c.Source)
		})
	}
}

func TestDistCatalogIsValid(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	t.Parallel()
	c, err := Load(filepath.Join("..", "..", "..", "cfg", "targets.toml.dist"))
	require.NoError(t, err)

	assert.Equal(t, EnvProd, c.Env, "the reference catalogue describes the prod contour")
	assert.Len(t, c.Profiles, 8, "eight readonly targets (slack and topsrv since 2026-09-07)")
	assert.Len(t, c.Roles, 3)
	assert.NotEmpty(t, c.Repos)

	for name, p := range c.Profiles {
		assert.False(t, p.Write, "the prod contour carries no write profile: %s", name)
	}

	grafana, ok := c.Profile("grafana")
	require.True(t, ok)
	assert.True(t, grafana.Allows("POST", "/api/ds/query"), "the one POST that is really a read")
	assert.False(t, grafana.Allows("POST", "/api/admin/users"))
	assert.True(t, strings.HasPrefix(grafana.Description, "prod"), "environment goes first (D25)")

	// A JSON-RPC API behind one path: the path allows every method, so the
	// body has to be checked, and the one that writes is not on the list.
	topsrv, ok := c.Profile("topsrv")
	require.True(t, ok)
	assert.True(t, topsrv.Allows("POST", "/api/v1/rpc/"))
	assert.False(t, topsrv.Allows("GET", "/api/v1/rpc/"), "the server answers GET with 405 anyway")
	require.NoError(t, topsrv.CheckRPC(`{"jsonrpc":"2.0","id":1,"method":"alert.list"}`))
	require.Error(t, topsrv.CheckRPC(`{"jsonrpc":"2.0","id":1,"method":"alert.silence"}`), "the write behind the same path")

	// Two windows in one body, each in the format its method reads
	// (weblogs:read, 2026-09-09): the metric range in unix seconds, the log
	// filter in RFC3339.
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	body, err := topsrv.ExpandBody(`{"id":1,"method":"metric.queryRange","params":{"start":"now-1h"}}`, at)
	require.NoError(t, err)
	assert.Contains(t, body, `"start":1788951600`)
	body, err = topsrv.ExpandBody(`{"id":1,"method":"weblog.summary","params":{"filter":{"from":"now-1h"}}}`, at)
	require.NoError(t, err)
	assert.Contains(t, body, `"from":"2026-09-09T11:00:00Z"`)

	// The clone token is the GitLab profile's token, written once.
	apisrv, ok := c.Repos["apisrv"]
	require.True(t, ok)
	assert.Equal(t, "gitlab", apisrv.CloneTarget)
	assert.Equal(t, "{{ .gitlabToken }}", apisrv.Spec("apisrv").CloneToken, "the placeholder the template substitutes")
}

// A catalogue with a GitLab profile and one repository that clones through
// it, the way every shipped catalogue is written.
const cloneTargetTOML = validTOML + `
[Profiles.gitlab]
Description  = "prod · GitLab"
BaseURL      = "https://git.example.com"
Headers      = ["PRIVATE-TOKEN: glpat-secret"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v4/projects/\\d+$"]

[Repos.apisrv]
CloneURL      = "https://git.example.com/backend/apisrv.git"
CloneTarget   = "gitlab"
DefaultBranch = "master"
`

// CloneTarget hands the profile's token to git: the same credential serves
// the API and the clone, and lives in one place.
func TestCloneTarget(t *testing.T) {
	t.Parallel()

	c, err := Parse([]byte(cloneTargetTOML))
	require.NoError(t, err)
	spec := c.Repos["apisrv"].Spec("apisrv")
	assert.Equal(t, "glpat-secret", spec.CloneToken)
	assert.Equal(t, "https://git.example.com/backend/apisrv.git", spec.CloneURL)

	// Bearer is GitLab's other spelling of the same token.
	c, err = Parse([]byte(strings.Replace(cloneTargetTOML, "PRIVATE-TOKEN: glpat-secret", "Authorization: Bearer glpat-secret", 1)))
	require.NoError(t, err)
	assert.Equal(t, "glpat-secret", c.Repos["apisrv"].Spec("apisrv").CloneToken)

	// The catalogue-wide header is the outpost pass-through, never the clone
	// token: the profile's own headers are what count.
	c, err = Parse([]byte(strings.Replace(cloneTargetTOML, "[Profiles.prom]", "[Defaults]\nHeaders = [\"Authorization: Basic dXNlcjpwYXNz\"]\n\n[Profiles.prom]", 1)))
	require.NoError(t, err)
	assert.Equal(t, "glpat-secret", c.Repos["apisrv"].Spec("apisrv").CloneToken)

	// CloneToken alone still works for a host without a profile.
	c, err = Parse([]byte(strings.Replace(cloneTargetTOML, `CloneTarget   = "gitlab"`, `CloneToken    = "literal"`, 1)))
	require.NoError(t, err)
	assert.Equal(t, "literal", c.Repos["apisrv"].Spec("apisrv").CloneToken)
}

func TestCloneTarget_Rejected(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		toml string
		want string
	}{
		"unknown profile": {
			toml: strings.Replace(cloneTargetTOML, `CloneTarget   = "gitlab"`, `CloneTarget   = "gitlb"`, 1),
			want: `CloneTarget "gitlb" is not a profile`,
		},
		"both fields": {
			toml: cloneTargetTOML + "CloneToken = \"also\"\n",
			want: "mutually exclusive",
		},
		"write profile": {
			toml: strings.Replace(cloneTargetTOML, `AllowMethods = ["GET"]
AllowPaths   = ["^/api/v4/projects/\\d+$"]`, `AllowMethods = ["POST"]
AllowPaths   = ["^/api/v4/projects/\\d+/merge_requests$"]
Write        = true`, 1),
			want: "is a write profile",
		},
		"clone url on another host": {
			// The token was issued for git.example.com; a URL pointing
			// elsewhere would hand it to whoever answers there.
			toml: strings.Replace(cloneTargetTOML, "https://git.example.com/backend/apisrv.git", "https://git.example.org/backend/apisrv.git", 1),
			want: "must be an https URL on the host of CloneTarget",
		},
		"file url with a target": {
			toml: strings.Replace(cloneTargetTOML, "https://git.example.com/backend/apisrv.git", "file:///tmp/apisrv.git", 1),
			want: "must be an https URL on the host of CloneTarget",
		},
		"profile without a token header": {
			toml: strings.Replace(cloneTargetTOML, `Headers      = ["PRIVATE-TOKEN: glpat-secret"]`, `Headers      = ["X-Custom: x"]`, 1),
			want: "nothing to clone with",
		},
		"profile with two token headers": {
			toml: strings.Replace(cloneTargetTOML, `Headers      = ["PRIVATE-TOKEN: glpat-secret"]`, `Headers      = ["PRIVATE-TOKEN: a", "Authorization: Bearer b"]`, 1),
			want: "want exactly one",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.toml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A minimal valid catalogue every negative case starts from.
const validTOML = `
Env = "prod"

[Profiles.prom]
Description  = "prod · Prometheus"
BaseURL      = "https://prom.example.com"
Headers      = ["Authorization: token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["prom"]
`

func TestParse_Valid(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validTOML))
	require.NoError(t, err)

	p, ok := c.Profile("prom")
	require.True(t, ok)
	assert.Equal(t, "prom", p.Name())
	assert.True(t, p.Allows("GET", "/api/v1/query"))
	assert.False(t, p.Allows("GET", "/api/v1/admin"), "anchored pattern must not match a prefix")
	assert.False(t, p.Allows("POST", "/api/v1/query"), "method is checked too")
}

// The model writes query parameters into `path`, while the reference patterns
// are anchored at both ends and describe the path alone. Matching the raw
// string would reject every parameterized request — which is most of them.
func TestAllows_PathWithQuery(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validTOML))
	require.NoError(t, err)
	p, ok := c.Profile("prom")
	require.True(t, ok)

	cases := map[string]bool{
		"/api/v1/query":                            true,
		"/api/v1/query?query=up":                   true,
		`/api/v1/query?query=up{job="apisrv"}&x=1`: true,
		"/api/v1/query#frag":                       true,
		"/api/v1/queryx":                           false,
		"/api/v1/admin":                            false,
		"/api/v1/query/../../admin":                false,
		"api/v1/query":                             false,
		"//evil.example.com/api/v1/query":          false,
		"https://evil.example.com/api/v1/query":    false,
		"":                                         false,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, p.Allows("GET", path))
		})
	}
}

func TestParse_RejectsBrokenCatalogs(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		toml string
		want string
	}{
		"unanchored path": {
			toml: strings.Replace(validTOML, `["^/api/v1/query$"]`, `["/api/v1/query$"]`, 1),
			want: "anchored",
		},
		"http base url": {
			toml: strings.Replace(validTOML, "https://prom.example.com", "http://prom.example.com", 1),
			want: "https",
		},
		"broken DefaultJQ": {
			toml: strings.Replace(validTOML, `Description  = "prod · Prometheus"`, "Description  = \"prod · Prometheus\"\nDefaultJQ = \".data[\"", 1),
			want: "DefaultJQ",
		},
		"base url with path": {
			toml: strings.Replace(validTOML, "https://prom.example.com", "https://prom.example.com/api", 1),
			want: "path",
		},
		"non-GET without write flag": {
			toml: strings.Replace(validTOML, `AllowMethods = ["GET"]`, `AllowMethods = ["GET", "POST"]`, 1),
			want: "Write = true",
		},
		"empty header value": {
			toml: strings.Replace(validTOML, "Authorization: token", "Authorization:", 1),
			want: "empty value",
		},
		"unknown tool in role": {
			toml: strings.Replace(validTOML, `Tools       = ["api_call"]`, `Tools       = ["api_call", "run_sql"]`, 1),
			want: "unknown tool",
		},
		"unknown target in role": {
			toml: strings.Replace(validTOML, `Targets     = ["prom"]`, `Targets     = ["prom", "loki"]`, 1),
			want: "unknown profile",
		},
		"role without groups": {
			toml: strings.Replace(validTOML, `Groups      = ["ringsrv-users"]`, `Groups      = []`, 1),
			want: "Groups is empty",
		},
		"missing env": {
			toml: strings.Replace(validTOML, "Env = \"prod\"", "", 1),
			want: "Env must be",
		},
		"description contradicts the contour": {
			toml: strings.Replace(validTOML, "Env = \"prod\"", "Env = \"dev\"", 1),
			want: "must start with",
		},
		"unknown key": {
			toml: validTOML + "\nAllowPath = \"typo\"\n",
			want: "unknown keys",
		},
		"unknown redact rule": {
			toml: validTOML + "\n[Profiles.sentry]\nDescription = \"prod · Sentry\"\nBaseURL = \"https://s.example.com\"\nAllowMethods = [\"GET\"]\nAllowPaths = [\"^/api/0/issues/\"]\nRedact = \"redact\"\nRedactRules = [\"emial\"]\n",
			want: "unknown rule",
		},
		"bad redact mode": {
			toml: validTOML + "\n[Profiles.sentry]\nDescription = \"prod · Sentry\"\nBaseURL = \"https://s.example.com\"\nAllowMethods = [\"GET\"]\nAllowPaths = [\"^/api/0/issues/\"]\nRedact = \"maybe\"\n",
			want: "Redact",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.toml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// The dev contour reaches Prometheus and Nomad by their Consul names, over
// plain HTTP, because that is the only way they answer inside the datacenter.
// The exception is the suffix and nothing else: a public name that merely
// carries the words is still a public name, and https is still required of it.
func TestParse_HTTPOnlyInsideThePerimeter(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"http://prometheus.service.consul:9090":         true,
		"http://http.nomad-servers.service.consul:4646": true,
		"http://Prometheus.Service.Consul:9090":         true,
		"http://localhost:9090":                         true,
		"https://prom.example.com":                      true,
		"http://prom.example.com":                       false,
		"http://service.consul.example.com":             false,
		"http://192.168.0.10:9090":                      false,
	}
	for base, want := range cases {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(strings.Replace(validTOML, "https://prom.example.com", base, 1)))
			if want {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be https")
		})
	}
}

// A write profile listed by a role that cannot write is a typo worth failing
// on, not something to resolve quietly at request time.
func TestParse_WriteProfileInReadOnlyRole(t *testing.T) {
	t.Parallel()
	toml := validTOML + `
[Profiles.youtrack-rw]
Description  = "prod · YouTrack (write)"
BaseURL      = "https://bugs.example.com"
AllowMethods = ["POST"]
AllowPaths   = ["^/api/issues$"]
Write        = true
`
	_, err := Parse([]byte(strings.Replace(toml, `Targets     = ["prom"]`, `Targets     = ["prom", "youtrack-rw"]`, 1)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AllowWrite is false")
}

// Merge, delete and deploy must not be expressible. This is the check that
// survives a well-meaning edit widening a pattern.
func TestParse_ForbiddenCallsNeverPass(t *testing.T) {
	t.Parallel()
	toml := `
Env = "prod"

[Profiles.gitlab-rw]
Description  = "prod · GitLab (write)"
BaseURL      = "https://git.example.com"
AllowMethods = ["POST", "PUT"]
AllowPaths   = ["^/api/v4/projects/\\d+/merge_requests/\\d+/"]
Write        = true

[Roles.oncall]
Description = "oncall"
Groups      = ["ringsrv-oncall"]
Tools       = ["api_call"]
Targets     = ["gitlab-rw"]
AllowWrite  = true
`
	_, err := Parse([]byte(toml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "merge, delete and deploy must never pass")
}

func TestResolve(t *testing.T) {
	t.Parallel()
	c, err := Load(filepath.Join("..", "..", "..", "cfg", "targets.toml.dist"))
	require.NoError(t, err)

	t.Run("no matching group is a closed door", func(t *testing.T) {
		t.Parallel()
		acc := c.Resolve([]string{"random-team"})
		assert.True(t, acc.Empty())
		assert.Empty(t, acc.Targets)
	})

	t.Run("viewer sees metrics only", func(t *testing.T) {
		t.Parallel()
		acc := c.Resolve([]string{"ringsrv-users"})
		assert.Equal(t, []string{"viewer"}, acc.Roles)
		assert.True(t, acc.HasTool("api_call"))
		assert.False(t, acc.HasTool("code_read"))
		assert.True(t, acc.HasTarget("prom"))
		assert.False(t, acc.HasTarget("gitlab"))
		assert.False(t, acc.AllowWrite)
	})

	t.Run("write targets need the write bit", func(t *testing.T) {
		t.Parallel()
		// On its own catalogue: the shipped contour has carried no write
		// profile since 2026-09-14, and the rule under test is how resolution
		// treats one, not what prod happens to allow this month.
		wc, err := Parse([]byte(validTOML + `
[Profiles.youtrack-rw]
Description  = "prod · YouTrack (write)"
BaseURL      = "https://bugs.example.com"
AllowMethods = ["POST"]
AllowPaths   = ["^/api/issues$"]
Write        = true

[Roles.oncall]
Description = "write"
Groups      = ["ringsrv-oncall"]
Tools       = ["api_call"]
Targets     = ["prom", "youtrack-rw"]
AllowWrite  = true
MaxWrites   = 10
`))
		require.NoError(t, err)

		viewer := wc.Resolve([]string{"ringsrv-users"})
		assert.False(t, viewer.AllowWrite)
		assert.False(t, viewer.HasTarget("youtrack-rw"), "a read-only role never sees a write profile")

		oncall := wc.Resolve([]string{"ringsrv-oncall"})
		assert.True(t, oncall.AllowWrite)
		assert.True(t, oncall.HasTarget("youtrack-rw"))
		assert.Equal(t, 10, oncall.MaxWrites)
	})

	t.Run("membership in several roles unions the grants", func(t *testing.T) {
		t.Parallel()
		acc := c.Resolve([]string{"ringsrv-users", "ringsrv-developers"})
		assert.Equal(t, []string{"developer", "viewer"}, acc.Roles)
		assert.True(t, acc.HasTarget("gitlab"), "from developer")
		assert.True(t, acc.HasTarget("prom"), "from viewer")
		assert.False(t, acc.AllowWrite, "neither role grants writes")
	})

	t.Run("wildcard expands to the catalogue", func(t *testing.T) {
		t.Parallel()
		acc := c.Resolve([]string{"ringsrv-oncall"})
		assert.Len(t, acc.Targets, len(c.Profiles))
		assert.Len(t, acc.Tools, len(KnownTools))
		assert.Len(t, acc.Repos, len(c.Repos))
	})
}

// Every shipped catalogue names its databases and grants them by name: the
// developer and on-call roles see both, the viewer none.
func TestDatabases_ShippedCatalogs(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	t.Parallel()
	for _, cat := range targettest.Catalogs(t) {
		t.Run(cat.Name, func(t *testing.T) {
			t.Parallel()
			c, err := Load(cat.Path)
			require.NoError(t, err, cat.Source)
			// One ClickHouse and the PostgreSQL bases of the contours the
			// catalogue covers; how many of the latter is the catalogue's
			// business, that each is whole is the code's.
			require.GreaterOrEqual(t, len(c.Databases), 2)
			all := sortedKeys(names(c.Databases))

			// The catalogue names its databases after the project; the test
			// finds them by driver, so the name stays the catalogue's business.
			ch := databaseByDriver(t, c, DriverClickHouse)
			assert.Equal(t, DriverClickHouse, ch.Driver)

			var pgName string
			for name, pg := range c.Databases {
				if pg.Driver != DriverPostgres {
					continue
				}
				pgName = name
				assert.NotEmpty(t, pg.Redact, "%s: a postgres target names its redaction", name)
				require.NotEmpty(t, pg.Schemas, name)
				assert.Equal(t, "public", pg.Schemas[0], "%s: the first schema is the head of the search path", name)
				// Every schema the catalogue shows answers with a pointer to
				// the code, resolved to a repository of this catalogue at
				// load: a schema nobody claims sends the model looking
				// through all of them.
				for _, s := range pg.Schemas {
					refs := pg.CodeRefs(s)
					require.NotEmpty(t, refs, "%s: schema %q has no repository behind it", name, s)
					for _, ref := range refs {
						assert.Contains(t, c.Repos, ref.Repo)
						assert.NotEmpty(t, ref.Layer, "the db layer resolved at load")
					}
				}
			}
			require.NotEmpty(t, pgName, "no postgres database in the catalogue")

			assert.Empty(t, c.Resolve([]string{"ringsrv-users"}).Databases, "the viewer reads metrics, not data")
			dev := c.Resolve([]string{"ringsrv-developers"})
			assert.Equal(t, all, dev.Databases, "the developer role is granted every database by name")
			assert.True(t, dev.HasDatabase(pgName))
			assert.False(t, dev.HasTarget(pgName), "a database is not an api_call target")
			if oncall := c.Resolve([]string{"ringsrv-oncall"}); !oncall.Empty() {
				assert.Equal(t, all, oncall.Databases)
			}
		})
	}
}

// A minimal valid catalogue with a database, for the negative cases.
const validDBTOML = validTOML + `
[Databases.pg]
Description = "prod · PostgreSQL"
Driver      = "postgres"
Addr        = "db.example.com:5432"
Database    = "app"
User        = "ringsrv_ro"
Password    = "secret"
Redact      = "off"
Schemas     = ["public", "billing"]
Repo        = "apisrv"

[Roles.analyst]
Description = "data"
Groups      = ["ringsrv-analysts"]
Tools       = ["db_query", "db_introspect"]
Targets     = ["prom", "pg"]

[Repos.apisrv]
CloneURL      = "https://git.example.com/backend/apisrv.git"
DefaultBranch = "master"

[Repos.apisrv.Layers]
db = "pkg/db"
`

func TestDatabases_Parse(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validDBTOML))
	require.NoError(t, err)

	pg, ok := c.Database("pg")
	require.True(t, ok)
	assert.Equal(t, []string{"public", "billing"}, pg.Schemas)
	assert.Zero(t, pg.Timeout, "unset limits stay zero — the instance config fills them in")

	timed, err := Parse([]byte(strings.Replace(validDBTOML, `Password    = "secret"`, "Password    = \"secret\"\nTimeout     = \"15s\"", 1)))
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, timed.Databases["pg"].Timeout)
	assert.Equal(t, []CodeRef{{Repo: "apisrv", Layer: "pkg/db"}}, pg.CodeRefs("public"))
	assert.Equal(t, []CodeRef{{Repo: "apisrv", Layer: "pkg/db"}}, pg.CodeRefs("billing"),
		"without SchemaRepos every schema reads the base's Repo")

	bare, err := Parse([]byte(strings.Replace(validDBTOML, `Schemas     = ["public", "billing"]`, "", 1)))
	require.NoError(t, err)
	assert.Equal(t, []string{"public"}, bare.Databases["pg"].Schemas, "the default is written down at load")

	acc := c.Resolve([]string{"ringsrv-analysts"})
	assert.Equal(t, []string{"pg"}, acc.Databases)
	assert.Equal(t, []string{"prom"}, acc.Targets)
	assert.True(t, acc.HasTool("db_query"))

	// A role may grant the tools without a database: emptiness is a property
	// of the role, not a catalogue error.
	_, err = Parse([]byte(strings.Replace(validDBTOML, `Targets     = ["prom", "pg"]`, `Targets     = ["prom"]`, 1)))
	require.NoError(t, err)
}

// One base whose schemas belong to different services: the pointer to the
// code is per schema, and a schema two services both have models for names
// both.
func TestDatabases_SchemaRepos(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validDBTOML + `
[Repos.paysrv]
CloneURL      = "https://git.example.com/backend/paysrv.git"
DefaultBranch = "master"

[Repos.paysrv.Layers]
db = "internal/store"

[Databases.pg.SchemaRepos]
billing = ["paysrv", "apisrv"]
`))
	require.NoError(t, err)

	pg, ok := c.Database("pg")
	require.True(t, ok)
	assert.Equal(t, []CodeRef{{Repo: "paysrv", Layer: "internal/store"}, {Repo: "apisrv", Layer: "pkg/db"}}, pg.CodeRefs("billing"),
		"the order is the catalogue's — the owner of the schema comes first")
	assert.Equal(t, []CodeRef{{Repo: "apisrv", Layer: "pkg/db"}}, pg.CodeRefs("public"),
		"a schema SchemaRepos does not name falls back to Repo")

	// A base that names no Repo at all answers without a pointer rather than
	// with somebody else's: the tables of an unclaimed schema are nobody's.
	bare, err := Parse([]byte(strings.Replace(validDBTOML, `Repo        = "apisrv"`, "", 1) + `
[Databases.pg.SchemaRepos]
billing = ["apisrv"]
`))
	require.NoError(t, err)
	assert.Empty(t, bare.Databases["pg"].CodeRefs("public"))
	assert.Len(t, bare.Databases["pg"].CodeRefs("billing"), 1)
}

// One base several services write names them all. On clickhouse there are no
// schemas to hang SchemaRepos on, so the base-wide pointer is the only one
// there is, and a list is how it stops being one service's.
func TestDatabases_RepoList(t *testing.T) {
	t.Parallel()
	const paysrv = `
[Repos.paysrv]
CloneURL      = "https://git.example.com/backend/paysrv.git"
DefaultBranch = "master"

[Repos.paysrv.Layers]
db = "internal/store"
`
	c, err := Parse([]byte(strings.Replace(validDBTOML, `Repo        = "apisrv"`, `Repo        = ["apisrv", "paysrv"]`, 1) + paysrv))
	require.NoError(t, err)
	assert.Equal(t, []CodeRef{{Repo: "apisrv", Layer: "pkg/db"}, {Repo: "paysrv", Layer: "internal/store"}},
		c.Databases["pg"].CodeRefs("public"), "the order is the catalogue's — the owner comes first")

	// Both forms are the same field: a base of one owner still writes a name.
	one, err := Parse([]byte(validDBTOML))
	require.NoError(t, err)
	assert.Equal(t, []CodeRef{{Repo: "apisrv", Layer: "pkg/db"}}, one.Databases["pg"].CodeRefs("public"))

	// A service that never grew a db package keeps the row types in its
	// domain: the pointer goes there rather than nowhere.
	domain, err := Parse([]byte(strings.Replace(validDBTOML, `db = "pkg/db"`, `domain = "pkg/app"`, 1)))
	require.NoError(t, err)
	assert.Equal(t, []CodeRef{{Repo: "apisrv", Layer: "pkg/app"}}, domain.Databases["pg"].CodeRefs("public"))

	for _, tt := range []struct{ name, line, want string }{
		{"empty list", `Repo        = []`, "names no repository"},
		{"unknown repository", `Repo        = ["apisrv", "nosuch"]`, `"nosuch" is not a repository`},
		{"named twice", `Repo        = ["apisrv", "apisrv"]`, `names "apisrv" twice`},
		{"not a name", `Repo        = ["apisrv", 1]`, "element 2 is int64"},
		{"neither form", `Repo        = 1`, "field Repo is int64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(strings.Replace(validDBTOML, `Repo        = "apisrv"`, tt.line, 1)))
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// The wildcard came into the catalogue for metrics and logs; production data
// must not follow it at the next edit.
func TestDatabases_WildcardDoesNotGrantThem(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validDBTOML + `
[Roles.everything]
Description = "star"
Groups      = ["platform"]
Tools       = ["*"]
Targets     = ["*"]

[Roles.named]
Description = "star and a name"
Groups      = ["data-platform"]
Tools       = ["*"]
Targets     = ["*", "pg"]
`))
	require.NoError(t, err)

	star := c.Resolve([]string{"platform"})
	assert.Equal(t, []string{"prom"}, star.Targets)
	assert.Empty(t, star.Databases, "the wildcard expands to profiles only")
	assert.True(t, star.HasTool("db_query"), "the tool may be granted; the data is not")

	named := c.Resolve([]string{"data-platform"})
	assert.Equal(t, []string{"pg"}, named.Databases)
}

// The twenty-four ways a database section can be wrong without failing on its
// own — each would surface hours later as a first query that looks like a
// network fault.
func TestDatabases_RejectsBroken(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		toml string
		want string
	}{
		"name is also a profile": {
			toml: strings.Replace(validDBTOML, "[Databases.pg]", "[Databases.prom]", 1),
			want: "also a profile",
		},
		"description without the contour": {
			toml: strings.Replace(validDBTOML, `Description = "prod · PostgreSQL"`, `Description = "PostgreSQL"`, 1),
			want: "must start with",
		},
		"empty description": {
			toml: strings.Replace(validDBTOML, `Description = "prod · PostgreSQL"`, `Description = ""`, 1),
			want: "Description is required",
		},
		"unknown driver": {
			toml: strings.Replace(validDBTOML, `Driver      = "postgres"`, `Driver      = "mysql"`, 1),
			want: "Driver",
		},
		"addr with a scheme": {
			toml: strings.Replace(validDBTOML, `Addr        = "db.example.com:5432"`, `Addr        = "postgres://db.example.com:5432"`, 1),
			want: "host:port",
		},
		"addr without a port": {
			toml: strings.Replace(validDBTOML, `Addr        = "db.example.com:5432"`, `Addr        = "db.example.com"`, 1),
			want: "host:port",
		},
		"addr with a bad port": {
			toml: strings.Replace(validDBTOML, `Addr        = "db.example.com:5432"`, `Addr        = "db.example.com:70000"`, 1),
			want: "1..65535",
		},
		"empty password": {
			toml: strings.Replace(validDBTOML, `Password    = "secret"`, `Password    = ""`, 1),
			want: "Password is required",
		},
		"empty user": {
			toml: strings.Replace(validDBTOML, `User        = "ringsrv_ro"`, `User        = ""`, 1),
			want: "User is required",
		},
		"postgres without redact": {
			toml: strings.Replace(validDBTOML, `Redact      = "off"`, "", 1),
			want: "Redact is required for postgres",
		},
		"unknown redact rule": {
			toml: strings.Replace(validDBTOML, `Redact      = "off"`, "Redact      = \"redact\"\nRedactRules = [\"emial\"]", 1),
			want: "unknown rule",
		},
		"schema that is not an identifier": {
			toml: strings.Replace(validDBTOML, `Schemas     = ["public", "billing"]`, `Schemas     = ["a-b"]`, 1),
			want: "not an identifier",
		},
		"schemas on clickhouse": {
			toml: strings.Replace(strings.Replace(validDBTOML, `Driver      = "postgres"`, `Driver      = "clickhouse"`, 1), `Redact      = "off"`, "", 1),
			want: "Schemas is for postgres",
		},
		"repo that does not exist": {
			toml: strings.Replace(validDBTOML, `Repo        = "apisrv"`, `Repo        = "billing"`, 1),
			want: "not a repository",
		},
		"repo without a db layer": {
			toml: strings.Replace(validDBTOML, `db = "pkg/db"`, `rpc = "pkg/rpc"`, 1),
			want: "no Layers.db",
		},
		"negative rows": {
			toml: strings.Replace(validDBTOML, `Password    = "secret"`, "Password    = \"secret\"\nMaxRows     = -1", 1),
			want: "MaxRows -1",
		},
		"negative timeout": {
			toml: strings.Replace(validDBTOML, `Password    = "secret"`, "Password    = \"secret\"\nTimeout     = \"-1s\"", 1),
			want: "Timeout",
		},
		"bare integer timeout is nanoseconds": {
			toml: strings.Replace(validDBTOML, `Password    = "secret"`, "Password    = \"secret\"\nTimeout     = 15", 1),
			want: "duration string",
		},
		"role names a database that does not exist": {
			toml: strings.Replace(validDBTOML, `Targets     = ["prom", "pg"]`, `Targets     = ["prom", "ch"]`, 1),
			want: "unknown profile or database",
		},
		"schema repos on a schema outside Schemas": {
			toml: validDBTOML + "\n[Databases.pg.SchemaRepos]\naudit = [\"apisrv\"]\n",
			want: "not in Schemas",
		},
		"schema repos naming no repository": {
			toml: validDBTOML + "\n[Databases.pg.SchemaRepos]\nbilling = []\n",
			want: "names no repository",
		},
		"schema repos with an unknown repository": {
			toml: validDBTOML + "\n[Databases.pg.SchemaRepos]\nbilling = [\"paysrv\"]\n",
			want: "not a repository",
		},
		"schema repos naming one repository twice": {
			toml: validDBTOML + "\n[Databases.pg.SchemaRepos]\nbilling = [\"apisrv\", \"apisrv\"]\n",
			want: `names "apisrv" twice`,
		},
		"schema repos on clickhouse": {
			toml: strings.Replace(strings.Replace(strings.Replace(validDBTOML,
				`Driver      = "postgres"`, `Driver      = "clickhouse"`, 1),
				`Redact      = "off"`, "", 1),
				`Schemas     = ["public", "billing"]`, "", 1) +
				"\n[Databases.pg.SchemaRepos]\nbilling = [\"apisrv\"]\n",
			want: "SchemaRepos is for postgres",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.toml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope.toml")
}

// The descriptions are inlined into the tool description on every request, so
// the catalogue cannot grow the context budget unnoticed.
func TestValidate_DescriptionsBudget(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("очень длинное описание таргета, ", 200)
	toml := strings.Replace(validTOML, `Description  = "prod · Prometheus"`, `Description  = "prod · `+long+`"`, 1)

	_, err := Parse([]byte(toml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget")
}

// A wildcard is a convenience, not a way around the write bit: a role that
// lists every target but cannot write must still not see the write profiles.
func TestResolve_WildcardDoesNotGrantWrites(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validTOML + `
[Profiles.youtrack-rw]
Description  = "prod · YouTrack (write)"
BaseURL      = "https://bugs.example.com"
AllowMethods = ["POST"]
AllowPaths   = ["^/api/issues$"]
Write        = true

[Roles.everything]
Description = "reads everything"
Groups      = ["ringsrv-readers"]
Tools       = ["*"]
Targets     = ["*"]
AllowWrite  = false
`))
	require.NoError(t, err)

	acc := c.Resolve([]string{"ringsrv-readers"})
	assert.True(t, acc.HasTarget("prom"))
	assert.False(t, acc.HasTarget("youtrack-rw"), "the wildcard expands to what the role may have, not to everything")
	assert.False(t, acc.AllowWrite)
}

// Exclude does two jobs: it keeps vendored code out of a search that would
// otherwise drown in it, and it keeps keys from leaving the disk at all.
func TestRepo_Excluded(t *testing.T) {
	t.Parallel()
	c, err := Load(filepath.Join("..", "..", "..", "cfg", "targets.toml.dist"))
	require.NoError(t, err)

	// Any repository of the example: every one of them carries the same
	// exclusions, and naming one ties the test to the catalogue a checkout
	// happens to ship rather than to Excluded.
	names := slices.Sorted(maps.Keys(c.Repos))
	require.NotEmpty(t, names, "the example catalogue carries no repositories")
	r := c.Repos[names[0]]

	excluded := []string{
		"vendor/github.com/x/y.go",
		"internal/vendor/pkg/file.go",
		"certs/server.pem",
		".env",
		".env.local",
		"node_modules/pkg/index.js",
		"id_rsa",
	}
	for _, p := range excluded {
		assert.True(t, r.Excluded(p), "%q must not be readable", p)
	}

	for _, p := range []string{"internal/rpc/order.go", "main.go", "docs/env.md"} {
		assert.False(t, r.Excluded(p), "%q is ordinary code", p)
	}
}

func TestRepo_Spec(t *testing.T) {
	t.Parallel()
	c, err := Load(filepath.Join("..", "..", "..", "cfg", "targets.toml.dist"))
	require.NoError(t, err)

	// Whichever repository the example carries: under test is the mapping into
	// a mirror spec, not one entry of one catalogue. Which repositories are in
	// the example is a property of the checkout — a contour names its own, a
	// public one names none of them.
	names := slices.Sorted(maps.Keys(c.Repos))
	require.NotEmpty(t, names, "the example catalogue carries no repositories")
	name := names[0]
	r := c.Repos[name]

	spec := r.Spec(name)
	assert.Equal(t, name, spec.Name)
	assert.Equal(t, r.DefaultBranch, spec.DefaultBranch)
	assert.NotEmpty(t, spec.DefaultBranch, "the code tools never take HEAD silently")
	assert.Equal(t, r.Tags, spec.Tags)
	assert.NotEmpty(t, spec.CloneURL)
}

// The tracker request comes from the catalogue, so a repository on another
// tracker is a config change rather than a Go edit.
func TestIssueRequest(t *testing.T) {
	t.Parallel()

	// Empty means YouTrack, which is what every repository uses today.
	def := &Repo{}
	assert.Equal(t, "/api/issues/ABC-1?fields=summary,description,comments(text)", def.IssueRequest("ABC-1"))

	jira := &Repo{IssuePath: "/rest/api/2/issue/{id}?fields=summary,description,comment"}
	assert.Equal(t, "/rest/api/2/issue/ABC-1?fields=summary,description,comment", jira.IssueRequest("ABC-1"))
}

// A path the profile does not allow would make every `why` answer "tracker
// unavailable" with nothing to say why — so the catalogue refuses to load.
func TestValidate_IssuePathMustPassTheAllowlist(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`
Env = "dev"

[Profiles.youtrack]
Description  = "dev · YouTrack"
BaseURL      = "https://bugs.example.com"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/issues(/[A-Z]+-\\d+)?$"]

[Repos.apisrv]
Description   = "test"
CloneURL      = "https://git.example.com/x.git"
DefaultBranch = "master"
IssueTarget   = "youtrack"
IssuePath     = "/rest/api/2/issue/{id}"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IssuePath")
	assert.Contains(t, err.Error(), "not allowed by profile")
}

// A contour behind an Authentik forward-auth: without the
// service header every target answers with a redirect to the login page. The
// header is declared once, or the eighth target gets added without it.
func TestDefaultHeaders(t *testing.T) {
	t.Parallel()

	const catalog = `
Env = "dev"

[Defaults]
Headers = ["X-Authentik-Token: outpost-secret"]

[Profiles.prom]
Description  = "dev · Prometheus"
BaseURL      = "https://prom.example.com"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Profiles.grafana]
Description  = "dev · Grafana"
BaseURL      = "https://grafana.example.com"
Headers      = ["Authorization: Bearer grafana-token"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api/search$"]

[Profiles.own]
Description  = "dev · target with its own idea of that header"
BaseURL      = "https://own.example.com"
Headers      = ["x-authentik-token: mine"]
AllowMethods = ["GET"]
AllowPaths   = ["^/api$"]

[Profiles.direct]
Description        = "dev · reached without the outpost"
BaseURL            = "https://direct.example.com"
SkipDefaultHeaders = true
AllowMethods       = ["GET"]
AllowPaths         = ["^/api$"]

[Roles.viewer]
Description = "reads"
Groups      = ["g"]
Tools       = ["api_call"]
Targets     = ["prom", "grafana", "own", "direct"]
`

	c, err := Parse([]byte(catalog))
	require.NoError(t, err)

	headers := func(name string) []string {
		p, ok := c.Profile(name)
		require.True(t, ok)
		return p.Upstream().Headers
	}

	assert.Equal(t, []string{"X-Authentik-Token: outpost-secret"}, headers("prom"),
		"a profile with no headers of its own still gets the contour's")
	assert.Equal(t, []string{"X-Authentik-Token: outpost-secret", "Authorization: Bearer grafana-token"}, headers("grafana"),
		"its own credentials come on top of the contour's")

	// Header names are case-insensitive, so a profile spelling it differently
	// still overrides rather than ending up with both.
	assert.Equal(t, []string{"x-authentik-token: mine"}, headers("own"))

	assert.Equal(t, []string(nil), headers("direct"),
		"a target reached without the outpost says so and gets nothing")
}

// A default header that is not "Name: value" would be sent to every target at
// once, so it is refused at load like any other broken field.
func TestDefaultHeaders_Malformed(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`
Env = "dev"

[Defaults]
Headers = ["X-Authentik-Token"]

[Profiles.prom]
Description  = "dev · Prometheus"
BaseURL      = "https://prom.example.com"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Roles.viewer]
Description = "reads"
Groups      = ["g"]
Tools       = ["api_call"]
Targets     = ["prom"]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[Defaults]")
}

// A pattern may be bound to one method, and on a ReadOnlyPost profile a bare
// pattern is GET-only. This is the Grafana lesson: POST in
// AllowMethods plus a prefix pattern let a viewer create annotations, because
// nothing said which method the pattern was written for.
func TestAllows_MethodBoundPatterns(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(`
Env = "prod"

[Profiles.grafana]
Description  = "prod · Grafana"
BaseURL      = "https://grafana.example.com"
Headers      = ["Authorization: token"]
AllowMethods = ["GET", "POST"]
AllowPaths   = ["^/api/annotations$", "^/api/search$", "POST ^/api/ds/query$"]
ReadOnlyPost = true

[Profiles.youtrack-rw]
Description  = "prod · YouTrack write"
BaseURL      = "https://youtrack.example.com"
Headers      = ["Authorization: token"]
AllowMethods = ["GET", "POST"]
AllowPaths   = ["^/api/issues$", "GET ^/api/admin/projects$"]
Write        = true

[Roles.viewer]
Description = "read"
Groups      = ["g"]
Tools       = ["api_call"]
Targets     = ["grafana"]
`))
	require.NoError(t, err)

	grafana, ok := c.Profile("grafana")
	require.True(t, ok)
	cases := map[string]bool{
		"GET /api/annotations":           true,
		"POST /api/annotations":          false, // bare pattern, ReadOnlyPost → GET only
		"GET /api/search":                true,
		"POST /api/ds/query":             true,
		"GET /api/ds/query":              false, // bound to POST alone
		"POST /api/annotations/graphite": false,
	}
	for call, want := range cases {
		method, path, _ := strings.Cut(call, " ")
		assert.Equal(t, want, grafana.Allows(method, path), call)
	}
	assert.True(t, grafana.AllowsPath("/api/ds/query"), "a cheat sheet asks about the path alone")

	// On a write profile a bare pattern keeps applying to every allowed
	// method; a prefix narrows it.
	youtrack, ok := c.Profile("youtrack-rw")
	require.True(t, ok)
	assert.True(t, youtrack.Allows("POST", "/api/issues"))
	assert.True(t, youtrack.Allows("GET", "/api/issues"))
	assert.True(t, youtrack.Allows("GET", "/api/admin/projects"))
	assert.False(t, youtrack.Allows("POST", "/api/admin/projects"))
}

// The end of a pattern has to be said out loud, the method prefix has to be
// an allowed method, and a ReadOnlyPost profile has to name its POST.
func TestParse_PatternShape(t *testing.T) {
	t.Parallel()
	base := func(paths, methods, flags string) string {
		return `
Env = "prod"

[Profiles.p]
Description  = "prod · P"
BaseURL      = "https://p.example.com"
Headers      = ["Authorization: token"]
AllowMethods = ` + methods + `
AllowPaths   = ` + paths + `
` + flags + `

[Roles.viewer]
Description = "read"
Groups      = ["g"]
Tools       = ["api_call"]
Targets     = ["p"]
`
	}
	cases := map[string]struct {
		toml string
		want string
	}{
		"no end anchor": {
			toml: base(`["^/api/v1/query"]`, `["GET"]`, ""),
			want: "must end with $",
		},
		"subtree with a trailing slash is explicit enough": {
			toml: base(`["^/api/0/issues/"]`, `["GET"]`, ""),
		},
		"method prefix outside AllowMethods": {
			toml: base(`["PUT ^/api/x$"]`, `["GET"]`, ""),
			want: "names method PUT",
		},
		"ReadOnlyPost without a POST pattern": {
			toml: base(`["^/api/x$"]`, `["GET", "POST"]`, "ReadOnlyPost = true"),
			want: "no pattern is prefixed with POST",
		},
		"ReadOnlyPost with DELETE": {
			toml: base(`["^/api/x$", "POST ^/api/q$"]`, `["GET", "POST", "DELETE"]`, "ReadOnlyPost = true"),
			want: `"DELETE" is not allowed on a ReadOnlyPost profile`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.toml))
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A percent-encoded ".." is ".." to the server that decodes it. It has to be
// refused for every profile, whatever the pattern — which is why it is also
// one of the forbidden samples the validator runs.
func TestAllows_EncodedTraversal(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(strings.Replace(validTOML, `["^/api/v1/query$"]`, `["^/api/v1/"]`, 1)))
	require.NoError(t, err)
	p, ok := c.Profile("prom")
	require.True(t, ok)

	for _, path := range []string{
		"/api/v1/%2e%2e/%2e%2e/admin",
		"/api/v1/%2E%2E/admin",
		"/api/v1/x%2f..%2fadmin",
		"/api/v1/x/%00",
		"/api/v1/x%5c..%5cadmin",
		"/api/v1//admin",
		"/api/v1/./admin",
	} {
		assert.False(t, p.Allows("GET", path), path)
	}
	// Escapes that decode to something harmless stay allowed: GitLab reads a
	// file path as one segment only when its slashes are encoded.
	assert.True(t, p.Allows("GET", "/api/v1/internal%2Frpc%2Forder.go"))
	assert.True(t, p.Allows("GET", "/api/v1/a%20b"))
}

// replaceLine swaps the one line of a TOML fixture that starts with prefix.
func replaceLine(toml, prefix, line string) string {
	lines := strings.Split(toml, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			lines[i] = line
		}
	}
	return strings.Join(lines, "\n")
}
