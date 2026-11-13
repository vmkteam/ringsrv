package public

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"golang.org/x/sync/errgroup"
)

// Mattermost ids are 26 characters of lower-case base32; names of teams,
// channels and people are letters, digits and . _ -.
const (
	mmID   = `[a-z0-9]{26}`
	mmName = `[A-Za-z0-9._-]+`
)

var mattermostID = regexp.MustCompile(`^` + mmID + `$`)

// mmChannelOpen is the type of a public channel. Private ones are P, direct
// messages D, group messages G.
const mmChannelOpen = "O"

// mattermostRoutes are the paths of the Mattermost API v4 a PublicOnly
// profile may call, with what is checked around each.
var mattermostRoutes = []route{
	mmRoute(http.MethodGet, `/api/v4/teams`, ruleOpen),
	mmRoute(http.MethodGet, `/api/v4/teams/`+mmID, ruleOpen),
	mmRoute(http.MethodGet, `/api/v4/teams/name/`+mmName, ruleOpen),
	mmRoute(http.MethodGet, `/api/v4/teams/`+mmID+`/channels`, ruleChannels),
	mmRoute(http.MethodPost, `/api/v4/teams/`+mmID+`/channels/search`, ruleChannels),
	mmRoute(http.MethodGet, `/api/v4/channels/`+mmID, ruleChannel),
	mmRoute(http.MethodGet, `/api/v4/teams/`+mmID+`/channels/name/`+mmName, ruleChannel),
	mmRoute(http.MethodGet, `/api/v4/teams/name/`+mmName+`/channels/name/`+mmName, ruleChannel),
	{method: http.MethodGet, re: regexp.MustCompile(`^/api/v4/channels/(` + mmID + `)/posts$`), rule: rulePosts, channelInPath: 1},
	mmRoute(http.MethodGet, `/api/v4/posts/`+mmID, rulePost),
	mmRoute(http.MethodGet, `/api/v4/posts/`+mmID+`/thread`, rulePosts),
	mmRoute(http.MethodPost, `/api/v4/teams/`+mmID+`/posts/search`, ruleSearch),
	mmRoute(http.MethodPost, `/api/v4/posts/search`, ruleSearch),
	mmRoute(http.MethodPost, `/api/v4/teams/`+mmID+`/files/search`, ruleFiles),
	mmRoute(http.MethodGet, `/api/v4/users/`+mmID, ruleOpen),
	mmRoute(http.MethodGet, `/api/v4/users/username/`+mmName, ruleOpen),
	mmRoute(http.MethodPost, `/api/v4/users/ids`, ruleOpen),
	mmRoute(http.MethodPost, `/api/v4/users/usernames`, ruleOpen),
}

func mmRoute(method, path string, r rule) route {
	return route{method: method, re: regexp.MustCompile(`^` + path + `$`), rule: r}
}

// object is a JSON object whose fields this package does not read pass on
// as they came.
type object = map[string]json.RawMessage

type mmChannel struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func (c mmChannel) public() bool { return c.Type == mmChannelOpen }

func decodeChannel(body []byte) (mmChannel, error) {
	var ch mmChannel
	if err := json.Unmarshal(body, &ch); err != nil {
		return ch, fmt.Errorf("not a channel: %w", err)
	}
	return ch, nil
}

// mattermost applies a rule to an answer of the Mattermost API.
func (g *Guard) mattermost(ctx context.Context, prof *target.Profile, r rule, body []byte) ([]byte, error) {
	switch r {
	case ruleOpen:
		return body, nil
	case ruleChannel:
		ch, err := decodeChannel(body)
		if err != nil {
			return nil, err
		}
		if !ch.public() {
			return nil, refused("channel %s is not public; only public channels are served", ch.ID)
		}
		return body, nil
	case ruleChannels:
		return keepPublicChannels(body)
	case rulePost:
		return g.post(ctx, prof, body)
	case rulePosts, ruleSearch:
		return g.postList(ctx, prof, body, r == ruleSearch)
	case ruleFiles:
		return g.fileList(ctx, prof, body)
	}
	return nil, fmt.Errorf("rule %d has no check", r)
}

// keepPublicChannels drops the channels that are not public from a list: a
// channel says its own type.
func keepPublicChannels(body []byte) ([]byte, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("not a list of channels: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(list))
	for _, raw := range list {
		ch, err := decodeChannel(raw)
		if err != nil {
			return nil, err
		}
		if ch.public() {
			kept = append(kept, raw)
		}
	}
	if len(kept) == len(list) {
		return body, nil
	}
	return json.Marshal(kept)
}

// post checks one post: of a public channel, with only the previews of
// public posts in it.
func (g *Guard) post(ctx context.Context, prof *target.Profile, body []byte) ([]byte, error) {
	var p object
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("not a post: %w", err)
	}
	posts := map[string]object{"": p}
	public, err := g.resolve(ctx, prof, channelsOf(posts))
	if err != nil {
		return nil, err
	}
	if ch := field(p, "channel_id"); !public[ch] {
		return nil, refused("post %s is in channel %s, which is not public", field(p, "id"), ch)
	}
	if !stripPreviews(p, public) {
		return body, nil
	}
	return json.Marshal(p)
}

