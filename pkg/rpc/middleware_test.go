package rpc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/zenrpc/v2"
)

// JSON-RPC 2.0 caller errors are not internal; server errors and 500 are.
func TestIsInternal(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]bool{
		zenrpc.ParseError:              false,
		zenrpc.InvalidRequest:          false,
		zenrpc.MethodNotFound:          false,
		zenrpc.InvalidParams:           false,
		zenrpc.InternalError:           true,
		zenrpc.ServerError:             true,
		-32099:                         true,
		http.StatusInternalServerError: true,
		http.StatusBadRequest:          false,
	} {
		assert.Equal(t, want, isInternal(&zenrpc.Error{Code: code}), "code %d", code)
	}
}

// An unknown method answers -32601 "Method not found", not "Internal error".
// The method is made up on purpose, under a namespace that exists: this used to
// be resources/templates/list, until mcpkit learned to answer it.
func TestMCP_UnknownMethodIsNotInternal(t *testing.T) {
	t.Parallel()
	srv := newMCPTestServer(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"resources/nonexistent"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Error *zenrpc.Error `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Error, rr.Body.String())
	assert.Equal(t, zenrpc.MethodNotFound, resp.Error.Code)
	assert.Equal(t, "Method not found", resp.Error.Message)
}
