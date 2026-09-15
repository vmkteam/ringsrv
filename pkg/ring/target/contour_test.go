package target

import (
	"regexp"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target/targettest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/redact"
)

// The shipped catalogues describe one project on three contours: prod and dev
// (the `targets` heredoc of deployments/master.vars.hcl and devel.vars.hcl) and
// a laptop that mirrors dev with literal stubs (cfg/targets.local.toml.dist).
// A contour is the set of
// answers to the questionnaire of docs/onboarding-catalogue.md step 7 —
// the contour itself, its hosts, the forward-auth in front of them, the
// environment of a shared upstream, database addresses and redaction, limits,
// the deploy branch and who may write — and nothing else. Allowlists, jq
// filters, header schemes, repository layers are knowledge about the systems,
// and the systems are the same on every contour.
//
// This test holds that line. A profile that differs outside the questionnaire
// is drift, and drift surfaces in prod as a 401 with nothing to compare
// against: the prod catalogue once carried a YouTrack header without the
// Bearer scheme that dev had, and nothing said so.
func TestShippedCatalogsDifferOnlyByContour(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	t.Parallel()
	base := loadShipped(t, targettest.Dev)

	for _, name := range []string{targettest.Prod, targettest.Local} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := loadShipped(t, name)

			for pname, p := range c.Profiles {
				b, ok := base.Profiles[pname]
				if !ok {
					t.Logf("profile %s is not in the dev catalogue (questionnaire: writes or a system dev lacks)", pname)
					continue
				}
				assert.Equal(t, profileSystem(b), profileSystem(p), "profile %s differs from dev outside the contour questionnaire", pname)
			}
			for dname, d := range c.Databases {
				b, ok := base.Databases[dname]
				if !ok {
					t.Logf("database %s is not in the dev catalogue", dname)
					continue
				}
				assert.Equal(t, databaseSystem(b), databaseSystem(d), "database %s differs from dev outside the contour questionnaire", dname)
			}
			for rname, r := range c.Repos {
				b, ok := base.Repos[rname]
				if !ok {
					t.Logf("repository %s is not in the dev catalogue", rname)
					continue
				}
				assert.Equal(t, repoSystem(b), repoSystem(r), "repository %s differs from dev outside the contour questionnaire", rname)
			}
			for rname, r := range c.Roles {
				b, ok := base.Roles[rname]
				if !ok {
					t.Logf("role %s is not in the dev catalogue (questionnaire: writes)", rname)
					continue
				}
				// IdP groups belong to the organisation, not to a contour:
				// the same people are the same role everywhere.
				assert.Equal(t, b.Groups, r.Groups, "role %s binds different groups than dev", rname)
			}
		})
	}

	// The laptop catalogue is dev with stubs: same targets, same names, so
	// that make init produces what the dev job runs (cfg/README.md).
	t.Run("the laptop catalogue mirrors dev", func(t *testing.T) {
		t.Parallel()
		local := loadShipped(t, targettest.Local)
		assert.Equal(t, base.Env, local.Env)
		assert.ElementsMatch(t, sortedKeys(names(base.Profiles)), sortedKeys(names(local.Profiles)), "profiles")
		assert.ElementsMatch(t, sortedKeys(names(base.Databases)), sortedKeys(names(local.Databases)), "databases")
		assert.ElementsMatch(t, sortedKeys(names(base.Repos)), sortedKeys(names(local.Repos)), "repositories")
		assert.ElementsMatch(t, sortedKeys(names(base.Roles)), sortedKeys(names(local.Roles)), "roles")
	})
}

// databaseByDriver finds the one database of a driver in a catalogue. The
// shipped catalogues name their databases after the project, and the tests
// do not repeat that name: which project ships here is not the code's business.
func databaseByDriver(t *testing.T, c *Catalog, driver string) *Database {
	t.Helper()
	var found string
	for name, d := range c.Databases {
		if d.Driver == driver {
			require.Empty(t, found, "two %s databases in one catalogue, the test expects one", driver)
			found = name
		}
	}
	require.NotEmpty(t, found, "no %s database in the catalogue", driver)
	return c.Databases[found]
}

func loadShipped(t *testing.T, contour string) *Catalog {
	t.Helper()
	c, err := Load(targettest.Path(t, contour))
	require.NoError(t, err)
	return c
}

func names[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// secretRe matches a template placeholder or the literal stub of the laptop
// catalogue; both stand for "a secret goes here", and the comparison must
// not care which.
var secretRe = regexp.MustCompile(`\{\{[^}]*\}\}|replace-me`)

func neutral(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = secretRe.ReplaceAllString(s, "<secret>")
	}
	return out
}

