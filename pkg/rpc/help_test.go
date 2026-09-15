package rpc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/mcp"
)

func decodeHelp(t *testing.T, res mcp.ToolCallResult) HelpResult {
	t.Helper()
	require.False(t, res.IsError, res.Content[0].Text)
	var out HelpResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	return out
}

// helpOne reads the single sheet of a one-name call.
func helpOne(t *testing.T, res mcp.ToolCallResult) HelpItem {
	t.Helper()
	out := decodeHelp(t, res)
	require.Len(t, out.Results, 1)
	require.Nil(t, out.Results[0].Error, "expected a sheet, got a refusal")
	return out.Results[0]
}

func sheetNamesOf(out HelpResult) []string {
	names := make([]string, len(out.Sheets))
	for i, s := range out.Sheets {
		names[i] = s.Name
	}
	return names
}

// help is the cheat sheets as a tool result, the one channel that reaches
// Claude Desktop. It lists and reads exactly what the role may call:
// a target outside the role is unknown here too, and the list in that
// refusal names only what is readable.
func TestCall_Help(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)
	viewer := ctxWithGroups("ringsrv-users")

	t.Run("list follows the role", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(viewer, ToolHelp, nil)
		require.NoError(t, err)
		out := decodeHelp(t, res)
		assert.Equal(t, "prod", out.Env)
		names := sheetNamesOf(out)
		assert.Contains(t, names, "prom")
		assert.Contains(t, names, "topsrv")
		assert.NotContains(t, names, "gitlab", "not a viewer's target")
		assert.NotContains(t, names, sheetCode, "no code tools, no code sheet")
		assert.NotContains(t, names, sheetDB)
		for _, sh := range out.Sheets {
			assert.NotEmpty(t, sh.Description, "the frontmatter description travels with the name")
		}

		res, err = s.Call(ctxWithGroups("ringsrv-developers"), ToolHelp, map[string]any{})
		require.NoError(t, err)
		names = sheetNamesOf(decodeHelp(t, res))
		assert.Contains(t, names, "gitlab")
		assert.Contains(t, names, sheetCode)
		assert.Contains(t, names, sheetDB)
	})

	t.Run("one sheet by name", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(viewer, ToolHelp, map[string]any{"names": []any{"prom"}})
		require.NoError(t, err)
		out := decodeHelp(t, res)
		require.Len(t, out.Results, 1)
		assert.Equal(t, "prom", out.Results[0].Name)
		assert.Contains(t, out.Results[0].Text, "/api/v1/query")
		assert.Empty(t, out.Sheets, "the sheets asked for or the list of them, never both")
	})

	// The whole point of the list: a step that needs three sheets asks once.
	t.Run("several sheets in one call", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(viewer, ToolHelp, map[string]any{"names": []any{"prom", "nope", "topsrv"}})
		require.NoError(t, err)
		out := decodeHelp(t, res)
		require.Len(t, out.Results, 3)
		assert.Contains(t, out.Results[0].Text, "/api/v1/query")
		assert.Equal(t, ErrCodeSheetUnknown, out.Results[1].Error.Code,
			"a name nobody has a sheet for is that item's refusal")
		assert.NotEmpty(t, out.Results[2].Text, "and the sheets after it still answer")
		for i, item := range out.Results {
			assert.Equal(t, i, item.Index, "answers come back in the order asked")
		}

		res, _ = s.Call(viewer, ToolHelp, map[string]any{"names": []any{"prom", "prom", "prom", "prom", "prom", "prom", "prom"}})
		assert.Equal(t, ErrCodeBadArgs, decodeErr(t, res).Code, "an oversized list is refused, not trimmed")
	})

	t.Run("a target outside the role is unknown, and the list does not name it", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(viewer, ToolHelp, map[string]any{"names": []any{"gitlab"}})
		require.NoError(t, err)
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeSheetUnknown, e.Code)
		assert.Contains(t, e.Sheets, "prom", "the error is the documentation")
		assert.NotContains(t, e.Sheets, "gitlab")

		res, _ = s.Call(viewer, ToolHelp, map[string]any{"names": []any{"nope"}})
		assert.Equal(t, ErrCodeSheetUnknown, decodeErr(t, res).Code)
	})

	t.Run("a write profile reads the sheet of its read twin", func(t *testing.T) {
		t.Parallel()
		ws, writer := testToolsWithWrite(t), ctxWithGroups("ringsrv-writers")
		res, err := ws.Call(writer, ToolHelp, map[string]any{"names": []any{"youtrack-rw"}})
		require.NoError(t, err)
		out := helpOne(t, res)
		assert.Equal(t, "youtrack-rw", out.Name, "answered under the name that was asked")
		assert.Contains(t, out.Text, "/api/issues")

		res, err = ws.Call(writer, ToolHelp, nil)
		require.NoError(t, err)
		names := sheetNamesOf(decodeHelp(t, res))
		assert.Contains(t, names, "youtrack")
		assert.NotContains(t, names, "youtrack-rw", "one sheet, one row")
	})

	t.Run("no role, no help", func(t *testing.T) {
		t.Parallel()
		res, err := s.Call(ctxWithGroups("nobody"), ToolHelp, nil)
		require.NoError(t, err)
		assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)
	})
}

