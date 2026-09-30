package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/md"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/auth"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/mcptest"
	"github.com/vmkteam/mcpkit/ratelimit"
)

// The checks the migration plan ran after every step, as a table instead of a
// session with curl, so the next library upgrade has something to be checked by.
//
// Everything goes through the assembled endpoint — echo, the auth ladder, the
// limiter, the audit — because the wiring outside the namespaces is exactly what
// a unit test on the service skips.

const conformanceKey = "conformance-token"

// serveMCP builds /mcp the way Run does and hands back a client speaking the
// era asked for. mutate is where a test asks for the configuration its refusal
// needs — api keys for a 401, a one-per-minute budget for a 429.
func serveMCP(t *testing.T, era mcptest.Era, mutate func(c *Config), opts ...mcptest.Option) *mcptest.Client {
	t.Helper()

	a := mcpApp(t, func(c *Config) {
		// No principal and no OIDC: the dev ladder grants the catalogue's
		// read-only profiles, which is what makes the common set callable
		// without inventing a role for the test.
		c.Server.IsDevel = true
		if mutate != nil {
			mutate(c)
		}
	})

	return mcptest.New(t, a.echo, append([]mcptest.Option{
		mcptest.WithEra(era),
		mcptest.WithPath("/mcp"),
	}, opts...)...)
}

// The same catalogue comes back whichever era asks for it. Running
// the whole set twice is the only check of "both eras on one endpoint" that
// cannot pass by accident.
func TestMCPConformance(t *testing.T) {
	t.Parallel()
	for _, era := range []mcptest.Era{mcptest.Modern, mcptest.Legacy} {
		t.Run(string(era), func(t *testing.T) {
			t.Parallel()
			c := serveMCP(t, era, nil)

			// The composition, not a golden file: a key that stops being
			// sent is the regress this guards, and the values of the ones that
			// stay are checked where they are decided.
			t.Run("tools.list", func(t *testing.T) {
				list := c.Tools(t)
				require.NotEmpty(t, list.Tools)

				names := make([]string, len(list.Tools))
				for i, tool := range list.Tools {
					names[i] = tool.Name
					assert.NotEmpty(t, tool.Description, "%s describes itself", tool.Name)
					assert.NotEmpty(t, tool.InputSchema, "%s has a schema", tool.Name)
					require.NotNil(t, tool.Annotations, "%s annotates itself", tool.Name)
					// All four spelt out: an absent destructiveHint defaults to
					// true in the spec, so a tool that says nothing claims to be
					// destructive.
					assert.NotNil(t, tool.Annotations.ReadOnlyHint, "%s: readOnlyHint", tool.Name)
					assert.NotNil(t, tool.Annotations.DestructiveHint, "%s: destructiveHint", tool.Name)
					assert.NotNil(t, tool.Annotations.OpenWorldHint, "%s: openWorldHint", tool.Name)
					assert.NotEmpty(t, tool.Annotations.Title, "%s: title", tool.Name)
				}
				assert.Contains(t, names, "api_call")
				assert.Contains(t, names, "help")
				assert.Equal(t, "help", names[len(names)-1], "help is last, where the model reads it after the tools it is about")

				// Private because the list is built from the caller's role:
				// public would let a cache hand one caller the tools of another.
				assert.Equal(t, mcp.CacheScopePrivate, list.CacheScope)
				assert.Empty(t, list.NextCursor, "paging is off, so one page")
			})

			// help is the call that needs no upstream: it answers from the
			// markdown compiled into the binary.
			t.Run("tools.call", func(t *testing.T) {
				res := c.CallTool(t, "help", map[string]any{})
				require.False(t, res.IsError, res.Content[0].Text)
				require.Len(t, res.Content, 1)
				assert.Contains(t, res.Content[0].Text, `"sheets"`)
				assert.Contains(t, res.Content[0].Text, `"env"`, "the contour travels on every top-level answer (D25)")
			})

			// A refusal is an envelope, not a transport error: the model is meant
			// to read it and pick another tool.
			t.Run("tools.call refuses inside a result", func(t *testing.T) {
				res := c.CallTool(t, "no_such_tool", map[string]any{})
				assert.True(t, res.IsError)
				assert.Contains(t, res.Content[0].Text, "BadArgs", "this service answers in its own codes")
			})

			// The URIs are quoted in the instructions and in written-down
			// scenarios, so they are a contract and not an implementation detail.
			t.Run("resources", func(t *testing.T) {
				list := c.Resources(t)
				require.NotEmpty(t, list.Resources)

				uris := make([]string, len(list.Resources))
				for i, r := range list.Resources {
					uris[i] = r.URI
					assert.True(t, len(r.URI) > len(md.URIScheme) && r.URI[:len(md.URIScheme)] == md.URIScheme,
						"%s carries this service's scheme", r.URI)
				}
				assert.Contains(t, uris, "ringsrv://tools/db.md")
				assert.Contains(t, uris, "ringsrv://tools/code.md")

				data := c.ReadResource(t, "ringsrv://tools/db.md")
				require.Len(t, data.Contents, 1)
				assert.NotEmpty(t, data.Contents[0].Text)
			})

			t.Run("prompts", func(t *testing.T) {
				list := c.Prompts(t)
				require.NotEmpty(t, list.Prompts)

				p := list.Prompts[0]
				args := make(map[string]string, len(p.Arguments))
				for _, a := range p.Arguments {
					args[a.Name] = "apisrv"
				}
				got := c.GetPrompt(t, p.Name, args)
				require.NotEmpty(t, got.Messages)
				assert.Equal(t, mcp.RoleUser, got.Messages[0].Role)
				assert.NotContains(t, got.Messages[0].Content.Text, "{{", "every placeholder was filled")
			})
		})
	}
}

