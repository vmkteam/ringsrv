// Package upstream performs the actual HTTP call to a target API. It knows how
// to speak HTTP to something described by a Target, and nothing about
// catalogues, roles or tools.
//
// Everything the model controls — method, path, query, body — is checked against
// the profile before it gets here, and nothing it sends influences the
// destination: the host comes from the profile and only from there. The one
// exception is Download, whose host may also be one the caller fixed in code.
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vmkteam/appkit"
)

// CleanPath strips the query and the fragment and rejects anything that is not a
// plain absolute path: a scheme, a protocol-relative host or a ".." would send
// the request somewhere the allowlist never approved.
//
// The decoded form is checked too: the path travels as written, but the server
// on the other end decodes it before routing, so "%2e%2e" is ".." to it whatever
// the allowlist saw. The catalogue calls this before matching its patterns, so
// both decisions are made on the same string.
func CleanPath(raw string) (string, bool) {
	p, _, _ := strings.Cut(raw, "?")
	p, _, _ = strings.Cut(p, "#")
	if p == "" || !strings.HasPrefix(p, "/") || !plainPath(p) {
		return "", false
	}
	decoded, err := url.PathUnescape(p)
	if err != nil || !plainPath(decoded) {
		return "", false
	}
	return p, true
}

// plainPath rejects what would move a request off the approved path: parent and
// dot segments, empty segments (how a protocol-relative host starts),
// backslashes and control characters.
func plainPath(p string) bool {
	if strings.Contains(p, "..") || strings.Contains(p, "//") ||
		strings.Contains(p, "/./") || strings.HasSuffix(p, "/.") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	return true
}

// Target is everything this package needs to make one call. The catalogue builds
// it; the role that allowed it is none of this package's business.
type Target struct {
	// Name labels the metrics and appears in errors.
	Name string
	// BaseURL decides where the request goes, and it is the only thing that
	// does: nothing from the caller may influence the host.
	BaseURL string
	// PathPrefix goes before every path of Do: an API mounted under a path of
	// its host. The catalogue checked it; the path after it is the caller's.
	PathPrefix string
	// Headers are "Name: value" pairs with credentials already substituted.
	Headers []string
}

// Response is what a target answered, after reading at most MaxBytes of body.
type Response struct {
	Status int
	Body   []byte
	// Truncated reports that the body was longer than the read limit — not the
	// same thing as the truncation of the final answer.
	Truncated bool
	// Location is where a 3xx answer pointed. Redirects are never followed (see
	// New); the caller reports the destination instead of going there.
	Location string
	// ContentType is the answer's own claim about its body. A download checks
	// it: a login page served with 200 is still text/html.
	ContentType string
}

// ErrTimeout separates "upstream is slow" from "upstream said no": the two need
// different answers from the tool and different reactions from a human.
var ErrTimeout = errors.New("upstream: timeout")

// Options configure the client. Zero values mean "no limit", which is only
// sane in tests.
type Options struct {
	// AppName and Version identify us to the upstream and label the metrics.
	AppName string
	Version string

	Timeout       time.Duration
	MaxConcurrent int
	// MaxBodyBytes caps what is read from a response. Deliberately larger than
	// the answer limit: cutting before jq would throw away what the filter was
	// for.
	MaxBodyBytes int64
}

// Client calls upstreams on behalf of api_call.
type Client struct {
	http *http.Client
	opts Options
	sem  chan struct{}
}

// New builds the client. Metrics come from appkit's transport, so the existing
// dashboards work.
//
// The transport itself is ours rather than appkit.NewHTTPClient's, for two
// reasons. Its idle pool is sized to MaxConcurrent, where http.DefaultTransport
// keeps two per host and eight parallel calls would pay the TLS handshake again.
// And it carries no header policy: NewInternalHeaders is for vmkteam services
// talking to each other, and it adds headers rather than setting them, which
// would duplicate one a profile already declared.
func New(opts Options) *Client {
	c := &Client{
		http: &http.Client{
			Timeout:   opts.Timeout,
			Transport: newTransport(opts.MaxConcurrent),
			// Never follow a redirect: the allowlist approved one host, and Go
			// forwards every custom header to the next one — PRIVATE-TOKEN,
			// X-Nomad-Token — dropping only Authorization and Cookie. The 3xx
			// comes back with its Location instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		opts: opts,
	}
	if opts.MaxConcurrent > 0 {
		c.sem = make(chan struct{}, opts.MaxConcurrent)
	}
	return c
}

// newTransport clones the stdlib default and gives it a pool that matches how
// many calls we allow at once.
func newTransport(maxConcurrent int) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return appkit.WithMetricsTransport(http.DefaultTransport)
	}
	clone := t.Clone()
	if maxConcurrent > 0 {
		clone.MaxIdleConnsPerHost = maxConcurrent
	}
	return appkit.WithMetricsTransport(clone)
}

// HTTP exposes the underlying client for callers that need the same transport —
// the OIDC provider, whose refreshes should be counted like any other request.
// A nil receiver answers nil, which OIDCConfig reads as "use the default".
func (c *Client) HTTP() *http.Client {
	if c == nil {
		return nil
	}
	return c.http
}

// Request is one call to a target. Path is raw, query included; it must
// already have passed the profile's allowlist.
type Request struct {
	Method string
	Path   string
	Body   string
	// Headers are the names the profile allowed a call to set, already checked
	// above. Written before the target's own, so a profile header always wins.
	Headers map[string]string
	// MaxBytes overrides the client's read limit for this one call — a call
	// the server makes itself, whose answer it reads whole before cutting it.
	MaxBytes int64
}

