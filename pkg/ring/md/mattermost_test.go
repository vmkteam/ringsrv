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

// mattermostExample is the profile cfg/README.md recommends. No shipped
// catalogue carries a Mattermost target yet, so the sheet is checked against
// this one — and the README against it, so the two cannot drift.
func mattermostExample(t *testing.T) (*target.Profile, string) {
	t.Helper()
	body, err := os.ReadFile(path.Join("testdata", "mattermost.toml"))
	require.NoError(t, err)
	toml := "Env = \"prod\"\n\n" + string(body) + `
[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["mattermost"]
`
	cat, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	p, ok := cat.Profile("mattermost")
	require.True(t, ok)
	return p, string(body)
}

func TestMattermostSheetMatchesExample(t *testing.T) {
	t.Parallel()
	p, example := mattermostExample(t)

	readme, err := os.ReadFile(path.Join("..", "..", "..", "cfg", "README.md"))
	require.NoError(t, err)
	// Index rather than Contains: a failure would print the whole README.
	assert.NotEqual(t, -1, strings.Index(string(readme), example), "cfg/README.md shows the profile the sheet is checked against, verbatim")

	body, err := fs.ReadFile(md.FS, path.Join(md.Root, "targets", "mattermost.md"))
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
}

// The default filter turns each answer the sheet describes into the shape it
// describes.
func TestMattermostDefaultJQ(t *testing.T) {
	t.Parallel()
	p, _ := mattermostExample(t)
	shape := func(body string) any {
		t.Helper()
		out, err := ring.ShapeJSON(context.Background(), []byte(body), p.DefaultJQ, time.Second, "", nil, 1<<20)
		require.NoError(t, err)
		raw, err := json.Marshal(out.Data)
		require.NoError(t, err)
		var v any
		require.NoError(t, json.Unmarshal(raw, &v))
		return v
	}

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

	person := shape(`{"id":"u","username":"ann","first_name":"Ann","last_name":"","email":"ann@example.com","notify_props":{"x":"y"},"is_bot":false}`)
	assert.Equal(t, map[string]any{"id": "u", "username": "ann", "first_name": "Ann", "is_bot": false}, person)

	channels := shape(`[{"id":"c","team_id":"t","type":"O","display_name":"Town Square","name":"town-square","purpose":"","total_msg_count":5}]`)
	assert.Equal(t, []any{map[string]any{"id": "c", "team_id": "t", "type": "O", "display_name": "Town Square", "name": "town-square"}}, channels)

	team := shape(`{"id":"t","name":"acme","display_name":"Acme","type":"I","email":"owner@example.com","allowed_domains":"","invite_id":"secret"}`)
	assert.Equal(t, map[string]any{"id": "t", "name": "acme", "display_name": "Acme", "type": "I"}, team)

	files := shape(`{"order":["f"],"file_infos":{"f":{"id":"f","name":"a.csv","extension":"csv","size":3,"channel_id":"c","post_id":"p","user_id":"u"}}}`)
	assert.Equal(t, map[string]any{"files": []any{map[string]any{"id": "f", "name": "a.csv", "extension": "csv", "size": 3.0, "channel_id": "c", "post_id": "p"}}}, files)
}
