package rpc

import "github.com/vmkteam/mcpkit/mcp"

// Single source of truth for rpc-layer types: tool argument structs with their
// reflected JSON schemas, and the domain shapes the tools answer with. The
// protocol DTOs live in mcp — one mirror of the wire format for three services.

// --- tools ------------------------------------------------------------------

// APICallArgs are the inputs to the api_call tool. One tool for every upstream:
// a catalogue of paths is cheaper to keep in config than in dozens of schemas
// that only change with a server release.
//
// A list rather than one call, with no scalar form beside it: the client
// dispatches calls one at a time and waits for each answer, so N independent
// questions cost N round trips. In one day's log, 211 of 337 dispatches were
// fan-outs over ids the model already had in hand.
type APICallArgs struct {
	// Calls carry the headers argument; apiCallBatchBase is the same list
	// without it.
	Calls []APICall `json:"calls" jsonschema:"required" jsonschema_description:"Independent calls, run at once; results[] in this order. One call is a list of one."`
	// MaxBytes is the budget for the whole answer rather than for each call in
	// it: twenty calls at the per-target default would be megabytes of context.
	MaxBytes int `json:"max_bytes,omitempty" jsonschema_description:"Budget for the whole batch, bytes; max 262144. Tighten jq first."`
}

// apiCallBatchBase is APICallArgs without the headers argument: its own type so
// tools/list can hand a role whose targets all refuse headers a schema without
// bytes it cannot spend.
type apiCallBatchBase struct {
	Calls    []apiCallBase `json:"calls" jsonschema:"required" jsonschema_description:"Independent calls, run at once; results[] in this order. One call is a list of one."`
	MaxBytes int           `json:"max_bytes,omitempty" jsonschema_description:"Budget in bytes for the whole batch; max 262144. Tighten jq first."`
}

// APICall is one call inside the batch.
type APICall struct {
	apiCallBase
	// Headers is the escape hatch for an upstream whose per-caller credential
	// cannot live in the catalogue. The profile's AllowHeaders decides which
	// names pass; an unlisted one is an error, not a silent drop.
	Headers map[string]string `json:"headers,omitempty" jsonschema_description:"Only where the target allows; the catalogue fixes which names."`
}

// apiCallBase is one call without the headers.
type apiCallBase struct {
	Target string `json:"target" jsonschema:"required" jsonschema_description:"Name from the list above."`
	Method string `json:"method" jsonschema:"required,enum=GET,enum=POST,enum=PUT" jsonschema_description:"POST/PUT only where the target allows."`
	Path   string `json:"path" jsonschema:"required" jsonschema_description:"From /, query allowed; no absolute URLs."`
	Body   string `json:"body,omitempty" jsonschema_description:"JSON body (POST/PUT)."`
	JQ     string `json:"jq,omitempty" jsonschema_description:"Omitted = target default (default_jq:true in answer); a dot = raw. Lists: [.[]|{ID,Status}]."`
	Intent string `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited. Required for writes."`
}

// APICallBatch is the answer of api_call: one result per call, in the order
// sent. Env comes from the instance config and never from an argument, so an
// answer always says which contour it is about; it and the trace sit on the
// batch rather than on every item.
type APICallBatch struct {
	Env string `json:"env"`
	// TraceID stitches a skill's report to the calls behind it: the report
	// carries `Trace: ringsrv/…`, the log carries the same value.
	TraceID string        `json:"trace_id,omitempty"`
	Results []APICallItem `json:"results"`
	// Truncated says at least one answer is incomplete — cut to its own limit or
	// dropped by the batch budget — without scanning the list for it.
	Truncated bool `json:"truncated,omitempty"`
	// Budget is the hourly work budget with this batch paid for.
	Budget *Budget `json:"budget,omitempty"`
}