// postList checks a PostList or a PostSearchResults: the posts in "posts",
// their ids in "order", and for a search their "matches". A thread or a
// channel's history is refused whole when a post in it is not public — the
// path named one, and something else answered; a search keeps what is
// public.
func (g *Guard) postList(ctx context.Context, prof *target.Profile, body []byte, search bool) ([]byte, error) {
	var list object
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("not a list of posts: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := unmarshalField(list, "posts", &raw); err != nil {
		return nil, err
	}
	posts := make(map[string]object, len(raw))
	for id, r := range raw {
		var p object
		if err := json.Unmarshal(r, &p); err != nil {
			return nil, fmt.Errorf("post %s: %w", id, err)
		}
		posts[id] = p
	}
	public, err := g.resolve(ctx, prof, channelsOf(posts))
	if err != nil {
		return nil, err
	}

	changed := false
	for id, p := range posts {
		ch := field(p, "channel_id")
		if public[ch] {
			if stripPreviews(p, public) {
				changed = true
				if raw[id], err = json.Marshal(p); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !search {
			return nil, refused("post %s is in channel %s, which is not public", id, ch)
		}
		delete(raw, id)
		changed = true
	}
	if !changed {
		return body, nil
	}
	if err := setField(list, "posts", raw); err != nil {
		return nil, err
	}
	if err := keepKeys(list, "order", raw); err != nil {
		return nil, err
	}
	if err := keepMapKeys(list, "matches", raw); err != nil {
		return nil, err
	}
	return json.Marshal(list)
}

// fileList checks a FileInfoList: the files in "file_infos", their ids in
// "order". Those of channels that are not public are dropped.
func (g *Guard) fileList(ctx context.Context, prof *target.Profile, body []byte) ([]byte, error) {
	var list object
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("not a list of files: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := unmarshalField(list, "file_infos", &raw); err != nil {
		return nil, err
	}
	infos := make(map[string]object, len(raw))
	for id, r := range raw {
		var f object
		if err := json.Unmarshal(r, &f); err != nil {
			return nil, fmt.Errorf("file %s: %w", id, err)
		}
		infos[id] = f
	}
	public, err := g.resolve(ctx, prof, channelsOf(infos))
	if err != nil {
		return nil, err
	}
	changed := false
	for id, f := range infos {
		if !public[field(f, "channel_id")] {
			delete(raw, id)
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	if err := setField(list, "file_infos", raw); err != nil {
		return nil, err
	}
	if err := keepKeys(list, "order", raw); err != nil {
		return nil, err
	}
	return json.Marshal(list)
}

// channelsOf collects the channels a set of posts or files touches: their
// own, and those of the posts their permalink previews show.
func channelsOf(items map[string]object) []string {
	var out []string
	for _, it := range items {
		out = append(out, field(it, "channel_id"))
		for _, e := range previews(it) {
			out = append(out, e.channel)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// resolve says which of the channels are public, looking them up a few at a
// time. An empty id is not public.
func (g *Guard) resolve(ctx context.Context, prof *target.Profile, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	results := make([]bool, len(ids))
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(lookupsInFlight)
	for i, id := range ids {
		if id == "" {
			continue
		}
		eg.Go(func() error {
			public, err := g.ChannelPublic(ctx, prof, id)
			results[i] = public
			return err
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	for i, id := range ids {
		out[id] = results[i]
	}
	return out, nil
}

// preview is one permalink preview in a post: Mattermost shows the post a
// link points at inside the post that links it, whatever channel that one is
// in — so a public post can carry a private one.
type preview struct {
	index   int
	channel string
}

func previews(p object) []preview {
	var meta object
	if json.Unmarshal(p["metadata"], &meta) != nil {
		return nil
	}
	var embeds []object
	if json.Unmarshal(meta["embeds"], &embeds) != nil {
		return nil
	}
	var out []preview
	for i, e := range embeds {
		if field(e, "type") != "permalink" {
			continue
		}
		var data object
		_ = json.Unmarshal(e["data"], &data)
		out = append(out, preview{index: i, channel: field(data, "channel_id")})
	}
	return out
}

// stripPreviews drops the permalink previews of posts in channels that are
// not public, and reports whether it dropped any.
func stripPreviews(p object, public map[string]bool) bool {
	drop := map[int]bool{}
	for _, e := range previews(p) {
		if !public[e.channel] {
			drop[e.index] = true
		}
	}
	if len(drop) == 0 {
		return false
	}
	var meta object
	_ = json.Unmarshal(p["metadata"], &meta)
	var embeds []json.RawMessage
	_ = json.Unmarshal(meta["embeds"], &embeds)
	kept := make([]json.RawMessage, 0, len(embeds))
	for i, e := range embeds {
		if !drop[i] {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		delete(meta, "embeds")
	} else {
		meta["embeds"], _ = json.Marshal(kept)
	}
	p["metadata"], _ = json.Marshal(meta)
	return true
}

// field is a string field of an object, empty when it is not one.
func field(o object, name string) string {
	var s string
	if json.Unmarshal(o[name], &s) != nil {
		return ""
	}
	return s
}

// unmarshalField reads a field the answer must have.
func unmarshalField(o object, name string, v any) error {
	raw, ok := o[name]
	if !ok {
		return fmt.Errorf("no %q in the answer", name)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%q: %w", name, err)
	}
	return nil
}

func setField(o object, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	o[name] = raw
	return nil
}

// keepKeys keeps in a list of ids those still in kept. A list that is absent
// or null stays as it was.
func keepKeys[V any](o object, name string, kept map[string]V) error {
	raw, ok := o[name]
	if !ok || string(raw) == "null" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return fmt.Errorf("%q: %w", name, err)
	}
	ids = slices.DeleteFunc(ids, func(id string) bool {
		_, ok := kept[id]
		return !ok
	})
	return setField(o, name, ids)
}

// keepMapKeys keeps in a map by id the entries still in kept.
func keepMapKeys[V any](o object, name string, kept map[string]V) error {
	raw, ok := o[name]
	if !ok || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("%q: %w", name, err)
	}
	for id := range m {
		if _, ok := kept[id]; !ok {
			delete(m, id)
		}
	}
	return setField(o, name, m)
}
