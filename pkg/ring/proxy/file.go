package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/files"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
)

// file answers the file route. What comes back is this server's own JSON, so
// the profile's DefaultJQ — written for the upstream's answers — does not run
// over it; a jq the call brought does, and so do the redaction and the size
// limit, as for any answer. A picture leaves its bytes on the answer for a
// content block, and the JSON describes the file.
func (p *Manager) file(ctx context.Context, prof *target.Profile, req Request) (*Answer, *Error) {
	limit := ring.PickLimit(req.MaxBytes, prof.MaxBytes, p.maxBytes)
	var (
		res *files.Result
		err error
	)
	if route := prof.StoreRoute(req.Method, req.Path); route != "" {
		res, err = p.files.Store(ctx, prof, route, req.Path, limit)
	} else {
		res, err = p.files.Fetch(ctx, prof, req.Path, limit)
	}
	if err != nil {
		return nil, fileError(err)
	}

	var doc any = res.Text
	if res.Image != nil {
		doc = res.Meta
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, &Error{Kind: FailUpstream, Err: err}
	}
	shaped, err := ring.ShapeJSON(ctx, body, req.JQ, p.jqTimeout, prof.Redact, prof.RedactRules, limit)
	switch {
	case errors.Is(err, ring.ErrJQCompile), errors.Is(err, ring.ErrJQRun):
		return nil, &Error{Kind: FailJQ, Err: err, Status: http.StatusOK, Keys: ring.TopLevelKeys(body)}
	case err != nil:
		return nil, &Error{Kind: FailUpstream, Err: err}
	}
	return &Answer{
		Status: http.StatusOK, Data: shaped.Data, Text: shaped.Text, AsText: shaped.AsText,
		Truncated: shaped.Truncated || (res.Text != nil && res.Text.Truncated), BytesTotal: shaped.BytesTotal,
		Redacted: shaped.Redacted, File: &res.Meta, Image: res.Image,
	}, nil
}

// fileError maps what the route returned onto the kinds the layer above knows.
// A failure the upstream answered with keeps its status: an upstream that said
// no is not a failure of this server, and the layer above logs only those.
func fileError(err error) *Error {
	var (
		ref *files.RefusedError
		up  *files.UpstreamError
	)
	switch {
	case errors.As(err, &ref):
		return &Error{Kind: FailRefused, Code: ref.Code, Err: err}
	case errors.As(err, &up):
		return &Error{Kind: FailUpstream, Err: err, Status: up.Status}
	case errors.Is(err, files.ErrBadParams):
		return &Error{Kind: FailBadArgs, Err: err}
	case errors.Is(err, upstream.ErrTimeout):
		return &Error{Kind: FailTimeout, Err: err}
	default:
		return &Error{Kind: FailUpstream, Err: err}
	}
}
