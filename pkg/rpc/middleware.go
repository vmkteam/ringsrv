package rpc

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/vmkteam/appkit"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/zenrpc/v2"
)

// withCallLog logs every MCP call: who, which method, how long, and the outcome
// — not the params. zenrpc-middleware's WithSLog logs them whole, and for
// tools/call the params are the arguments: the body of a write, an issue text, a
// comment on its way to YouTrack. That would turn the shared log into a copy of
// everything written through this service, readable by anyone with Loki.
//
// An error with an internal code is also reported to Sentry and its text
// replaced before it leaves: an internal message is a stack of our own paths and
// upstream names, not something a client is owed.
func withCallLog(logger embedlog.Logger, serverName string) zenrpc.MiddlewareFunc {
	return func(h zenrpc.InvokeFunc) zenrpc.InvokeFunc {
		return func(ctx context.Context, method string, params json.RawMessage) zenrpc.Response {
			start := time.Now()
			r := h(ctx, method, params)
			took := time.Since(start)

			name := zenrpc.NamespaceFromContext(ctx) + "." + method
			if serverName != "" {
				name = serverName + "." + name
			}
			args := make([]any, 0, logArgsCap)
			args = append(args,
				"ip", appkit.IPFromContext(ctx),
				logKeyMethod, name,
				"duration", took.String(),
				"durationMS", took.Milliseconds(),
				"userAgent", appkit.UserAgentFromContext(ctx),
				"xRequestId", appkit.XRequestIDFromContext(ctx),
			)
			if r.Error == nil {
				logger.Print(ctx, "rpc", args...)
				return r
			}

			args = append(args, "err", r.Error)
			if !isInternal(r.Error) {
				logger.Print(ctx, "rpc", args...)
				return r
			}
			logger.Error(ctx, "rpc error", args...)
			if hub := sentry.GetHubFromContext(ctx); hub != nil {
				hub.Scope().SetTag(logKeyMethod, name)
				hub.CaptureException(r.Error)
			}
			r.Error.Err = nil
			r.Error.Message = "Internal error"
			return r
		}
	}
}

// logKeyMethod is the one key the call log, the Sentry tag and the audit record
// share. What it names differs by line — the zenrpc method here, the HTTP verb
// in a record — but the question is the same: which method was this.
const logKeyMethod = "method"

// logArgsCap is the number of key/value slots a line needs: six pairs plus the
// error. The linter requires the hint; keep it in step with the append above.
const logArgsCap = 14

// isInternal is the zenrpc convention: 500 and every negative code are ours,
// everything else is the caller's.
func isInternal(e *zenrpc.Error) bool {
	return e.Code == http.StatusInternalServerError || e.Code < 0
}
