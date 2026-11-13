package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/files"
	"github.com/vmkteam/ringsrv/pkg/ring/public"

	"github.com/stretchr/testify/assert"
)

// The file route and the public-only check fail in the same few ways, and an
// upstream that answered keeps its status whichever of the two met it — the
// file route checks a Mattermost channel through the public package.
func TestRouteError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		kind   string
		code   string
		status int
	}{
		"file refused":       {&files.RefusedError{Code: files.CodeTooLarge, Message: "big"}, FailRefused, files.CodeTooLarge, 0},
		"public refused":     {&public.RefusedError{Code: public.CodeNotPublic, Message: "private"}, FailRefused, public.CodeNotPublic, 0},
		"file upstream":      {&files.UpstreamError{Status: http.StatusNotFound, Err: errors.New("gone")}, FailUpstream, "", http.StatusNotFound},
		"channel lookup 500": {fmt.Errorf("wrapped: %w", &public.UpstreamError{Status: http.StatusInternalServerError, Err: errors.New("down")}), FailUpstream, "", http.StatusInternalServerError},
		"bad params":         {fmt.Errorf("%w: x", files.ErrBadParams), FailBadArgs, "", 0},
		"timeout":            {fmt.Errorf("%w: slow", upstream.ErrTimeout), FailTimeout, "", 0},
		"other":              {errors.New("boom"), FailUpstream, "", 0},
	} {
		got := routeError(tc.err)
		assert.Equal(t, tc.kind, got.Kind, name)
		assert.Equal(t, tc.code, got.Code, name)
		assert.Equal(t, tc.status, got.Status, name)
	}
}
