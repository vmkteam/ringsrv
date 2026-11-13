package public

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/public/publictest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	team    = publictest.ID("team")
	open    = publictest.ID("open")
	open2   = publictest.ID("open2")
	private = publictest.ID("priv")
	direct  = publictest.ID("dm")
	hidden  = publictest.ID("forbid")
	gone    = publictest.ID("gone")
)

// chat is a fake with a public channel, a second public one, a private one
// the account was added to, a direct message, one the account may not see
// and none for gone.
func chat(t *testing.T) *publictest.Mattermost {
	t.Helper()
	m := publictest.New(t)
	m.Channel(open, "O")
	m.Channel(open2, "O")
	m.Channel(private, "P")
	m.Channel(direct, "D")
	m.Channel(hidden, "403")
	return m
}

// AllowPaths is the allowlist of a Mattermost profile, as the cheat sheet
// gives it: the paths the check has rules for and nothing else.
const AllowPaths = `[
  "^/api/v4/teams(/[a-z0-9]{26}|/name/[A-Za-z0-9._-]+)?$",
  "^/api/v4/teams/[a-z0-9]{26}/channels$",
  "^/api/v4/teams/([a-z0-9]{26}|name/[A-Za-z0-9._-]+)/channels/name/[A-Za-z0-9._-]+$",
  "^/api/v4/channels/[a-z0-9]{26}(/posts)?$",
  "^/api/v4/posts/[a-z0-9]{26}(/thread)?$",
  "^/api/v4/users/([a-z0-9]{26}|username/[A-Za-z0-9._-]+)$",
  "POST ^/api/v4/(teams/[a-z0-9]{26}/)?posts/search$",
  "POST ^/api/v4/teams/[a-z0-9]{26}/(channels|files)/search$",
  "POST ^/api/v4/users/(ids|usernames)$",
]`

func profileFor(t *testing.T, m *publictest.Mattermost, prefix string) *target.Profile {
	t.Helper()
	extra := ""
	if prefix != "" {
		extra = fmt.Sprintf("PathPrefix = %q", prefix)
	}
	toml := fmt.Sprintf(`
Env = "prod"

[Profiles.mattermost]
Description  = "prod · Mattermost"
BaseURL      = %q
Headers      = ["Authorization: Bearer mm-token"]
AllowMethods = ["GET", "POST"]
ReadOnlyPost = true
AllowPaths   = %s
PublicOnly   = "mattermost"
%s

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["mattermost"]
`, m.URL(), AllowPaths, extra)
	c, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	p, ok := c.Profile("mattermost")
	require.True(t, ok)
	return p
}

func newGuard() *Guard {
	return NewGuard(upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}))
}

func refusal(err error) string {
	var ref *RefusedError
	if errors.As(err, &ref) {
		return ref.Code
	}
	return ""
}

func post(id, channel, message string, extra ...string) string {
	p := fmt.Sprintf(`{"id":%q,"channel_id":%q,"message":%q`, id, channel, message)
	if len(extra) > 0 {
		p += "," + strings.Join(extra, ",")
	}
	return p + "}"
}

// run is one call through Before and After, the way the proxy makes it.
func run(t *testing.T, g *Guard, prof *target.Profile, method, path, body string) ([]byte, error) {
	t.Helper()
	c, err := g.Before(context.Background(), prof, method, path)
	if err != nil {
		return nil, err
	}
	return g.After(context.Background(), prof, c, []byte(body), false)
}

func TestBefore_Routes(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")

	for _, tc := range []struct {
		method, path string
		ok           bool
	}{
		{http.MethodGet, "/api/v4/teams", true},
		{http.MethodGet, "/api/v4/teams/name/acme", true},
		{http.MethodGet, "/api/v4/channels/" + open + "/posts?page=0&per_page=60", true},
		{http.MethodGet, "/api/v4/posts/" + publictest.ID("p") + "/thread", true},
		{http.MethodPost, "/api/v4/teams/" + team + "/posts/search", true},
		{http.MethodGet, "/api/v4/users/username/ann.smith", true},
		{http.MethodGet, "/api/v4/channels/" + private + "/posts", false},
		{http.MethodGet, "/api/v4/channels/" + direct + "/posts", false},
		{http.MethodGet, "/api/v4/channels/" + hidden + "/posts", false},
		{http.MethodGet, "/api/v4/channels/" + gone + "/posts", false},
		{http.MethodGet, "/api/v4/users/me/channels", false},
		{http.MethodGet, "/api/v4/channels/" + open + "/members", false},
		{http.MethodGet, "/api/v4/files/" + publictest.ID("f"), false},
		{http.MethodPost, "/api/v4/teams", false},
		{http.MethodGet, "/api/v4/channels/" + open + "/posts/../members", false},
	} {
		_, err := g.Before(context.Background(), prof, tc.method, tc.path)
		if tc.ok {
			require.NoError(t, err, "%s %s", tc.method, tc.path)
			continue
		}
		require.Error(t, err, "%s %s", tc.method, tc.path)
		assert.Equal(t, CodeNotPublic, refusal(err), "%s %s: %v", tc.method, tc.path, err)
	}
}

