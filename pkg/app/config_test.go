package app

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/target/targettest"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/auth"
)

// The Nomad job templates live far away from this struct and drift silently: a
// section added to Config (and to cfg/*.dist) but forgotten in
// deployments/*.vars.hcl costs nothing at boot — the feature just stays off in
// production. These tests render the templates and decode them into Config.

var (
	tmplPlaceholder = regexp.MustCompile(`\{\{\s*\.([A-Za-z0-9_]+)\s*\}\}`)
	heredoc         = regexp.MustCompile(`(?s)config\s*=\s*<<-EOF\n(.*?)\nEOF`)
	catalogue       = regexp.MustCompile(`(?sm)^targets\s*=\s*<<-EOF$\n(.*?)\n^EOF$`)
)

// renderVars extracts the TOML heredoc from a *.vars.hcl and substitutes the
// consul-template bits: control lines ({{- with }} / {{- end }}) are dropped,
// and each {{ .nomadVarKey }} becomes its own key name — a literal that parses
// as TOML and stays distinguishable from the other placeholders.
// readDeployment reads a file from deployments/, or skips when it is not in
// this checkout: a public mirror carries the code and the Dockerfile, while the
// job spec and the contour variables stay in the internal repository.
func readDeployment(t *testing.T, name string) []byte {
	t.Helper()

	path := filepath.Join("..", "..", "deployments", name)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Skipf("deployments/%s is not in this checkout", name)
	}

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func renderVars(t *testing.T, path string) string {
	t.Helper()

	b := readDeployment(t, filepath.Base(path))

	m := heredoc.FindSubmatch(b)
	require.Len(t, m, 2, "%s: no `config = <<-EOF ... EOF` block", path)

	var out strings.Builder
	for line := range strings.SplitSeq(string(m[1]), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{{-") {
			continue
		}
		out.WriteString(tmplPlaceholder.ReplaceAllString(line, "$1") + "\n")
	}
	return out.String()
}

func decodeVars(t *testing.T, name string) Config {
	t.Helper()
	var cfg Config
	md, err := toml.Decode(renderVars(t, filepath.Join("..", "..", "deployments", name+".vars.hcl")), &cfg)
	require.NoError(t, err)
	// unknown keys are typos: TOML decoding ignores them silently
	assert.Empty(t, md.Undecoded(), "unknown keys")
	return cfg
}

// TestNomadVarsCoverConfig fails when a Config section has no counterpart in a
// deployed config — the way a whole feature ships switched off.
func TestNomadVarsCoverConfig(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	for _, name := range []string{"master", "devel"} {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			md, err := toml.Decode(renderVars(t, filepath.Join("..", "..", "deployments", name+".vars.hcl")), &cfg)
			require.NoError(t, err)
			assert.Empty(t, md.Undecoded(), "unknown keys")

			defined := map[string]bool{}
			for _, k := range md.Keys() {
				defined[strings.ToLower(k[0])] = true
			}

			rt := reflect.TypeFor[Config]()
			for field := range rt.Fields() {
				f := field.Name
				// APIKeys is the one section a contour may leave out, and only
				// where OIDC.Required makes it inert: the two ways in are
				// exclusive, so a key would be refused by the ladder before it
				// was read. Every other section missing is a feature that
				// shipped switched off with nobody saying so.
				if f == "APIKeys" && cfg.OIDC.Required {
					continue
				}
				assert.True(t, defined[strings.ToLower(f)], "section [%s] missing", f)
			}
		})
	}
}

