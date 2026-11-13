// Package public keeps a profile's answers to the public channels of its
// upstream (target.Profile.PublicOnly), for a chat whose credential sees more
// than that. An account added to a private channel by mistake, or written to
// directly, reads both, and the API has no scope that would say otherwise.
//
// Every path of such a profile has a rule here: what to check before the call
// — the channel a path names — and what to keep of its answer. A path with no
// rule is refused, not passed unchecked: the allowlist can grow faster than
// this table, and the table is what the promise rests on.
package public

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
)

// CodeNotPublic is the refusal the model reads: the path or its answer is
// about a channel that is not public, or about something the rules cannot
// tell.
const CodeNotPublic = "NotPublic"

// RefusedError is a call the guard will not let through. The message names
// ids, never what is in a private channel.
type RefusedError struct {
	Code    string
	Message string
}

func (e *RefusedError) Error() string { return e.Message }

func refused(format string, args ...any) error {
	return &RefusedError{Code: CodeNotPublic, Message: fmt.Sprintf(format, args...)}
}

// UpstreamError is the upstream failing a lookup the guard needed: it
// answered, with Status, and not with what the check reads. The call is not
// made, but this is not a refusal either.
type UpstreamError struct {
	Status int
	Err    error
}

func (e *UpstreamError) Error() string { return e.Err.Error() }
func (e *UpstreamError) Unwrap() error { return e.Err }

// Cache of channel types: a channel made private stops being served within
// cacheTTL, and a search over dozens of channels costs one lookup each per
// minute rather than per call.
const (
	cacheTTL  = time.Minute
	maxCached = 10_000
	// lookupMaxBytes caps the answer about one channel: metadata, a few
	// kilobytes.
	lookupMaxBytes = 1 << 20
	// lookupsInFlight bounds the lookups one answer starts at once.
	lookupsInFlight = 4
)

// Guard checks the calls of every PublicOnly profile of an instance.
type Guard struct {
	client *upstream.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cached
}

type cacheKey struct{ profile, channel string }

type cached struct {
	public  bool
	expires time.Time
}

// NewGuard builds the guard over the instance's upstream client: its lookups
// go out like any call of the profile, prefix and credential included.
func NewGuard(client *upstream.Client) *Guard {
	return &Guard{client: client, now: time.Now, cache: map[cacheKey]cached{}}
}

// Check is what Before matched, for After to finish.
type Check struct {
	rule rule
}

// rule says what is checked around one path.
type rule int

const (
	// ruleOpen: nothing of a channel's content — teams, people.
	ruleOpen rule = iota + 1
	// ruleChannel: the answer is one channel, public or refused.
	ruleChannel
	// ruleChannels: the answer is a list of channels; the public stay.
	ruleChannels
	// rulePost: the answer is one post, of a public channel or refused.
	rulePost
	// rulePosts: the answer is posts of one thread or one channel, every one
	// of a public channel or the call is refused.
	rulePosts
	// ruleSearch: the answer is posts from anywhere; those of public
	// channels stay.
	ruleSearch
	// ruleFiles: the answer is files from anywhere; those of public channels
	// stay.
	ruleFiles
)

// route is one path the rules know.
type route struct {
	method string
	re     *regexp.Regexp
	rule   rule
	// channelInPath is the submatch naming a channel to check before the
	// call; zero for none.
	channelInPath int
}

// Before finds the rule of a call and checks what can be checked before it
// is made: the channel its path names.
func (g *Guard) Before(ctx context.Context, prof *target.Profile, method, rawPath string) (Check, error) {
	routes, err := routesOf(prof)
	if err != nil {
		return Check{}, err
	}
	path, ok := upstream.CleanPath(rawPath)
	if !ok {
		return Check{}, refused("path %q is not one the public-only check can read", rawPath)
	}
	for _, r := range routes {
		if !strings.EqualFold(r.method, method) {
			continue
		}
		m := r.re.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		if r.channelInPath > 0 {
			public, err := g.ChannelPublic(ctx, prof, m[r.channelInPath])
			if err != nil {
				return Check{}, err
			}
			if !public {
				return Check{}, refused("channel %s is not public; only public channels are served", m[r.channelInPath])
			}
		}
		return Check{rule: r.rule}, nil
	}
	return Check{}, refused("%s %s is not a path the public-only check of this target knows, so it is not called: "+
		"what it answers could not be checked", method, path)
}

// After checks the answer of a call Before let through and returns what of it
// may be shown. An answer cut at the read limit cannot be checked and is
// refused, the open ones aside.
func (g *Guard) After(ctx context.Context, prof *target.Profile, c Check, body []byte, truncated bool) ([]byte, error) {
	if c.rule == ruleOpen {
		return body, nil
	}
	if truncated {
		return nil, refused("the answer is over the read limit and cannot be checked whole; ask for fewer at a time (per_page)")
	}
	out, err := g.mattermost(ctx, prof, c.rule, body)
	if err != nil {
		var ref *RefusedError
		var up *UpstreamError
		if errors.As(err, &ref) || errors.As(err, &up) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, refused("the answer cannot be checked: %s", err)
	}
	return out, nil
}

// routesOf picks the rules of the profile's provider. The catalogue refuses an
// unknown one at load time.
func routesOf(prof *target.Profile) ([]route, error) {
	if prof.PublicOnly == target.PublicMattermost {
		return mattermostRoutes, nil
	}
	return nil, fmt.Errorf("public: profile %q names no provider this server knows", prof.Name())
}

// ChannelPublic says a channel is public, asking the upstream about it once a
// minute at most. A channel the credential may not see, or that is not there,
// is not public; an upstream that fails to say is an error, not an answer.
func (g *Guard) ChannelPublic(ctx context.Context, prof *target.Profile, id string) (bool, error) {
	if !mattermostID.MatchString(id) {
		return false, nil
	}
	key := cacheKey{prof.Name(), id}
	g.mu.Lock()
	c, ok := g.cache[key]
	g.mu.Unlock()
	if ok && g.now().Before(c.expires) {
		return c.public, nil
	}

	public, err := g.lookup(ctx, prof, id)
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	if len(g.cache) >= maxCached {
		clear(g.cache)
	}
	g.cache[key] = cached{public: public, expires: g.now().Add(cacheTTL)}
	g.mu.Unlock()
	return public, nil
}

func (g *Guard) lookup(ctx context.Context, prof *target.Profile, id string) (bool, error) {
	resp, err := g.client.Do(ctx, prof.Upstream(), upstream.Request{
		Method: http.MethodGet, Path: "/api/v4/channels/" + id, MaxBytes: lookupMaxBytes,
	})
	switch {
	case err != nil:
		return false, err
	case resp.Status == http.StatusForbidden, resp.Status == http.StatusNotFound:
		return false, nil
	case resp.Status != http.StatusOK:
		return false, &UpstreamError{Status: resp.Status, Err: fmt.Errorf("checking channel %s: the upstream answered %d", id, resp.Status)}
	case resp.Truncated:
		return false, &UpstreamError{Status: resp.Status, Err: fmt.Errorf("checking channel %s: the answer is over %d bytes", id, lookupMaxBytes)}
	}
	ch, err := decodeChannel(resp.Body)
	if err != nil {
		return false, &UpstreamError{Status: resp.Status, Err: fmt.Errorf("checking channel %s: %w", id, err)}
	}
	if ch.ID != id {
		return false, &UpstreamError{Status: resp.Status, Err: fmt.Errorf("checking channel %s: the upstream answered about %q", id, ch.ID)}
	}
	return ch.public(), nil
}