// A refusal by method, path, query parameter or RPC method carries the
// target's sheet: the refusal is when the sheet is needed, and it arrives
// as a tool result in every client.
func TestCall_RefusalCarriesSheet(t *testing.T) {
	t.Parallel()
	s := testTools(t, false)
	ctx := ctxWithGroups("ringsrv-users")

	res, _ := s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "prom", "method": "GET", "path": "/api/v1/admin/tsdb"}))
	e := decodeErr(t, res)
	assert.Equal(t, ErrCodePathNotAllowed, e.Code)
	assert.Contains(t, e.Help, "Разрешённые пути")
	assert.Contains(t, e.Help, "/api/v1/query")

	res, _ = s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "prom", "method": "PUT", "path": "/api/v1/query"}))
	e = decodeErr(t, res)
	assert.Equal(t, ErrCodeMethodNotAllowed, e.Code)
	assert.Contains(t, e.Help, "/api/v1/query")

	res, _ = s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "prom", "method": "GET", "path": "/api/v1/query?query=up&pretty=1"}))
	e = decodeErr(t, res)
	assert.Equal(t, ErrCodeQueryParamNotAllowed, e.Code)
	assert.Contains(t, e.Help, "Параметры запроса")

	res, _ = s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "topsrv", "method": "POST", "path": "/api/v1/rpc/", "body": `{"jsonrpc":"2.0","id":1,"method":"alert.silence"}`}))
	e = decodeErr(t, res)
	assert.Equal(t, ErrCodeRPCMethodNotAllowed, e.Code)
	assert.Contains(t, e.Help, "alert.list")

	// A refusal that is not about a target has no sheet to carry.
	res, _ = s.Call(ctx, ToolAPICall, apiArgs(map[string]any{"target": "graphana", "method": "GET", "path": "/api/search"}))
	e = decodeErr(t, res)
	assert.Equal(t, ErrCodeTargetUnknown, e.Code)
	assert.Empty(t, e.Help)
}

// Without the library — a test build — help is still listed but has nothing
// to say, and a refusal carries no sheet rather than failing.
func TestCall_HelpWithoutDocs(t *testing.T) {
	t.Parallel()
	s := NewToolsService(ToolsDeps{Targets: testCatalog(t)})
	ctx := ctxWithGroups("ringsrv-users")

	list, err := s.List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, ToolHelp, list.Tools[len(list.Tools)-1].Name)

	res, _ := s.Call(ctx, ToolHelp, nil)
	assert.Empty(t, decodeHelp(t, res).Sheets)
	res, _ = s.Call(ctx, ToolHelp, map[string]any{"names": []any{"prom"}})
	assert.Equal(t, ErrCodeSheetUnknown, decodeErr(t, res).Code)
}