// Do performs one call.
func (c *Client) Do(ctx context.Context, t Target, r Request) (*Response, error) {
	method, path, body := r.Method, r.Path, r.Body
	if t.PathPrefix != "" && strings.HasPrefix(path, "/") {
		path = t.PathPrefix + path
	}
	url, err := buildURL(t.BaseURL, path)
	if err != nil {
		return nil, err
	}

	// The caller label turns the shared metrics into per-target ones, without a
	// metric of our own.
	ctx = appkit.NewCallerNameContext(ctx, t.Name)

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, fmt.Errorf("upstream: build request: %w", err)
	}
	// The call's headers go on first, the profile's over them: the order is what
	// keeps a catalogue credential from being displaced, without depending on
	// the layer above being right about which names collide.
	for name, value := range r.Headers {
		req.Header.Set(name, value)
	}
	for _, h := range t.Headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, r.MaxBytes)
}

// DownloadOptions bound a Download: where it may go and how much it may read.
type DownloadOptions struct {
	// Hosts are the hosts the URL may name besides the target's own, over
	// https. The caller fixes them in code: never from the model, never from
	// the answer that named the URL.
	Hosts []string
	// MaxBytes caps the body; zero leaves the client's limit.
	MaxBytes int64
}

// Download fetches an absolute URL an upstream answer named — the bytes of a
// file behind the API — carrying the target's Authorization and nothing else of
// the target's. The URL must be on the target's own scheme and host, or https
// on one of opts.Hosts; redirects are not followed, as for Do.
//
// This is the one way a request leaves for a host other than the profile's,
// and it is why the destination is checked here rather than trusted: the URL
// came out of an answer, and an answer is upstream data.
func (c *Client) Download(ctx context.Context, t Target, rawURL string, opts DownloadOptions) (*Response, error) {
	u, err := downloadURL(t.BaseURL, rawURL, opts.Hosts)
	if err != nil {
		return nil, err
	}
	ctx = appkit.NewCallerNameContext(ctx, t.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("upstream: build request: %w", err)
	}
	// The credential alone: whatever else the profile sends — a contour pin, a
	// forward-auth secret — is for the API host, not for a file host.
	for _, h := range t.Headers {
		name, value, ok := strings.Cut(h, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Authorization") {
			req.Header.Set("Authorization", strings.TrimSpace(value))
		}
	}
	return c.send(req, opts.MaxBytes)
}

// send makes the request under the concurrency limit and reads the answer up to
// maxBytes, or up to the client's limit when that is zero.
func (c *Client) send(req *http.Request, maxBytes int64) (*Response, error) {
	release, err := c.acquire(req.Context())
	if err != nil {
		return nil, err
	}
	defer release()

	// Only when the profile did not decide otherwise: an upstream's log has to
	// tell us apart from everything else holding the same token.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent())
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if isTimeout(err) {
			return nil, fmt.Errorf("%w after %s", ErrTimeout, c.opts.Timeout)
		}
		return nil, fmt.Errorf("upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	limit := c.opts.MaxBodyBytes
	if maxBytes > 0 {
		limit = maxBytes
	}
	data, truncated, err := readBody(resp.Body, limit, resp.ContentLength)
	if err != nil {
		if isTimeout(err) {
			return nil, fmt.Errorf("%w after %s", ErrTimeout, c.opts.Timeout)
		}
		return nil, fmt.Errorf("upstream: read body: %w", err)
	}

	return &Response{
		Status: resp.StatusCode, Body: data, Truncated: truncated,
		Location: resp.Header.Get("Location"), ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func (c *Client) userAgent() string {
	if c.opts.AppName == "" {
		return "ringsrv"
	}
	return c.opts.AppName + " (Version:" + c.opts.Version + ")"
}

// Slot takes a slot in the instance's concurrency limit for a call that does
// not go through Do — a session with a store over another protocol — so that
// it counts against the same limit. The release has to run.
func (c *Client) Slot(ctx context.Context) (func(), error) { return c.acquire(ctx) }

// Timeout is what one call may take, for a caller that bounds its own.
func (c *Client) Timeout() time.Duration { return c.opts.Timeout }

// acquire takes a slot in the concurrency limit and returns the release, which
// runs on every path out: a leaked slot degrades the whole instance and looks
// like an unrelated slowdown.
func (c *Client) acquire(ctx context.Context) (func(), error) {
	if c.sem == nil {
		return func() {}, nil
	}
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w waiting for a slot", ErrTimeout)
	}
}

// readBody reads at most limit bytes and reports whether there was more. One
// byte past the limit is the only way to tell "just fits" from "cut off".
//
// size is the length the answer announced, -1 for none. A body of megabytes
// read into a growing buffer is copied over and over; one of the announced
// size, bounded by the limit, is filled once.
func readBody(r io.Reader, limit, size int64) ([]byte, bool, error) {
	if limit <= 0 {
		data, err := io.ReadAll(r)
		return data, false, err
	}
	var buf bytes.Buffer
	if size > 0 {
		buf.Grow(int(min(size, limit)) + bytes.MinRead)
	}
	if _, err := buf.ReadFrom(io.LimitReader(r, limit+1)); err != nil {
		return nil, false, err
	}
	data := buf.Bytes()
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var terr interface{ Timeout() bool }
	return errors.As(err, &terr) && terr.Timeout()
}