// TestNomadVarsValues guards the settings that must not diverge between the
// two environments or from the job spec.
func TestNomadVarsValues(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	spec := readDeployment(t, "service.nomad.hcl")

	for _, name := range []string{"master", "devel"} {
		t.Run(name, func(t *testing.T) {
			cfg := decodeVars(t, name)

			assert.Equal(t, "0.0.0.0", cfg.Server.Host)
			assert.Contains(t, string(spec), "to = "+"8085", "job port must match Server.Port")
			assert.Equal(t, 8085, cfg.Server.Port)
			assert.True(t, strings.HasPrefix(cfg.Server.BaseURL, "https://"))

			// The canonical URI check is the one this stack fails on most
			// often, and it fails with a valid token in hand. An empty
			// Audience is the one shape that is not a mismatch: it selects
			// the ClientID/azp fallback the verifier keeps for an IdP whose
			// audience mapping is not configured yet, and Config.Validate
			// allows it for the same reason. A contour that names an
			// audience still has to name this one.
			if cfg.OIDC.Audience != "" {
				assert.Equal(t, cfg.Server.BaseURL, cfg.OIDC.Audience,
					"OIDC.Audience must equal Server.BaseURL byte for byte")
			}

			// Repository data lives on the mounted volume: it is a cache worth
			// keeping across deploys.
			assert.Contains(t, string(spec), `destination = "/data"`, "job must mount the data volume")
			for _, p := range []string{cfg.Storage.ReposDir, cfg.Storage.WorktreesDir} {
				assert.True(t, strings.HasPrefix(p, "/data/"), "%q must live on the volume", p)
			}

			// The catalogue carries credentials, so it is rendered into the
			// allocation's tmpfs and never touches the node's disk.
			assert.Equal(t, "/secrets/targets.toml", cfg.Catalog.Path)
			assert.Contains(t, string(spec), `destination = "secrets/targets.toml"`)

			// The catalogue body itself is in git, so it is reviewed like
			// code and cannot drift into an untracked copy on a volume. It
			// travels in the contour's own vars file, wrapped in the same
			// nomadVar block as the config above it: a -var-file may not call
			// functions, but it takes a literal string, and `nomad job run`
			// then reads nothing from the checkout but its two arguments.
			vars := readDeployment(t, name+".vars.hcl")
			// The variable path is matched without its closing quote: a
			// contour may keep the catalogue's tokens task-scoped, under
			// nomad/jobs/ringsrv/<group>/<task>, which the workload identity
			// reads just as well. What is checked is that they come from
			// Nomad Variables under this job and from nowhere else.
			assert.Contains(t, string(vars), `nomadVar "nomad/jobs/ringsrv`, "tokens come from Nomad Variables")

			body := catalogue.FindSubmatch(vars)
			require.Len(t, body, 2, "the vars file must carry the catalogue body")
			assert.Contains(t, string(body[1]), `nomadVar "nomad/jobs/ringsrv`, "the catalogue gets its tokens from Nomad Variables too")
			assert.Contains(t, string(body[1]), "[Roles.", "the block must hold a catalogue, not a path to one")
			assert.Contains(t, string(spec), "data        = var.targets", "the job spec renders the body the contour carries")

			// The audit record is written at Info level, so an instance
			// running without verbose logging keeps working and silently
			// stops recording who called what.
			assert.Contains(t, string(spec), "RINGSRV_VERBOSE = true",
				"audit records are Info-level: without verbose the log loses them")

			assert.Positive(t, cfg.Limits.UpstreamTimeout)
			assert.Positive(t, cfg.Limits.MaxBytes)
			// The database defaults are chosen in the deployed config rather
			// than inherited: a limit nobody wrote down is a limit nobody
			// reviews.
			assert.Positive(t, cfg.Limits.DBTimeout)
			assert.Positive(t, cfg.Limits.DBMaxRows)
			// One way in, and the config names which. Under OIDC.Required a key
			// is refused before it is read, so carrying one is a credential
			// nothing will ever look at — and a block kept "so the template has
			// the variables" is how the last breakage happened: it was emptied
			// rather than removed, and empty is what validation rejects.
			if cfg.OIDC.Required {
				assert.Empty(t, cfg.APIKeys, "keys are inert under OIDC.Required")
			} else {
				assert.NotEmpty(t, cfg.APIKeys, "without OIDC.Required a key is the only way in")
			}
		})
	}
}

