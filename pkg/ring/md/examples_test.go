package md_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exampleProfile is a profile cfg/README.md recommends for a target no shipped
// catalogue carries, read from testdata/<name>.toml. The sheet is checked
// against it, and the README against it, so the three cannot drift.
func exampleProfile(t *testing.T, name string) (*target.Profile, string) {
	t.Helper()
	body, err := os.ReadFile(path.Join("testdata", name+".toml"))
	require.NoError(t, err)
	toml := "Env = \"prod\"\n\n" + string(body) + `
[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["` + name + `"]
`
	cat, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	p, ok := cat.Profile(name)
	require.True(t, ok)
	return p, string(body)
}

func TestExampleSheetsMatchExample(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(path.Join("..", "..", "..", "cfg", "README.md"))
	require.NoError(t, err)

	for _, name := range []string{"mattermost", "slack"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, example := exampleProfile(t, name)
			// Index rather than Contains: a failure would print the whole README.
			assert.NotEqual(t, -1, strings.Index(string(readme), example), "cfg/README.md shows the profile the sheet is checked against, verbatim")

			body, err := fs.ReadFile(md.FS, path.Join(md.Root, "targets", name+".md"))
			require.NoError(t, err)
			sheet := string(body)

			paths := extractPaths(sheet)
			require.NotEmpty(t, paths)
			for _, raw := range paths {
				assert.True(t, p.AllowsPath(raw), "%q is documented but the example rejects it", raw)
				require.NoError(t, p.CheckQuery(raw), "%q is documented but its parameters are refused", raw)
			}

			listed := map[string]bool{}
			for _, entry := range p.AllowQueryParams {
				param, _, _ := strings.Cut(entry, "<")
				listed[param] = true
				assert.Contains(t, sheet, "`"+param+"`", "the example takes %q, the sheet does not mention it", param)
			}
			sentence := paramSentence.FindString(sheet)
			require.NotEmpty(t, sentence)
			for _, m := range paramName.FindAllStringSubmatch(sentence, -1) {
				assert.True(t, listed[m[1]], "the sheet lists %q, the example does not take it", m[1])
			}
		})
	}
}

// shapeWith runs a profile's default filter the way a call without its own jq
// does, and hands back plain JSON values to compare.
func shapeWith(t *testing.T, p *target.Profile, body string) any {
	t.Helper()
	out, err := ring.ShapeJSON(context.Background(), []byte(body), p.DefaultJQ, time.Second, "", nil, 1<<20)
	require.NoError(t, err)
	raw, err := json.Marshal(out.Data)
	require.NoError(t, err)
	var v any
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}

// The default filter turns each answer the sheet describes into the shape it
// describes.
func TestMattermostDefaultJQ(t *testing.T) {
	t.Parallel()
	p, _ := exampleProfile(t, "mattermost")
	shape := func(body string) any { return shapeWith(t, p, body) }

	list := shape(`{"order":["b","a"],"posts":{
		"a":{"id":"a","channel_id":"c","root_id":"","user_id":"u","create_at":1,"edit_at":0,"message":"root","reply_count":1,"props":{},"metadata":{}},
		"b":{"id":"b","channel_id":"c","root_id":"a","user_id":"bot","create_at":2,"message":"","props":{"override_username":"Ann","attachments":[{"title":"Alert","text":"disk full","fields":[{"title":"host","value":"db1","short":true}],"color":"#f00"}]},
		     "metadata":{"files":[{"id":"f","name":"df.txt","extension":"txt","size":10,"mime_type":"text/plain"}]}}},
		"next_post_id":"","prev_post_id":"","has_next":false}`)
	assert.Equal(t, map[string]any{"posts": []any{
		map[string]any{"id": "b", "channel_id": "c", "root_id": "a", "user_id": "bot", "override_username": "Ann", "create_at": 2.0,
			"files":       []any{map[string]any{"id": "f", "name": "df.txt", "extension": "txt", "size": 10.0}},
			"attachments": []any{map[string]any{"title": "Alert", "text": "disk full", "fields": []any{map[string]any{"title": "host", "value": "db1"}}}}},
		map[string]any{"id": "a", "channel_id": "c", "user_id": "u", "create_at": 1.0, "message": "root", "reply_count": 1.0},
	}, "has_next": false}, list, "posts in order, empty fields dropped, a bot's attachments kept")

	empty := shape(`{"order":[],"posts":{},"next_post_id":"","prev_post_id":"","matches":{}}`)
	assert.Equal(t, map[string]any{"posts": []any{}}, empty, "a search that found nothing public says so")

	person := shape(`{"id":"u","username":"ann","first_name":"Ann","last_name":"","email":"ann@example.com","notify_props":{"x":"y"},"is_bot":false}`)
	assert.Equal(t, map[string]any{"id": "u", "username": "ann", "first_name": "Ann", "is_bot": false}, person)

	channels := shape(`[{"id":"c","team_id":"t","type":"O","display_name":"Town Square","name":"town-square","purpose":"","total_msg_count":5}]`)
	assert.Equal(t, []any{map[string]any{"id": "c", "team_id": "t", "type": "O", "display_name": "Town Square", "name": "town-square"}}, channels)

	team := shape(`{"id":"t","name":"acme","display_name":"Acme","type":"I","email":"owner@example.com","allowed_domains":"","invite_id":"secret"}`)
	assert.Equal(t, map[string]any{"id": "t", "name": "acme", "display_name": "Acme", "type": "I"}, team)

	files := shape(`{"order":["f"],"file_infos":{"f":{"id":"f","name":"a.csv","extension":"csv","size":3,"channel_id":"c","post_id":"p","user_id":"u"}}}`)
	assert.Equal(t, map[string]any{"files": []any{map[string]any{"id": "f", "name": "a.csv", "extension": "csv", "size": 3.0, "channel_id": "c", "post_id": "p"}}}, files)
}