// The half that differs: each era opens the conversation its own way, and
// both are answered. A modern client never sends initialize at all.
func TestMCPConformance_BothHandshakes(t *testing.T) {
	t.Parallel()

	t.Run("server.discover answers the modern era", func(t *testing.T) {
		t.Parallel()
		got := serveMCP(t, mcptest.Modern, nil).Discover(t)
		assert.Contains(t, got.SupportedVersions, mcp.ProtocolVersion)
		require.NotNil(t, got.Capabilities.Tools)
		require.NotNil(t, got.Meta.ServerInfo)
		assert.Equal(t, "ringsrv", got.Meta.ServerInfo.Name)
	})

	t.Run("initialize answers the legacy era", func(t *testing.T) {
		t.Parallel()
		got := serveMCP(t, mcptest.Legacy, nil).Initialize(t)
		assert.Equal(t, "ringsrv", got.ServerInfo.Name)
		assert.Contains(t, got.Instructions, "api_call", "the instructions name the tools")
		// Declared, and no more than this server can keep: nothing here pushes a
		// notification, so listChanged stays false in all three.
		require.NotNil(t, got.Capabilities.Tools)
		assert.False(t, got.Capabilities.Tools.ListChanged)
		require.NotNil(t, got.Capabilities.Resources)
		assert.False(t, got.Capabilities.Resources.Subscribe)
		require.NotNil(t, got.Capabilities.Prompts)
	})
}