// profileFacts is what a profile says about the upstream itself. Left out on
// purpose, as the questionnaire's axes: Description, BaseURL,
// SkipDefaultHeaders, Query, Redact, MaxBytes.
type profileFacts struct {
	Headers      []string
	AllowHeaders []string
	AllowMethods []string
	AllowPaths   []string
	DefaultJQ    string
	TimeParams   []string
	TimeFormat   string
	TimeBody     []string
	RPCMethods   []string
	QueryParams  []string
	RedactRules  []string
	Write        bool
	ReadOnlyPost bool
}

func profileSystem(p *Profile) profileFacts {
	return profileFacts{
		Headers:      neutral(p.Headers),
		AllowHeaders: p.AllowHeaders,
		AllowMethods: p.AllowMethods,
		AllowPaths:   p.AllowPaths,
		DefaultJQ:    p.DefaultJQ,
		TimeParams:   p.TimeParams,
		TimeFormat:   p.TimeFormat,
		TimeBody:     p.TimeBodyParams,
		RPCMethods:   p.AllowRPCMethods,
		QueryParams:  p.AllowQueryParams,
		RedactRules:  p.RedactRules,
		Write:        p.Write,
		ReadOnlyPost: p.ReadOnlyPost,
	}
}

// databaseFacts leaves out Description, Addr, Redact and the limits: a
// replica or a primary, redact or warn, how many connections — that is what
// a contour decides.
type databaseFacts struct {
	Driver      string
	Database    string
	User        string
	Password    string
	Schemas     []string
	Repo        RepoList
	SchemaRepos map[string][]string
	RedactRules []string
}

func databaseSystem(d *Database) databaseFacts {
	return databaseFacts{
		Driver:      d.Driver,
		Database:    d.Database,
		User:        d.User,
		Password:    neutral([]string{d.Password})[0],
		Schemas:     d.Schemas,
		Repo:        d.Repo,
		SchemaRepos: d.SchemaRepos,
		RedactRules: d.RedactRules,
	}
}

// repoFacts leaves out DefaultBranch only: which branch a contour deploys is
// the one thing about a repository that the contour decides.
type repoFacts struct {
	GitLabProject int
	Description   string
	CloneURL      string
	CloneTarget   string
	CloneToken    string
	Refspec       []string
	Tags          bool
	SentrySlug    string
	IssueTarget   string
	IssuePath     string
	NomadJob      string
	PromJob       string
	Layers        map[string]string
	Exclude       []string
	TaskIDRegexp  string
}

func repoSystem(r *Repo) repoFacts {
	return repoFacts{
		GitLabProject: r.GitLabProject,
		Description:   r.Description,
		CloneURL:      r.CloneURL,
		CloneTarget:   r.CloneTarget,
		CloneToken:    neutral([]string{r.CloneToken})[0],
		Refspec:       r.Refspec,
		Tags:          r.Tags,
		SentrySlug:    r.SentrySlug,
		IssueTarget:   r.IssueTarget,
		IssuePath:     r.IssuePath,
		NomadJob:      r.NomadJob,
		PromJob:       r.PromJob,
		Layers:        r.Layers,
		Exclude:       r.Exclude,
		TaskIDRegexp:  r.TaskIDRegexp,
	}
}