func TestAfter_SearchKeepsPublic(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")
	p1, p2, p3, p4 := publictest.ID("p1"), publictest.ID("p2"), publictest.ID("p3"), publictest.ID("p4")
	body := `{"order":["` + p1 + `","` + p2 + `","` + p3 + `","` + p4 + `"],"posts":{` +
		`"` + p1 + `":` + post(p1, open, "public") + `,` +
		`"` + p2 + `":` + post(p2, private, "salaries") + `,` +
		`"` + p3 + `":` + post(p3, direct, "between two") + `,` +
		`"` + p4 + `":` + post(p4, open2, "also public") + `},` +
		`"matches":{"` + p1 + `":["public"],"` + p2 + `":["salaries"]},"next_post_id":"","has_next":false}`

	out, err := run(t, g, prof, http.MethodPost, "/api/v4/teams/"+team+"/posts/search", body)
	require.NoError(t, err)
	var got struct {
		Order   []string                   `json:"order"`
		Posts   map[string]json.RawMessage `json:"posts"`
		Matches map[string][]string        `json:"matches"`
		HasNext *bool                      `json:"has_next"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, []string{p1, p4}, got.Order)
	assert.Len(t, got.Posts, 2)
	assert.Contains(t, got.Posts, p1)
	assert.Contains(t, got.Posts, p4)
	assert.Equal(t, map[string][]string{p1: {"public"}}, got.Matches)
	assert.NotNil(t, got.HasNext, "fields the check does not read stay")
	assert.NotContains(t, string(out), "salaries")
	assert.NotContains(t, string(out), "between two")
}

func TestAfter_ThreadRefusedWhole(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")
	p1 := publictest.ID("p1")
	body := `{"order":["` + p1 + `"],"posts":{"` + p1 + `":` + post(p1, private, "salaries") + `}}`

	_, err := run(t, g, prof, http.MethodGet, "/api/v4/posts/"+p1+"/thread", body)
	assert.Equal(t, CodeNotPublic, refusal(err))
	assert.NotContains(t, err.Error(), "salaries")

	_, err = run(t, g, prof, http.MethodGet, "/api/v4/posts/"+p1, post(p1, direct, "hi"))
	assert.Equal(t, CodeNotPublic, refusal(err))

	out, err := run(t, g, prof, http.MethodGet, "/api/v4/posts/"+p1, post(p1, open, "hi"))
	require.NoError(t, err)
	assert.JSONEq(t, post(p1, open, "hi"), string(out), "a public post passes as it came")
}

// A public post that links a private one carries its preview; the preview
// goes, the post stays.
func TestAfter_PermalinkPreviews(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")
	p1 := publictest.ID("p1")
	meta := `"metadata":{"embeds":[` +
		`{"type":"permalink","data":{"post_id":"x","channel_id":"` + private + `","post":{"message":"salaries"}}},` +
		`{"type":"permalink","data":{"post_id":"y","channel_id":"` + open2 + `","post":{"message":"release notes"}}},` +
		`{"type":"opengraph","url":"https://example.com"}],"files":[]}`
	body := `{"order":["` + p1 + `"],"posts":{"` + p1 + `":` + post(p1, open, "see link", meta) + `}}`

	out, err := run(t, g, prof, http.MethodGet, "/api/v4/posts/"+p1+"/thread", body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "salaries")
	assert.Contains(t, string(out), "release notes")
	assert.Contains(t, string(out), "opengraph")
	assert.Contains(t, string(out), `"files":[]`, "the rest of metadata stays")

	single, err := run(t, g, prof, http.MethodGet, "/api/v4/posts/"+p1, post(p1, open, "see link", meta))
	require.NoError(t, err)
	assert.NotContains(t, string(single), "salaries")
}

func TestAfter_Channels(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")

	list := `[{"id":"` + open + `","type":"O","name":"town-square"},{"id":"` + private + `","type":"P","name":"secret"}]`
	out, err := run(t, g, prof, http.MethodGet, "/api/v4/teams/"+team+"/channels", list)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"id":"`+open+`","type":"O","name":"town-square"}]`, string(out))

	_, err = run(t, g, prof, http.MethodGet, "/api/v4/teams/name/acme/channels/name/secret", `{"id":"`+private+`","type":"P","name":"secret"}`)
	assert.Equal(t, CodeNotPublic, refusal(err))
	assert.Zero(t, m.Lookups(), "a channel says its own type")
}

