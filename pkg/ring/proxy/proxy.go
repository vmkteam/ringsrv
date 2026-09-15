// Package proxy performs one allow-listed HTTP call to a target and shapes
// what came back: the jq filter, the PII rules of the target and the size
// limit, in that order.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// Manager performs the calls. Whether the caller may make one is decided before
// this — by the role, the catalogue and the write barrier — so what is left here
// is doing it and cutting the answer to size.
type Manager struct {
	client    *upstream.Client
	jqTimeout time.Duration
	maxBytes  int
}

// NewManager wires the upstream client with the instance-wide defaults.
func NewManager(client *upstream.Client, jqTimeout time.Duration, maxBytes int) *Manager {
	return &Manager{client: client, jqTimeout: jqTimeout, maxBytes: maxBytes}
}

// Configured reports whether calls can be made at all.
func (p *Manager) Configured() bool { return p != nil && p.client != nil }

// Request is one call to a target.
type Request struct {
	Method string
	Path   string
	Body   string
	// Headers are the ones the call brought and the profile allowed.
	Headers map[string]string
	// JQ overrides the profile's default filter.
	JQ string
	// MaxBytes overrides the profile's cap and the instance default.
	MaxBytes int
}

// Answer is a successful call. Either Data or Text carries the body: Text is
// what an oversized or non-JSON answer travels as, because half a JSON document
// is a parse error rather than a smaller answer.
type Answer struct {
	Status     int
	Data       any
	Text       string
	AsText     bool
	Truncated  bool
	BytesTotal int
	Redacted   redact.Result
	// DefaultJQ is set when the profile's filter ran because the request brought
	// none: a trimmed answer has to be distinguishable from the whole thing.
	DefaultJQ bool
}

// Kinds of failure, so the layer above maps one value to its error code and
// metric label instead of re-deriving the reason from a string.
const (
	FailBadArgs  = "bad_args"
	FailTimeout  = "timeout"
	FailUpstream = "upstream"
	FailJQ       = "jq"
)

// Error is a call that did not produce an answer.
type Error struct {
	Kind string
	Err  error
	// Status is the upstream status when there was one.
	Status int
	// Keys are the top-level keys of the body, filled when a jq expression found
	// nothing: the next attempt needs to know what was there.
	Keys []string
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Do performs the call and shapes the answer.
func (p *Manager) Do(ctx context.Context, prof *target.Profile, req Request) (*Answer, *Error) {
	if !p.Configured() {
		return nil, &Error{Kind: FailUpstream, Err: errors.New("upstream client is not configured")}
	}

	// Relative time is expanded by the profile: what the model may write and
	// what a target expects is a catalogue decision, not a transport one.
	path, err := prof.ExpandPath(req.Path, time.Now())
	if err != nil {
		return nil, &Error{Kind: FailBadArgs, Err: err}
	}
	// Fixed parameters go on last, so a value the model wrote for the same key
	// cannot survive: the contour is not negotiable per call.
	path = prof.ForceQuery(path)
	// The same for a window that travels in the body. Rewritten after the
	// allowlist, which saw the body as written.
	body, err := prof.ExpandBody(req.Body, time.Now())
	if err != nil {
		return nil, &Error{Kind: FailBadArgs, Err: err}
	}

	resp, err := p.client.Do(ctx, prof.Upstream(), upstream.Request{
		Method: req.Method, Path: path, Body: body, Headers: req.Headers,
	})
	switch {
	case errors.Is(err, upstream.ErrTimeout):
		return nil, &Error{Kind: FailTimeout, Err: err}
	case err != nil:
		return nil, &Error{Kind: FailUpstream, Err: err}
	case resp.Status >= 300 && resp.Status < 400:
		// Not followed: a redirect would carry the profile's credentials to
		// wherever the upstream pointed. The model gets the destination.
		return nil, &Error{
			Kind:   FailUpstream,
			Err:    fmt.Errorf("upstream answered %d with a redirect to %q; redirects are not followed", resp.Status, resp.Location),
			Status: resp.Status,
		}
	case resp.Status >= 400:
		// The first bytes of a failing body reach the model too, and a 4xx from
		// Sentry can quote the very event it refused: redaction applies here as
		// to any answer.
		msg, _ := redact.Text(clip(resp.Body, upstreamErrBodyBytes), redact.Options{Mode: prof.Redact, Rules: prof.RedactRules})
		return nil, &Error{
			Kind:   FailUpstream,
			Err:    fmt.Errorf("upstream answered %d: %s", resp.Status, msg),
			Status: resp.Status,
		}
	}

	return p.shape(ctx, prof, req, resp)
}

// shape runs the filter, the redaction and the size limit over a successful
// response — ring.ShapeJSON, shared with the database tools — plus the one case
// an HTTP answer has of its own: a body that is not JSON.
func (p *Manager) shape(ctx context.Context, prof *target.Profile, req Request, resp *upstream.Response) (*Answer, *Error) {
	expr := req.JQ
	byDefault := expr == "" && prof.DefaultJQ != ""
	if expr == "" {
		expr = prof.DefaultJQ
	}
	limit := ring.PickLimit(req.MaxBytes, prof.MaxBytes, p.maxBytes)

	shaped, err := ring.ShapeJSON(ctx, resp.Body, expr, p.jqTimeout, prof.Redact, prof.RedactRules, limit)
	switch {
	case errors.Is(err, ring.ErrJQNotJSON):
		// A backend answering in its own format still answered, and refusing
		// would hide the message that explains what happened. Plain text is
		// still text about somebody, so redaction runs first.
		text, red := redact.Text(string(resp.Body), redact.Options{Mode: prof.Redact, Rules: prof.RedactRules})
		text, truncated, total := mcp.Truncate(text, limit)
		return &Answer{
			Status: resp.Status, Text: text, AsText: true,
			Truncated: truncated || resp.Truncated, BytesTotal: total, Redacted: red,
		}, nil
	case errors.Is(err, ring.ErrJQCompile), errors.Is(err, ring.ErrJQRun):
		return nil, &Error{Kind: FailJQ, Err: err, Status: resp.Status, Keys: ring.TopLevelKeys(resp.Body)}
	case err != nil:
		return nil, &Error{Kind: FailUpstream, Err: err, Status: resp.Status}
	}
	return &Answer{
		Status: resp.Status, Data: shaped.Data, Text: shaped.Text, AsText: shaped.AsText,
		Truncated: shaped.Truncated || resp.Truncated, BytesTotal: shaped.BytesTotal,
		Redacted: shaped.Redacted, DefaultJQ: byDefault,
	}, nil
}

// upstreamErrBodyBytes caps how much of a failing body travels in the error:
// enough to see the complaint, not a whole HTML error page.
const upstreamErrBodyBytes = 512

func clip(body []byte, n int) string {
	if len(body) <= n {
		return string(body)
	}
	return string(body[:n]) + "…"
}