// The Slack filter lifts an error answered with HTTP 200, and keeps what bots
// and block-built messages carry outside text: half the calls brought a jq of
// their own to dig these out.
func TestSlackDefaultJQ(t *testing.T) {
	t.Parallel()
	p, _ := exampleProfile(t, "slack")
	shape := func(body string) any { return shapeWith(t, p, body) }

	failed := shape(`{"ok":false,"error":"missing_scope","needed":"channels:history","provided":"search:read.public"}`)
	assert.Equal(t, map[string]any{"error": "missing_scope", "needed": "channels:history", "provided": "search:read.public"}, failed)

	found := shape(`{"ok":true,"results":{"messages":[
		{"channel_id":"C1","channel_name":"incidents","is_author_bot":false,"author_name":"Ann","message_ts":"1725000000.000200",
		 "permalink":"https://acme.slack.com/archives/C1/p1725000000000200?thread_ts=1725000000.000100","reply_count":0,"content":"rollback done"},
		{"channel_id":"C1","channel_name":"incidents","is_author_bot":true,"author_name":"CI","message_ts":"1725000000.000300",
		 "permalink":"https://acme.slack.com/archives/C1/p1725000000000300","content":"pipeline failed"}]},
		"response_metadata":{"next_cursor":"abc"}}`)
	assert.Equal(t, map[string]any{"matches": []any{
		map[string]any{"channel": "C1", "name": "incidents", "user": "Ann", "ts": "1725000000.000200", "thread_ts": "1725000000.000100",
			"reply_count": 0.0, "text": "rollback done", "permalink": "https://acme.slack.com/archives/C1/p1725000000000200?thread_ts=1725000000.000100"},
		map[string]any{"channel": "C1", "name": "incidents", "user": "bot", "ts": "1725000000.000300", "thread_ts": nil,
			"reply_count": nil, "text": "pipeline failed", "permalink": "https://acme.slack.com/archives/C1/p1725000000000300"},
	}, "next_cursor": "abc"}, found, "a reply carries its thread, a bot is named bot")

	history := shape(`{"ok":true,"has_more":false,"messages":[
		{"user":"U1","ts":"1.1","text":"","blocks":[
			{"type":"rich_text","elements":[
				{"type":"rich_text_section","elements":[{"type":"text","text":"deploy "},{"type":"text","text":"failed","style":{"bold":true}}]},
				{"type":"rich_text_list","elements":[
					{"type":"rich_text_section","elements":[{"type":"text","text":"api"}]},
					{"type":"rich_text_section","elements":[{"type":"text","text":"web"}]}]}]},
			{"type":"section","text":{"type":"mrkdwn","text":"*rolled back*"},"fields":[{"type":"mrkdwn","text":"env: prod"}]}]},
		{"bot_id":"B1","ts":"1.2","text":"","attachments":[
			{"color":"#f00","pretext":"","title":"Alert","text":"disk full","fields":[{"title":"host","value":"db1","short":true}]}]},
		{"user":"U2","ts":"1.3","text":"see file","files":[{"id":"F1","name":"a.csv","filetype":"csv","size":3,"url_private":"https://files.slack.com/x"}],
		 "blocks":[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"see file"}]}]}]}]}`)
	assert.Equal(t, map[string]any{"messages": []any{
		map[string]any{"user": "U1", "ts": "1.1", "thread_ts": nil, "reply_count": nil, "text": "",
			"blocks": "deploy failed\napi\nweb\n*rolled back*\nenv: prod"},
		map[string]any{"user": nil, "ts": "1.2", "thread_ts": nil, "reply_count": nil, "text": "",
			"attachments": []any{map[string]any{"title": "Alert", "text": "disk full", "fields": []any{map[string]any{"title": "host", "value": "db1"}}}}},
		map[string]any{"user": "U2", "ts": "1.3", "thread_ts": nil, "reply_count": nil, "text": "see file",
			"files": []any{map[string]any{"id": "F1", "name": "a.csv", "filetype": "csv", "size": 3.0}}},
	}, "has_more": false, "next_cursor": nil}, history, "inline pieces join into one line, a bot's attachment stays, blocks only when text is empty")
}