func TestAfter_Files(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")
	f1, f2 := publictest.ID("f1"), publictest.ID("f2")
	body := `{"order":["` + f1 + `","` + f2 + `"],"file_infos":{` +
		`"` + f1 + `":{"id":"` + f1 + `","channel_id":"` + open + `","name":"a.csv"},` +
		`"` + f2 + `":{"id":"` + f2 + `","channel_id":"` + private + `","name":"salaries.xlsx"}}}`
	out, err := run(t, g, prof, http.MethodPost, "/api/v4/teams/"+team+"/files/search", body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "salaries")
	assert.Contains(t, string(out), `"order":["`+f1+`"]`)
}

func TestAfter_Unreadable(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")

	c, err := g.Before(context.Background(), prof, http.MethodPost, "/api/v4/teams/"+team+"/posts/search")
	require.NoError(t, err)
	_, err = g.After(context.Background(), prof, c, []byte(`{"order":[],"posts":{}`), true)
	assert.Equal(t, CodeNotPublic, refusal(err), "an answer cut at the read limit")
	_, err = g.After(context.Background(), prof, c, []byte(`<html>`), false)
	assert.Equal(t, CodeNotPublic, refusal(err), "an answer that is not JSON")
	_, err = g.After(context.Background(), prof, c, []byte(`{"order":[]}`), false)
	assert.Equal(t, CodeNotPublic, refusal(err), "a list without its posts")

	open, err := g.Before(context.Background(), prof, http.MethodGet, "/api/v4/teams")
	require.NoError(t, err)
	cut := []byte(`[{"id":"t`)
	out, err := g.After(context.Background(), prof, open, cut, true)
	require.NoError(t, err, "an open path is not read, cut or not")
	assert.Equal(t, cut, out)
}

func TestChannelPublic_Cache(t *testing.T) {
	t.Parallel()
	m := chat(t)
	g, prof := newGuard(), profileFor(t, m, "")
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }

	for range 3 {
		public, err := g.ChannelPublic(context.Background(), prof, open)
		require.NoError(t, err)
		assert.True(t, public)
	}
	assert.Equal(t, 1, m.Lookups(), "looked up once a minute")

	m.Channel(open, "P") // made private
	now = now.Add(cacheTTL + time.Second)
	public, err := g.ChannelPublic(context.Background(), prof, open)
	require.NoError(t, err)
	assert.False(t, public, "a channel made private stops being served when its entry expires")
	assert.Equal(t, 2, m.Lookups())

	public, err = g.ChannelPublic(context.Background(), prof, "not-an-id")
	require.NoError(t, err)
	assert.False(t, public)
	assert.Equal(t, 2, m.Lookups(), "a malformed id is not looked up")
}

func TestChannelPublic_UpstreamFails(t *testing.T) {
	t.Parallel()
	m := chat(t)
	broken := publictest.ID("broken")
	m.Channel(broken, "500")
	g, prof := newGuard(), profileFor(t, m, "")

	_, err := g.ChannelPublic(context.Background(), prof, broken)
	var up *UpstreamError
	require.ErrorAs(t, err, &up, "an upstream that fails to say is not an answer")
	assert.Equal(t, http.StatusInternalServerError, up.Status)

	public, err := g.ChannelPublic(context.Background(), prof, hidden)
	require.NoError(t, err)
	assert.False(t, public, "a channel the credential may not see is not public")
}

// Behind a mount path the lookups go through the same prefix as the calls.
func TestGuard_PathPrefix(t *testing.T) {
	t.Parallel()
	m := chat(t)
	m.Prefix = "/plugins/com.example.facade"
	g, prof := newGuard(), profileFor(t, m, m.Prefix)

	_, err := g.Before(context.Background(), prof, http.MethodGet, "/api/v4/channels/"+open+"/posts")
	require.NoError(t, err)
	assert.Equal(t, []string{"GET /api/v4/channels/" + open}, m.Requests())
}