// The scope a client is told to ask for has to be the one that carries the
// claim this server keys access on. Nothing connects the two at runtime: the
// authorization server drops a scope it does not have without a word, the token
// comes back without the claim, and every call is a 403 whose message names an
// empty list of roles and an empty list of groups. That is exactly how
// GroupsClaim moved to "entitlements" while scopes_supported still said
// "groups", and the contour stayed broken until somebody read an authorize URL.
//
// The claim and the scope carry the same name on both providers this has met —
// Authentik calls both "entitlements", Keycloak calls both "groups" — which is
// what makes the rule checkable here. If an IdP ever separates them, this test
// is where that gets written down rather than discovered.
func TestAdvertisedScopeCarriesTheGroupsClaim(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	for _, name := range []string{"master", "devel"} {
		t.Run(name, func(t *testing.T) {
			cfg := decodeVars(t, name)
			if !cfg.OIDC.Required {
				return
			}
			require.NotEmpty(t, cfg.OIDC.GroupsClaim, "a contour on OIDC names the claim it keys roles by")
			assert.Contains(t, cfg.OIDC.Scopes, cfg.OIDC.GroupsClaim,
				"scopes_supported must ask for the scope that carries %q, or the claim never arrives",
				cfg.OIDC.GroupsClaim)
		})
	}
}

// TestCatalogExampleMatchesProd — the prod catalogue is carried twice: the bytes
// that deploy are in deployments/master.vars.hcl, and cfg/targets.toml.dist is
// the readable example the docs point at, because a 40 KB heredoc is not how
// anyone learns the format. Two copies of the same thing drift, and this one
// drifts invisibly: the example goes on validating alone.
func TestCatalogExampleMatchesProd(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	example, err := os.ReadFile(filepath.Join(targettest.Root(t), targettest.Example))
	require.NoError(t, err)

	assert.Equal(t, string(example), string(targettest.Body(t, targettest.Prod)),
		"%s and the `targets` heredoc of deployments/master.vars.hcl must match byte for byte: editing the prod catalogue means editing both",
		targettest.Example)
}

// TestEnvsDoNotShareContour — the whole point of two instances (D25) is that a
// dev one can never answer about prod. The contour itself is named in the
// catalogue (target.Catalog.Env), which lives on the volume; what the deploy
// configs must keep apart is the identity of the two instances.
func TestEnvsDoNotShareContour(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	prod, devel := decodeVars(t, "master"), decodeVars(t, "devel")

	assert.NotEqual(t, prod.Server.BaseURL, devel.Server.BaseURL)
	assert.NotEqual(t, prod.OIDC.Audience, devel.OIDC.Audience)
	assert.True(t, prod.OIDC.Required, "prod must not accept api-keys")
	assert.False(t, prod.Server.IsDevel)
}

// TestDistConfigsDecode keeps the sample configs parseable — they are copied
// verbatim by `make init` and by hand for prod.
// decodeDist reads a shipped instance config, or skips when that file is not in
// this checkout: a public mirror carries local.toml.dist and docker.toml.dist
// but none of the contour ones.
func decodeDist(t *testing.T, name string) Config {
	t.Helper()

	path := filepath.Join("..", "..", "cfg", name)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Skipf("cfg/%s is not in this checkout", name)
	}

	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	require.NoError(t, err)
	assert.Empty(t, md.Undecoded(), "unknown keys")
	return cfg
}

func TestDistConfigsDecode(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	for _, name := range []string{"local.toml.dist", "prod.toml.dist"} {
		t.Run(name, func(t *testing.T) {
			decodeDist(t, name)
		})
	}
}

