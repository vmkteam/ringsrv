package rpc

import (
	"context"
	"strings"
	"time"

	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/ratelimit"
)

// Budget is the caller's hourly work budget as an answer leaves it: spent, the
// ceiling, what is left and when the window rolls. A long investigation reads
// it to narrow its queries before it runs into the ceiling rather than after —
// a refusal says the same, but only once it is too late.
//
// Durations are written the way a person reads them, "12m30s", because the
// reader is the model; a machine gets retry_after in seconds on the refusal.
type Budget struct {
	Used    string `json:"used"`
	Limit   string `json:"limit"`
	Left    string `json:"left"`
	ResetIn string `json:"reset_in"`
}

// budgetOf is the budget for an answer, this call's own charges included, or
// nil with the budget off — the field is then left out rather than zero.
func budgetOf(ctx context.Context) *Budget {
	b, ok := ratelimit.Remaining(ctx)
	if !ok {
		return nil
	}
	return &Budget{
		Used:    roughly(b.Used),
		Limit:   roughly(b.Limit),
		Left:    roughly(b.Left()),
		ResetIn: roughly(time.Until(b.ResetAt)),
	}
}

// roughly writes d in whole seconds without the zero tails: 20m rather than
// 20m0s, 1h rather than 1h0m0s.
func roughly(d time.Duration) string {
	s := max(0, d).Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// methodToolsCall is the MCP method every tool call arrives as.
const methodToolsCall = "tools/call"

// ExemptFromBudget names the calls the hourly budget does not apply to — it is
// ratelimit.Config.Exempt. They cost nothing, and they are what a spent budget
// used to take away first: the cheat sheets that teach a caller to spend less,
// and the handshake and lists a reconnecting bridge needs before it can ask
// for them. Refused, a spent budget was a server that would not even connect.
//
// The budget is all they are spared: every one still takes a rate token and a
// concurrency slot.
func ExemptFromBudget(method, name string) bool {
	switch method {
	case methodToolsCall:
		return name == ToolHelp || name == ToolRepoMap
	case "resources/read", "prompts/get",
		mcpkit.MethodInitialize, "ping", mcpkit.MethodDiscover,
		"tools/list", "resources/list", "resources/templates/list", "prompts/list":
		return true
	}
	return strings.HasPrefix(method, "notifications/")
}