// APICallItem is what one call in the batch produced — either an answer or the
// reason there is none. One failed call does not fail the batch: an error makes
// the model resend the whole list, answered items included.
type APICallItem struct {
	Index  int    `json:"index"`
	Target string `json:"target"`
	Status int    `json:"status,omitempty"`
	// DurationMS is this call's own round trip; the batch ran them at once, so
	// the sum of these is the work done and the wall clock is not.
	DurationMS int64 `json:"duration_ms,omitempty"`
	Truncated  bool  `json:"truncated,omitempty"`
	BytesTotal int   `json:"bytes_total,omitempty"`
	// DefaultJQ says the target's own filter shaped this answer. Without it a
	// trimmed Sentry issue looks whole and the model never asks for the rest.
	DefaultJQ bool `json:"default_jq,omitempty"`
	Data      any  `json:"data,omitempty"`
	// Redacted names the PII rules that matched: in warn mode the only sign, in
	// redact mode what was replaced, so an absent field is not absent data.
	Redacted []string `json:"redacted,omitempty"`
	// DedupOf points at the earlier item this answer was copied from.
	DedupOf *int `json:"dedup_of,omitempty"`
	// Error carries the refusal or the failure of this one call.
	Error *ToolError `json:"error,omitempty"`
}

// ToolError is the error envelope shared by every tool. With so few tools the
// error *is* the documentation: each code carries what the caller needs to fix
// the call on the next try.
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Env is omitted inside a batch: the contour is named once, on the batch.
	Env     string   `json:"env,omitempty"`
	Status  int      `json:"status,omitempty"`
	Targets []string `json:"targets,omitempty"`
	Repos   []string `json:"repos,omitempty"`
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	// RPCMethods are the JSON-RPC methods the target does allow in a body,
	// sent with a refusal of one it does not.
	RPCMethods []string `json:"rpc_methods,omitempty"`
	// QueryParams are the query parameters the target does take, bounds included
	// as written ("limit<=100"). Empty on a target that takes any name.
	QueryParams []string `json:"query_params,omitempty"`
	// Headers are the names the target does allow a call to set, sent with a
	// refusal of one it does not.
	Headers []string `json:"headers,omitempty"`
	// Keys are the top-level keys of the body a jq expression failed on: the
	// filter gets fixed on the next try instead of guessed.
	Keys []string `json:"keys,omitempty"`
	// Sheets are the cheat sheets the caller may ask help for, sent with a
	// refusal of a name it may not.
	Sheets []string `json:"sheets,omitempty"`
	// Help is the cheat sheet of the target a refused call was about: a tool
	// result reaches every client where a resource does not.
	Help string `json:"help,omitempty"`
}

// Error lets a decided refusal travel back through a plain error return
// alongside errors from git or an upstream.
func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// The two schemas api_call is offered with. Which one a role sees is decided
// in List by whether any of its targets allows a header at all.
var (
	apiCallInputSchema        = mcp.SchemaFor(apiCallBatchBase{})
	apiCallInputSchemaHeaders = mcp.SchemaFor(APICallArgs{})
)

// RepoMapArgs are the inputs to repo_map. Without a name the answer is the whole
// table; named rows gain the layer map and the exclusions.
type RepoMapArgs struct {
	Repos []string `json:"repos,omitempty" jsonschema_description:"Names from the table, up to 10, with layers and exclusions; omit for the whole table."`
}

// RepoEntry is one row of the map: what a repository is called in the other
// targets, which the model needs before it can ask Sentry or Nomad anything.
type RepoEntry struct {
	Repo          string `json:"repo"`
	SentrySlug    string `json:"sentry_slug,omitempty"`
	NomadJob      string `json:"nomad_job,omitempty"`
	PromJob       string `json:"prom_job,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	GitLabProject int    `json:"gitlab_project,omitempty"`
	// Description is the hand-written architecture note: layers, pitfalls, what
	// this service depends on.
	Description string `json:"description,omitempty"`
	// Layers and Exclude are only filled when a repository was named: in a table
	// they would cost more than they explain.
	Layers  map[string]string `json:"layers,omitempty"`
	Exclude []string          `json:"exclude,omitempty"`
}

// RepoMapResult is the answer of repo_map.
type RepoMapResult struct {
	Env   string      `json:"env"`
	Repos []RepoEntry `json:"repos"`
}

var repoMapInputSchema = mcp.SchemaFor(RepoMapArgs{})