// The questionnaire is short on purpose, and the prod catalogue answers every
// line of it: what dev and prod differ in is exactly what the onboarding asks.
func TestProdCatalogAnswersTheQuestionnaire(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	t.Parallel()
	dev := loadShipped(t, targettest.Dev)
	prod := loadShipped(t, targettest.Prod)

	assert.Equal(t, EnvDev, dev.Env)
	assert.Equal(t, EnvProd, prod.Env)

	// Shared systems keep their host; the contour's own systems are reached in a
	// way that cannot leave the contour. There are two such ways, and they look
	// like opposites.
	for _, shared := range []string{"sentry", "gitlab", "youtrack"} {
		assert.Equal(t, dev.Profiles[shared].BaseURL, prod.Profiles[shared].BaseURL, "%s is one instance for both contours", shared)
	}
	// A public name could be pointed at either contour, so the two must not
	// share it: that the strings differ is the whole guarantee.
	for _, public := range []string{"grafana"} {
		assert.NotEqual(t, dev.Profiles[public].BaseURL, prod.Profiles[public].BaseURL, "%s is a public host of the contour", public)
	}
	// A service name is the same string on both, and the sameness is the
	// guarantee rather than a hole in it: it resolves inside the cluster that
	// asks, so a dev instance cannot reach prod's Prometheus by using it. What
	// would break the contours apart here is a public host appearing instead.
	for _, discovered := range []string{"prom", "nomad"} {
		for env, cat := range map[string]*Catalog{EnvDev: dev, EnvProd: prod} {
			assert.Contains(t, cat.Profiles[discovered].BaseURL, ".service.consul",
				"%s on %s is reached through service discovery, which is what keeps it inside the contour", discovered, env)
		}
	}
	assert.Equal(t, []string{"environment=production"}, prod.Profiles["sentry"].Query, "the shared Sentry is pinned to the contour by Query (D5)")

	// The prod databases are replicas with redaction on; dev reads the
	// primary and only counts. Both contours carry the same
	// bases — the SAST one among them — under the same names.
	assert.ElementsMatch(t, sortedKeys(names(dev.Databases)), sortedKeys(names(prod.Databases)), "same databases on both contours")
	for name, pgProd := range prod.Databases {
		if pgProd.Driver != DriverPostgres {
			continue
		}
		pgDev, ok := dev.Databases[name]
		require.True(t, ok, "database %s is not in the dev catalogue", name)
		assert.NotEqual(t, pgDev.Addr, pgProd.Addr, "%s: prod reads a replica, dev the primary", name)
		assert.Equal(t, redact.ModeOn, pgProd.Redact, "%s: prod masks — the catalogue may say either spelling, validation normalises it", name)
	}
	chProd := databaseByDriver(t, prod, DriverClickHouse)
	assert.Equal(t, 2, chProd.MaxConcurrent, "the prod pool is capped in the catalogue, dev takes the client default")

	// Every repository the dev contour knows is deployed from master in prod —
	// or from main, where the repository has no master: a repository without a
	// release cycle names its trunk the way its author did, and the
	// questionnaire records which those are rather than pretending they deploy
	// a branch that is not there.
	assert.ElementsMatch(t, sortedKeys(names(dev.Repos)), sortedKeys(names(prod.Repos)), "same repositories on both contours")
	for name, r := range prod.Repos {
		assert.Contains(t, []string{"master", "main"}, r.DefaultBranch, "prod deploys master (or main): %s", name)
	}

	// Neither contour writes (2026-09-14). Writes lived in prod alone, as
	// separate profiles behind the oncall role; the questionnaire now answers
	// "no writes" for prod as well, so AllowWrite is false everywhere and the
	// profiles are gone rather than left inert behind a disabled flag.
	for env, cat := range map[string]*Catalog{EnvDev: dev, EnvProd: prod} {
		for name, p := range cat.Profiles {
			assert.False(t, p.Write, "%s has no write profile: %s", env, name)
		}
		for name, r := range cat.Roles {
			assert.False(t, r.AllowWrite, "%s: role %s may not write", env, name)
		}
	}
}

// The default filter of a profile runs on every call that does not bring its
// own, so it has to fit every shape the allowlist can produce. Prometheus has
// two: query and query_range wrap the result in .data.result, while labels,
// label/<name>/values, series and status/buildinfo put the answer straight
// into .data. The single-shape filter this profile started with answered
// JQFailed on an upstream 200 for three paths of five and handed back a silent
// null on the fourth — a target that looked healthy in the metrics and was
// half unusable.
func TestPromDefaultJQFitsEveryShape(t *testing.T) {
	t.Parallel()
	for _, contour := range []string{targettest.Dev, targettest.Prod, targettest.Local} {
		t.Run(contour, func(t *testing.T) {
			t.Parallel()
			prom, ok := loadShipped(t, contour).Profiles["prom"]
			require.True(t, ok)
			ctx := t.Context()

			for _, tt := range []struct {
				name, body string
				want       any
			}{
				{
					name: "query unwraps the envelope",
					body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up"},"value":[0,"1"]}]}}`,
					want: []any{map[string]any{"metric": map[string]any{"__name__": "up"}, "value": []any{0, "1"}}},
				},
				{
					name: "no series is an empty result, not a failure",
					body: `{"status":"success","data":{"resultType":"vector","result":[]}}`,
					want: []any{},
				},
				{
					name: "label values are an array in .data",
					body: `{"status":"success","data":["up","go_goroutines"]}`,
					want: []any{"up", "go_goroutines"},
				},
				{
					name: "series are objects in .data",
					body: `{"status":"success","data":[{"__name__":"up","job":"apisrv"}]}`,
					want: []any{map[string]any{"__name__": "up", "job": "apisrv"}},
				},
				{
					name: "buildinfo is an object without result",
					body: `{"status":"success","data":{"version":"3.13.1"}}`,
					want: map[string]any{"version": "3.13.1"},
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					got, err := ring.ApplyJQ(ctx, prom.DefaultJQ, []byte(tt.body), time.Second)
					require.NoError(t, err)
					assert.Equal(t, tt.want, got)
				})
			}
		})
	}
}
