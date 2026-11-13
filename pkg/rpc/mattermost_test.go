package rpc

import (
	"fmt"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/public/publictest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
)

// mattermostService builds a service whose mattermost target is kept to public
// channels and serves their files.
func mattermostService(t *testing.T, m *publictest.Mattermost, l embedlog.Logger) ToolsService {
	t.Helper()
	toml := fmt.Sprintf(`
Env = "dev"

[Profiles.mattermost]
Description      = "dev · Mattermost"
BaseURL          = %q
Headers          = ["Authorization: Bearer mm-token"]
AllowMethods     = ["GET", "POST"]
ReadOnlyPost     = true
AllowPaths       = ["^/api/v4/channels/[a-z0-9]{26}(/posts)?$", "^/api/v4/posts/[a-z0-9]{26}(/thread)?$", "POST ^/api/v4/teams/[a-z0-9]{26}/posts/search$", "^/files/download$"]
AllowQueryParams = ["page", "per_page", "file", "offset", "limit<=1000", "grep", "sheet", "entry"]
Files            = "mattermost"
PublicOnly       = "mattermost"

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["mattermost"]
`, m.URL())
	cat, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	return NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		MaxBytes:  32 << 10,
		JQTimeout: time.Second,
		Sessions:  ring.NewSessions(time.Hour, nil),
		Logger:    l,
		Docs:      testDocs(t),
	})
}

func mmCall(method, path, body string) map[string]any {
	c := map[string]any{"target": "mattermost", "method": method, "path": path}
	if body != "" {
		c["body"] = body
	}
	return c
}

// One batch through the whole pipeline: a search keeps the public posts, a
// private channel's history is refused before it is read, a public file is
// served, and the refusal is a denial in the audit.
func TestMattermost_PublicOnlyThroughAPICall(t *testing.T) {
	m := publictest.New(t)
	open, private, team := publictest.ID("open"), publictest.ID("priv"), publictest.ID("team")
	m.Channel(open, "O")
	m.Channel(private, "P")
	p1, p2 := publictest.ID("p1"), publictest.ID("p2")
	m.Answer("POST", "/api/v4/teams/"+team+"/posts/search", `{"order":["`+p1+`","`+p2+`"],"posts":{`+
		`"`+p1+`":{"id":"`+p1+`","channel_id":"`+open+`","message":"deploy done"},`+
		`"`+p2+`":{"id":"`+p2+`","channel_id":"`+private+`","message":"salaries"}}}`)
	m.Answer("GET", "/api/v4/channels/"+private+"/posts", `{"order":[],"posts":{}}`)
	file := publictest.ID("file")
	m.AddFile(file, open, "notes.txt", "text/plain", []byte("first\nsecond\n"), nil)

	var res mcp.ToolCallResult
	recs := captureLog(t, func(l embedlog.Logger) {
		var err error
		res, err = mattermostService(t, m, l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, map[string]any{"calls": []any{
			mmCall("POST", "/api/v4/teams/"+team+"/posts/search", `{"terms":"deploy","per_page":20}`),
			mmCall("GET", "/api/v4/channels/"+private+"/posts?per_page=20", ""),
			mmCall("GET", "/files/download?file="+file+"&limit=1", ""),
		}})
		require.NoError(t, err)
	})
	out := decodeBatch(t, res)
	require.Len(t, out.Results, 3)

	require.Nil(t, out.Results[0].Error)
	assert.NotContains(t, res.Content[0].Text, "salaries")
	assert.Contains(t, res.Content[0].Text, "deploy done")

	require.NotNil(t, out.Results[1].Error)
	assert.Equal(t, ErrCodeNotPublic, out.Results[1].Error.Code)
	assert.NotContains(t, m.Requests(), "GET /api/v4/channels/"+private+"/posts", "refused before the call")

	require.Nil(t, out.Results[2].Error)
	data, ok := out.Results[2].Data.(map[string]any)
	require.True(t, ok, "%#v", out.Results[2].Data)
	assert.Equal(t, []any{"1: first"}, data["lines"])

	records := itemRecords(recs)
	require.Len(t, records, 3)
	byIndex := map[float64]map[string]any{}
	for _, r := range records {
		idx, ok := r["batch_index"].(float64)
		require.True(t, ok)
		byIndex[idx] = r
	}
	assert.Equal(t, audit.DecisionDeny, byIndex[1]["decision"])
	assert.Equal(t, ErrCodeNotPublic, byIndex[1]["deny_reason"])
	assert.Equal(t, open, byIndex[2]["file_channels"])
}
