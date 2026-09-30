package rpc

import "strings"

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
	case "tools/call":
		return name == ToolHelp || name == ToolRepoMap
	case "resources/read", "prompts/get",
		"initialize", "ping", "server/discover",
		"tools/list", "resources/list", "resources/templates/list", "prompts/list":
		return true
	}
	return strings.HasPrefix(method, "notifications/")
}