// The refusals, which is the half of the endpoint that has no namespace
// behind it: the auth ladder, the limiter and the transport's own checks.
func TestMCPConformance_Refusals(t *testing.T) {
	t.Parallel()

	// A 401 that does not say how to authenticate sends a client away for good.
	t.Run("no key is 401 with WWW-Authenticate", func(t *testing.T) {
		t.Parallel()
		c := serveMCP(t, mcptest.Modern, withAPIKey)

		res := c.Call(t, "tools/list", nil)
		assert.Equal(t, http.StatusUnauthorized, res.Status)
		assert.NotEmpty(t, res.Header.Get("WWW-Authenticate"), "and it says how")
	})

	// A 429 without Retry-After is a client that retries immediately and is
	// refused again, and one without a JSON-RPC error in it reaches the model
	// as "server unavailable" rather than as a refused call.
	t.Run("over the budget is 429 with Retry-After", func(t *testing.T) {
		t.Parallel()
		c := serveMCP(t, mcptest.Modern, func(cfg *Config) {
			withAPIKey(cfg)
			cfg.RateLimit = ratelimit.Config{PerUserRPM: 1}
		}, mcptest.WithHeader("Authorization", "Bearer "+conformanceKey))

		require.Equal(t, http.StatusOK, c.Call(t, "tools/list", nil).Status, "the first one is inside the budget")

		var refused mcptest.Response
		for range 5 {
			if refused = c.Call(t, "tools/list", nil); refused.Status == http.StatusTooManyRequests {
				break
			}
		}
		require.Equal(t, http.StatusTooManyRequests, refused.Status)
		assert.NotEmpty(t, refused.Header.Get("Retry-After"), "and it says how long")
		require.NotNil(t, refused.Error, "an answer to the call: %s", refused.Body)
		assert.Equal(t, mcp.CodeRateLimited, refused.Error.Code)
		assert.Contains(t, string(refused.Error.Data), `"reason":"rpm"`)
	})

	// The modern era mirrors the method into a header so a proxy can route
	// without parsing the body; a server that did not compare them would let the
	// two disagree, which is the hole the header would open.
	t.Run("a header that disagrees with the body is refused", func(t *testing.T) {
		t.Parallel()
		c := serveMCP(t, mcptest.Modern, nil, mcptest.WithHeader(mcp.HeaderMethod, "resources/list"))

		res := c.Call(t, "tools/list", nil)
		assert.Equal(t, http.StatusBadRequest, res.Status)
		require.NotNil(t, res.Error)
		assert.Equal(t, mcp.CodeHeaderMismatch, res.Error.Code)
	})

	// A revision this server does not speak is refused rather than quietly
	// served under another one.
	t.Run("a revision outside the list is refused", func(t *testing.T) {
		t.Parallel()
		c := serveMCP(t, mcptest.Modern, nil, mcptest.WithProtocolVersion("2020-01-01"))

		res := c.Call(t, "tools/list", nil)
		assert.Equal(t, http.StatusBadRequest, res.Status)
		require.NotNil(t, res.Error)
		assert.Equal(t, mcp.CodeUnsupportedProtocolVersion, res.Error.Code)
	})

	// Only the modern era gets a 404 here. A legacy client — mcpurl and Claude
	// Desktop are both — still gets its error inside a 200, because that is what
	// the revision it speaks says an unknown method looks like.
	t.Run("an unknown method is 404 for modern and 200 for legacy", func(t *testing.T) {
		t.Parallel()

		modern := serveMCP(t, mcptest.Modern, nil).Call(t, "nope/nope", nil)
		assert.Equal(t, http.StatusNotFound, modern.Status)
		require.NotNil(t, modern.Error)

		legacy := serveMCP(t, mcptest.Legacy, nil).Call(t, "nope/nope", nil)
		assert.Equal(t, http.StatusOK, legacy.Status)
		require.NotNil(t, legacy.Error, "the error is in the envelope, where a legacy client reads it")
	})
}

// withAPIKey turns the ladder to api keys: a key with groups the catalogue
// knows, so that what a test sees is the refusal it asked for and not a role
// that grants nothing.
func withAPIKey(c *Config) {
	c.APIKeys = []auth.Key{{
		UserID:  "conformance",
		KeyHash: hashKey(conformanceKey),
		Groups:  []string{"ringsrv-users"},
	}}
}

// hashKey is what the catalogue stores for a token: the sha256 of the plaintext
// as hex, which is what the store compares against.
func hashKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
