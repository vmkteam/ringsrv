package rpc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/md"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/mcp"
)

// The MCP surface is exercised end to end — through the HTTP transport that
// rewrites tools/list into tools.list — because that rewrite is exactly the
// part a unit test on the service would skip.
func newMCPTestServer(t *testing.T) *mcpkit.Server {
	t.Helper()
	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)

	return mcpkit.NewServer(NewMCP(MCPDeps{
		Info:    mcp.ServerInfo{Name: "ringsrv", Version: "test"},
		Docs:    lib,
		Targets: testCatalog(t),
		Logger:  embedlog.Logger{},
		IsDevel: true,
	}), embedlog.Logger{})
}

// call sends one JSON-RPC request and returns the `result` object.
func call(t *testing.T, srv *mcpkit.Server, method string, params any, protocolVersion string) map[string]any {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	if protocolVersion != "" {
		req.Header.Set("Mcp-Protocol-Version", protocolVersion)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Result map[string]any  `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Nil(t, resp.Error, "%s: %s", method, resp.Error)
	return resp.Result
}

// An older client must keep working: we accept its initialize and answer in the
// revision it asked for, without ever issuing a session id.
func TestMCP_InitializeEchoesProtocolVersion(t *testing.T) {
	t.Parallel()
	srv := newMCPTestServer(t)

	res := call(t, srv, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	}, "2025-06-18")
	assert.Equal(t, "2025-06-18", res["protocolVersion"])

	// 2025-11-25 is answered exactly now: mcpkit speaks it, where the transport
	// this replaced did not and had to answer below. The rule it answers by is
	// unchanged — never above what was asked, because Claude Desktop asks for
	// 2025-11-25 and drops a server that replies with something newer.
	res = call(t, srv, "initialize", map[string]any{"protocolVersion": "2025-11-25"}, "")
	assert.Equal(t, mcp.Version20251125, res["protocolVersion"])

	// 2025-03-26 left the supported list with mcpkit: a client of that revision
	// may send a batch, and batches are refused here. Anything below our oldest
	// gets the oldest.
	res = call(t, srv, "initialize", map[string]any{"protocolVersion": "1999-01-01"}, "")
	assert.Equal(t, mcp.Version20250618, res["protocolVersion"], "older than anything we speak")

	info, ok := res["serverInfo"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "ringsrv", info["name"])
	assert.Contains(t, res["instructions"], "api_call")
}

// What the handshake declares, spelt out. A capability is a promise to answer,
// and the three namespaces are registered, so all three are there — but every
// flag inside them is false, and that is worth pinning rather than inferring
// from a zero value somebody wrote.
//
// listChanged said true until this service moved onto mcpkit, and it was a
// promise it could not keep: nothing here pushes a notification, so a client
// that believed it waited forever. The flags are false because they are false,
// and a change back is a change to the wire — this test is where it has to be
// argued.
func TestMCP_InitializeDeclaresWhatItCanKeep(t *testing.T) {
	t.Parallel()
	srv := newMCPTestServer(t)

	res := call(t, srv, "initialize", map[string]any{"protocolVersion": "2025-06-18"}, "2025-06-18")
	caps, ok := res["capabilities"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, map[string]any{
		"tools":     map[string]any{"listChanged": false},
		"resources": map[string]any{"subscribe": false, "listChanged": false},
		"prompts":   map[string]any{"listChanged": false},
	}, caps, "three namespaces, no promise this server cannot keep")
}

func TestMCP_NoSessionIsIssued(t *testing.T) {
	t.Parallel()
	srv := newMCPTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Empty(t, rr.Header().Get("Mcp-Session-Id"), "the server is stateless")

	// DELETE is session teardown; a stateless server has nothing to tear down.
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/mcp", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestMCP_ToolsList(t *testing.T) {
	t.Parallel()
	res := call(t, newMCPTestServer(t), "tools/list", nil, "")

	tools, ok := res["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 3, "api_call, repo_map and help")

	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ToolAPICall, tool["name"])
	assert.Contains(t, tool["description"], "prom —")
	assert.NotNil(t, tool["inputSchema"])
	assert.NotNil(t, tool["annotations"])
}

// Resources are the only way this knowledge reaches Claude Desktop, where
// skills and CLAUDE.md are not read.
func TestMCP_ResourcesListAndRead(t *testing.T) {
	t.Parallel()
	srv := newMCPTestServer(t)

	res := call(t, srv, "resources/list", nil, "")
	items, ok := res["resources"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, items)

	uris := make([]string, 0, len(items))
	for _, it := range items {
		m, isMap := it.(map[string]any)
		require.True(t, isMap)
		uri, isStr := m["uri"].(string)
		require.True(t, isStr)
		uris = append(uris, uri)
	}
	require.Contains(t, uris, "ringsrv://targets/prom.md")

	read := call(t, srv, "resources/read", map[string]any{"uri": "ringsrv://targets/prom.md"}, "")
	contents, ok := read["contents"].([]any)
	require.True(t, ok)
	require.Len(t, contents, 1)
	first, ok := contents[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, doc.MimeMarkdown, first["mimeType"])
	assert.Contains(t, first["text"], "query_range")
}

func TestMCP_PromptsListAndGet(t *testing.T) {
	t.Parallel()
	srv := newMCPTestServer(t)

	res := call(t, srv, "prompts/list", nil, "")
	prompts, ok := res["prompts"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, prompts)
	first, ok := prompts[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "errors", first["name"])

	got := call(t, srv, "prompts/get", map[string]any{
		"name":      "errors",
		"arguments": map[string]string{"service": "apisrv", "period": "24h"},
	}, "")
	messages, ok := got["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)
	msg, ok := messages[0].(map[string]any)
	require.True(t, ok)
	content, ok := msg["content"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, content["text"], "apisrv", "placeholders are substituted")
	assert.NotContains(t, content["text"], "{{service}}")
}

// The log line for a call carries the method and the outcome, never the
// arguments: for tools/call they are the body of a write on its way to a
// tracker, and a log every Loki reader can open must not hold it.
// Not parallel: the logger is built while stdout is swapped for a pipe.
func TestMCP_CallLogHasNoParams(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	logger := embedlog.NewLogger(true, true)
	os.Stdout = orig

	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)
	srv := mcpkit.NewServer(NewMCP(MCPDeps{
		Info:    mcp.ServerInfo{Name: "ringsrv", Version: "test"},
		Docs:    lib,
		Targets: testCatalog(t),
		Logger:  logger,
		IsDevel: true,
	}), embedlog.Logger{})

	const secret = "BODY-THAT-MUST-NOT-BE-LOGGED"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"api_call","arguments":{"target":"prom","method":"GET","path":"/api/v1/query","body":"` + secret + `"}}}`
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	require.NoError(t, w.Close())
	raw, err := io.ReadAll(r)
	require.NoError(t, err)

	assert.Contains(t, string(raw), "tools.call", "the call is logged")
	assert.NotContains(t, string(raw), secret, "its arguments are not")
}