// Validate is for the mistakes that do not fail on their own: a zero limit
// used to mean "no limit", a negative one meant the same, and OIDC.Required
// without an issuer was caught only once the App was half built.
func TestConfigValidate(t *testing.T) {
	t.Parallel()
	valid := func() Config {
		var c Config
		c.Server.Port = 8075
		c.Catalog.Path = "cfg/targets.toml"
		return c
	}

	t.Run("zero limits get defaults, and the log hears about it", func(t *testing.T) {
		t.Parallel()
		c := valid()
		applied, err := c.Validate()
		require.NoError(t, err)
		assert.Equal(t, defaultUpstreamTimeout, c.Limits.UpstreamTimeout)
		assert.Equal(t, defaultJQTimeout, c.Limits.JQTimeout)
		assert.Equal(t, defaultMaxBytes, c.Limits.MaxBytes)
		assert.Equal(t, defaultMaxConcurrent, c.Limits.MaxConcurrent)
		assert.Equal(t, defaultDBTimeout, c.Limits.DBTimeout)
		assert.Equal(t, defaultDBMaxRows, c.Limits.DBMaxRows)
		assert.Len(t, applied, 6, "each default is named once")
	})

	t.Run("chosen limits are kept", func(t *testing.T) {
		t.Parallel()
		c := valid()
		c.Limits.UpstreamTimeout, c.Limits.JQTimeout, c.Limits.MaxBytes, c.Limits.MaxConcurrent = time.Second, time.Second, 1, 1
		c.Limits.DBTimeout, c.Limits.DBMaxRows = time.Second, 1
		applied, err := c.Validate()
		require.NoError(t, err)
		assert.Empty(t, applied)
		assert.Equal(t, time.Second, c.Limits.UpstreamTimeout)
	})

	cases := map[string]struct {
		mutate func(c *Config)
		want   []string
	}{
		"port is required":              {func(c *Config) { c.Server.Port = 0 }, []string{"Server.Port"}},
		"catalogue path is required":    {func(c *Config) { c.Catalog.Path = "" }, []string{"Catalog.Path"}},
		"required oidc needs an issuer": {func(c *Config) { c.OIDC.Required = true }, []string{"OIDC.Issuer"}},
		"an issuer needs a client id":   {func(c *Config) { c.OIDC.Issuer = "https://idp" }, []string{"OIDC.ClientID"}},
		"audience must equal base url": {func(c *Config) {
			c.OIDC.Issuer, c.OIDC.ClientID = "https://idp", "x"
			c.Server.BaseURL, c.OIDC.Audience = "https://a", "https://b"
		}, []string{"OIDC.Audience"}},
		"negative disk budget":       {func(c *Config) { c.Storage.MaxDiskBytes = -1 }, []string{"Storage.MaxDiskBytes"}},
		"engine needs a repos dir":   {func(c *Config) { c.Storage.ASTEngine = true }, []string{"Storage.ReposDir"}},
		"repos need a worktrees dir": {func(c *Config) { c.Storage.ReposDir = "/data/repos" }, []string{"Storage.WorktreesDir"}},
		"negative limit is not off":  {func(c *Config) { c.Limits.UpstreamTimeout = -time.Second }, []string{"Limits.UpstreamTimeout"}},
		"negative db timeout":        {func(c *Config) { c.Limits.DBTimeout = -time.Second }, []string{"Limits.DBTimeout"}},
		"negative db rows":           {func(c *Config) { c.Limits.DBMaxRows = -1 }, []string{"Limits.DBMaxRows"}},
		"negative rate limit":        {func(c *Config) { c.RateLimit.PerUserRPM = -1 }, []string{"RateLimit.PerUserRPM"}},
		"api key without a hash":     {func(c *Config) { c.APIKeys = []auth.Key{{UserID: "x"}} }, []string{"ApiKeys[0]: KeyHash"}},
		"every finding at once": {func(c *Config) {
			c.Server.Port = 0
			c.OIDC.Required = true
		}, []string{"Server.Port", "OIDC.Issuer"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tc.mutate(&c)
			_, err := c.Validate()
			require.Error(t, err)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// Every shipped config validates: the samples are what `make init` and the
// Nomad job start from, and a sample that fails Validate teaches the wrong
// shape.
func TestShippedConfigsValidate(t *testing.T) {
	targettest.SkipUnlessConfigChecks(t)
	t.Parallel()
	for _, name := range []string{"local.toml.dist", "prod.toml.dist", "docker.toml.dist"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := decodeDist(t, name)
			_, err := cfg.Validate()
			require.NoError(t, err)
		})
	}
	for _, name := range []string{"master", "devel"} {
		t.Run(name+".vars.hcl", func(t *testing.T) {
			t.Parallel()
			cfg := decodeVars(t, name)
			_, err := cfg.Validate()
			require.NoError(t, err)
		})
	}
}
