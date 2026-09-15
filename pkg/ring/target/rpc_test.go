package target

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rpcTOML = `
Env = "prod"

[Profiles.topsrv]
Description  = "prod · topsrv"
BaseURL      = "https://topsrv.example.com"
Headers      = ["Authorization: Bearer token"]
AllowMethods = ["POST"]
AllowPaths   = ["POST ^/api/v1/rpc/$"]
ReadOnlyPost = true
AllowRPCMethods = ["meta.whoami", "host.list", "metric.queryRange"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["topsrv"]
`

// On an API that lives behind one path, the method in the body is the
// allowlist. Everything the allowlist cannot read, it refuses.
func TestCheckRPC(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(rpcTOML))
	require.NoError(t, err)
	p := c.Profiles["topsrv"]
	require.True(t, p.RPCGuarded())

	allowed := []string{
		`{"jsonrpc":"2.0","id":1,"method":"host.list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"Host.List"}`, // the servers do not care about case, so neither do we
		`{"method":"meta.whoami"}`,                      // a notification is still a call
		`[{"jsonrpc":"2.0","id":1,"method":"host.list"},{"jsonrpc":"2.0","id":2,"method":"metric.queryRange","params":{"query":"up"}}]`,
		"  \n" + `{"method":"host.list"}` + "\n",
	}
	for _, body := range allowed {
		require.NoError(t, p.CheckRPC(body), body)
	}

	refused := map[string]string{
		`{"jsonrpc":"2.0","id":1,"method":"alert.silence"}`:   `"alert.silence" is not allowed`,
		`[{"method":"host.list"},{"method":"alert.silence"}]`: `"alert.silence" is not allowed`,
		`[{"method":"host.list"},{"params":{}}]`:              "batch element 1 has no method",
		`{"jsonrpc":"2.0","id":1,"params":{}}`:                "has no method",
		`{"method":""}`:                                       "has no method",
		`{"method":5}`:                                        "not a JSON-RPC request",
		`[]`:                                                  "empty batch",
		`[1,2]`:                                               "not a JSON-RPC batch",
		`"host.list"`:                                         "want an object or a batch array",
		``:                                                    "body is empty",
		`{"method":"host.list"`:                               "not a JSON-RPC request",
	}
	for body, want := range refused {
		err := p.CheckRPC(body)
		require.Error(t, err, body)
		assert.Contains(t, err.Error(), want, body)
	}
}

// A profile without the list is not guarded: the check is a no-op so the
// call site does not have to ask first.
func TestCheckRPC_Unguarded(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(validTOML))
	require.NoError(t, err)
	p := c.Profiles["prom"]
	assert.False(t, p.RPCGuarded())
	require.NoError(t, p.CheckRPC(""))
	require.NoError(t, p.CheckRPC(`{"method":"anything"}`))
}

// The list is checked at load: a method list on a GET-only profile, a
// duplicate, and a state-changing method on a profile that promises to read.
func TestParse_RejectsBrokenRPCLists(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ toml, want string }{
		"rpc methods on a GET-only profile": {
			toml: strings.Replace(validTOML, `AllowPaths   = ["^/api/v1/query$"]`, "AllowPaths   = [\"^/api/v1/query$\"]\nAllowRPCMethods = [\"host.list\"]", 1),
			want: "GET only",
		},
		"time in body on a GET-only profile": {
			toml: strings.Replace(validTOML, `AllowPaths   = ["^/api/v1/query$"]`, "AllowPaths   = [\"^/api/v1/query$\"]\nTimeBodyParams = [\"params.start\"]\nTimeFormat = \"unix\"", 1),
			want: "GET only",
		},
		"time in body without a format": {
			toml: replaceLine(rpcTOML, "AllowRPCMethods", "TimeBodyParams = [\"params.start\"]"),
			want: "TimeFormat is empty",
		},
		"empty path segment": {
			toml: replaceLine(rpcTOML, "AllowRPCMethods", "TimeBodyParams = [\"params..start\"]\nTimeFormat = \"unix\""),
			want: "dotted path",
		},
		"format nobody implements": {
			toml: replaceLine(rpcTOML, "AllowRPCMethods", "TimeBodyParams = [\"params.filter.from:iso8601\"]\nTimeFormat = \"unix\""),
			want: "want one of",
		},
		"duplicate method": {
			toml: replaceLine(rpcTOML, "AllowRPCMethods", "AllowRPCMethods = [\"host.list\", \"Host.List\"]"),
			want: "twice",
		},
		"write method on a read-only profile": {
			toml: replaceLine(rpcTOML, "AllowRPCMethods", "AllowRPCMethods = [\"host.list\", \"alert.silence\"]"),
			want: "changes state",
		},
		"blank method": {
			toml: replaceLine(rpcTOML, "AllowRPCMethods", "AllowRPCMethods = [\"host.list\", \" \"]"),
			want: "not a method name",
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

// The same method on a write profile is what the flag is for: the barrier
// there is intent and budget, not the list.
func TestParse_WriteProfileMayNameWriteRPC(t *testing.T) {
	t.Parallel()
	toml := replaceLine(rpcTOML, "ReadOnlyPost", "Write = true")
	toml = replaceLine(toml, "AllowRPCMethods", "AllowRPCMethods = [\"alert.silence\"]")
	toml += "AllowWrite  = true\n"
	_, err := Parse([]byte(toml))
	require.NoError(t, err)
}
