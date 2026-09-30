package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/textproto"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/codegraph"
	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/code"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/proxy"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/auth"
	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/mcptool"
	"github.com/vmkteam/mcpkit/ratelimit"
	"github.com/vmkteam/zenrpc/v2"
)

// ToolAPICall is one HTTP call to one allow-listed upstream.
const ToolAPICall = "api_call"

// ToolRepoMap answers "what is this service called in the other targets":
// apisrv is `apisrv` in Sentry and `apisrv-prod` in Nomad.
const ToolRepoMap = "repo_map"

// api_call error codes. With a single tool the error is the documentation —
// each code comes with the data needed to fix the next call.
const (
	ErrCodeTargetUnknown    = "TargetUnknown"
	ErrCodeForbiddenRole    = "ForbiddenRole"
	ErrCodeBadArgs          = "BadArgs"
	ErrCodeMethodNotAllowed = "MethodNotAllowed"
	ErrCodePathNotAllowed   = "PathNotAllowed"
	ErrCodeHeaderNotAllowed = "HeaderNotAllowed"
	// ErrCodeRPCMethodNotAllowed is PathNotAllowed for an API behind one path:
	// the method in the JSON-RPC body is not in the profile's list.
	ErrCodeRPCMethodNotAllowed = "RPCMethodNotAllowed"
	// ErrCodeQueryParamNotAllowed: CheckQuery refused the query string, or
	// CheckBody found a credential among the body's keys.
	ErrCodeQueryParamNotAllowed = "QueryParamNotAllowed"
	ErrCodeUpstream             = "Upstream"
	ErrCodeTimeout              = "Timeout"
	ErrCodeJQFailed             = "JQFailed"
	ErrCodeIntentRequired       = "IntentRequired"
	ErrCodeWriteBudget          = "WriteBudget"
	// ErrCodeWriteNotBatched refuses a write sent inside a list of several
	// calls. Its own code rather than IntentRequired: the intent may well be
	// there, and adding one would not help.
	ErrCodeWriteNotBatched = "WriteNotBatched"
	// ErrCodeTruncated marks an answer the batch budget could not carry: narrow
	// the jq, raise max_bytes or ask for that one on its own.
	ErrCodeTruncated = "Truncated"
)

// ErrNoRole is returned when the caller's groups match no role. It must not
// look like an empty catalogue: a user who sees zero targets debugs the server,
// a user who sees 403 asks for access.
var ErrNoRole = zenrpc.NewStringError(http.StatusForbidden, "no ringsrv role matches your groups")

// ToolsDeps bundles dependencies for ToolsService.
type ToolsDeps struct {
	// Targets carries both the profiles and the contour they belong to.
	Targets  *target.Catalog
	Upstream *upstream.Client
	Repos    *git.Store
	// Graph is the AST engine. Nil on an instance without it: the tools that
	// need it then refuse by name instead of answering emptiness.
	Graph *codegraph.Client
	// DB serves the database targets. Nil when the catalogue names none: the
	// tools then stay out of the list.
	DB *dbq.Manager
	// Docs is the markdown library help and the refusals read cheat sheets from.
	Docs *doc.Library
	// MaxBytes is the answer size limit used when neither the call nor the
	// profile sets one.
	MaxBytes int
	// MaxConcurrent is the upstream client's own limit, which a batch must not
	// exceed on its own: the semaphore is shared by every caller.
	MaxConcurrent int
	JQTimeout     time.Duration
	Sessions      *ring.Sessions
	IsDevel       bool
	Logger        embedlog.Logger
}

// ToolsService implements the MCP tools.* namespace dispatched by mcpkit after
// the slash to dot rewrite (tools/list, tools/call → tools.list, tools.call).
type ToolsService struct {
	zenrpc.Service
	embedlog.Logger
	targets *target.Catalog
	// The upstream client is not held here: this layer decides who may call and
	// what the answer looks like, and nothing else.
	proxy    *proxy.Manager
	code     *code.Manager
	db       *dbq.Manager
	docs     *doc.Library
	sessions *ring.Sessions
	env      string
	isDevel  bool
	// maxBytes sizes the budget one answer gets; workers is how many calls of
	// one batch run at once. The upstream semaphore is shared by every caller,
	// so a batch that launched all twenty would hold slots others wait for.
	maxBytes int
	workers  int
	auditor  audit.Writer
	tools    *mcptool.Registry
}

// NewToolsService wires the catalogue into the tools dispatcher.
func NewToolsService(d ToolsDeps) ToolsService {
	initSeries(sortedProfileNames(d.Targets), sortedDatabaseNames(d.Targets))
	s := ToolsService{
		Logger:   d.Logger,
		targets:  d.Targets,
		proxy:    proxy.NewManager(d.Upstream, d.JQTimeout, d.MaxBytes),
		code:     code.NewManager(d.Repos, d.Graph, d.Upstream),
		db:       d.DB,
		docs:     d.Docs,
		sessions: d.Sessions,
		env:      d.Targets.Env,
		isDevel:  d.IsDevel,
		maxBytes: d.MaxBytes,
		workers:  batchWorkers(d.MaxConcurrent),
		// One writer per service: the rules and the message are the same on
		// every record, and choosing them does not belong on a hot path.
		auditor: newAuditWriter(d.Logger),
	}
	// After the fields: every tool holds the service it dispatches to.
	s.tools = newRegistry(s)
	return s
}

// batchWorkers is how many items of one batch run at once: never more than the
// instance-wide upstream semaphore can spare.
func batchWorkers(maxConcurrent int) int {
	if maxConcurrent > 0 {
		return min(defaultBatchWorkers, maxConcurrent)
	}
	return defaultBatchWorkers
}

func sortedProfileNames(c *target.Catalog) []string {
	if c == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(c.Profiles))
}

func sortedDatabaseNames(c *target.Catalog) []string {
	if c == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(c.Databases))
}

// maxTools is the widest tools/list this service builds: api_call, six code
// tools, two database tools, repo_map and help. A capacity hint, not a limit.
const maxTools = 11

// List returns the tools visible to the caller; the catalogue and annotations
// are built per role by each tool's own Describe (registry.go).
//
// The role is resolved here rather than in the registry because a caller whose
// groups match nothing must get a 403, not an empty catalogue. RPCError turns an
// invalid cursor into -32602 instead of zenrpc's blanket -32603.
//
//zenrpc:cursor nextCursor from the previous page; empty for the first
func (s ToolsService) List(ctx context.Context, cursor string) (mcp.ToolList, error) {
	if _, err := s.access(ctx); err != nil {
		return mcp.ToolList{}, err
	}
	list, err := s.tools.List(ctx, cursor)
	return list, mcpkit.RPCError("tools.list", err)
}

// Call dispatches an MCP tools/call request.
//
// MCP convention: tool errors are returned in result.IsError=true, never as
// JSON-RPC errors — that is why all branches return (result, nil). arguments is
// map[string]any (not json.RawMessage) to avoid the zenrpc-gen encoding/json
// double-import.
//
//zenrpc:name name of the tool to invoke (api_call)
//zenrpc:arguments tool-specific JSON arguments
func (s ToolsService) Call(ctx context.Context, name string, arguments map[string]any) (mcp.ToolCallResult, error) {
	return s.tools.Call(ctx, name, arguments)
}

// callRepoMap answers from the catalogue alone: no upstream, no cache, no TTL.
func (s ToolsService) callRepoMap(ctx context.Context, arguments map[string]any) mcp.ToolCallResult {
	rec, done := s.beginAudit(ctx, ToolRepoMap)
	defer done()

	acc, terr := s.grant(ctx, rec)
	if terr != nil {
		return errResult(*terr)
	}
	// Refused on the same condition that hides it from tools/list: a tool the
	// list does not offer must not answer when called anyway.
	if !acc.HasTool(ToolRepoMap) || len(acc.Repos) == 0 {
		rec.DenyReason = ErrCodeForbiddenRole
		return errResult(ToolError{
			Code:    ErrCodeForbiddenRole,
			Message: "your role grants no repositories for " + ToolRepoMap,
			Env:     s.env,
		})
	}

	var args RepoMapArgs
	if decErr := mcp.DecodeArgs(arguments, &args); decErr != nil {
		observe(ToolRepoMap, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + decErr.Error(), Env: s.env})
	}

	observeBatch(ToolRepoMap, len(args.Repos))
	if err := checkBatchMax(len(args.Repos), maxRepoMapBatch, "repos"); err != nil {
		observe(ToolRepoMap, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{
			Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(),
			Env: s.env, Repos: acc.Repos,
		})
	}
	rec.BatchSize, rec.BatchIndex = len(args.Repos), batchCallIndex
	names, detailed := acc.Repos, len(args.Repos) > 0
	if detailed {
		for _, name := range args.Repos {
			if !slices.Contains(acc.Repos, name) {
				observe(ToolRepoMap, "-", outcomeBadArgs)
				rec.DenyReason = ErrCodeTargetUnknown
				return errResult(ToolError{
					Code:    ErrCodeTargetUnknown,
					Message: fmt.Sprintf("unknown repository %q", name),
					Env:     s.env,
					Repos:   acc.Repos,
				})
			}
		}
		names = args.Repos
	}

	out := RepoMapResult{Env: s.env, Repos: make([]RepoEntry, 0, len(names))}
	for _, name := range names {
		r, ok := s.targets.Repos[name]
		if !ok {
			continue
		}
		out.Repos = append(out.Repos, newRepoEntry(name, r, detailed))
	}
	observe(ToolRepoMap, "-", outcomeOK)
	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	rec.Path = strings.Join(args.Repos, ",")
	res := okResultJSON(out, s.env)
	rec.BytesOut = res.Size()
	return res
}

// newRepoEntry maps one catalogue entry. The layer map and the exclusions come
// only with a single-repository answer: in a table they would cost more context
// than they explain.
func newRepoEntry(name string, r *target.Repo, detailed bool) RepoEntry {
	e := RepoEntry{
		Repo:          name,
		SentrySlug:    r.SentrySlug,
		NomadJob:      r.NomadJob,
		PromJob:       r.PromJob,
		DefaultBranch: r.DefaultBranch,
		GitLabProject: r.GitLabProject,
		Description:   r.Description,
	}
	if detailed {
		e.Layers, e.Exclude = r.Layers, r.Exclude
	}
	return e
}

// callAPI runs a batch of api_calls: the list is checked as a whole, then every
// call goes through role, target, write barrier, method and path checks on its
// own. One failed call does not fail the batch — IsError is for refusals where
// nothing ran at all, because an error makes the model resend the whole list.
func (s ToolsService) callAPI(ctx context.Context, arguments map[string]any) mcp.ToolCallResult {
	rec, done := s.beginAudit(ctx, ToolAPICall)
	defer done()
	// The record of the call itself, as opposed to the records of its items.
	rec.BatchIndex = batchCallIndex

	acc, terr := s.grant(ctx, rec)
	if terr != nil {
		observe(ToolAPICall, "-", outcomeForbidden)
		return errResult(*terr)
	}
	// Refused on the same condition that hides it from tools/list. The registry
	// checks it too; this one gives the refusal its audit record.
	if !acc.HasTool(ToolAPICall) {
		observe(ToolAPICall, "-", outcomeForbidden)
		rec.DenyReason = ErrCodeForbiddenRole
		return errResult(ToolError{
			Code:    ErrCodeForbiddenRole,
			Message: "your role does not grant " + ToolAPICall,
			Env:     s.env,
		})
	}

	var args APICallArgs
	if decErr := mcp.DecodeArgs(arguments, &args); decErr != nil {
		observe(ToolAPICall, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + decErr.Error(), Env: s.env})
	}
	observeBatch(ToolAPICall, len(args.Calls))
	if err := checkBatch(len(args.Calls), maxAPICallBatch, "calls"); err != nil {
		observe(ToolAPICall, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{
			Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(),
			Env: s.env, Targets: acc.Targets,
		})
	}
	rec.BatchSize = len(args.Calls)

	// Writes stay one call at a time: a list is how one injected instruction
	// spends the whole session budget in a single turn.
	if terr := s.refuseBatchedWrite(acc, args.Calls); terr != nil {
		observe(ToolAPICall, "-", outcomeForbidden)
		rec.DenyReason = terr.Code
		return errResult(*terr)
	}

	items := s.runAPIBatch(ctx, acc, args, rec)
	out := APICallBatch{Env: s.env, TraceID: rec.TraceID, Results: items, Budget: budgetOf(ctx)}
	for i := range items {
		out.Truncated = out.Truncated || items[i].Truncated
	}
	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	res := okResultJSON(out, s.env)
	rec.BytesOut = res.Size()
	return res
}

// batchCallIndex marks the audit record of a batched call itself, the one a
// human counts as "a tool call"; its items carry their own index from zero.
const batchCallIndex = -1

// refuseBatchedWrite rejects a list that mixes a write into it. The refusal is
// the whole batch rather than that one item: a partial answer where the write
// silently did not happen is the one outcome nobody would notice.
func (s ToolsService) refuseBatchedWrite(acc target.Access, calls []APICall) *ToolError {
	if len(calls) == 1 {
		return nil
	}
	for i, c := range calls {
		prof, ok := s.targets.Profile(c.Target)
		if !ok || !isWrite(prof, c.Method) {
			continue
		}
		return &ToolError{
			Code: ErrCodeWriteNotBatched,
			Message: fmt.Sprintf("calls[%d] writes to %s, and a write is sent on its own: repeat it as a batch of one, "+
				"so its intent and its cost in the session budget are visible", i, c.Target),
			Env:     s.env,
			Targets: acc.Targets,
		}
	}
	return nil
}

// runAPIBatch performs the calls and fits the answers into one budget. Audit
// records and metrics are written per item: "which path did this role ask for"
// is a question about an item, not about a list.
func (s ToolsService) runAPIBatch(ctx context.Context, acc target.Access, args APICallArgs, rec *auditRecord) []APICallItem {
	// The same call twice in one list is made once.
	dupes := dedup(args.Calls, callKeyOf)

	perItem := s.itemBytes(args)
	items := parallelMap(args.Calls, s.workers, func(i int, c APICall) APICallItem {
		if dupes[i] >= 0 {
			return APICallItem{Index: i, Target: c.Target}
		}
		return s.oneAPICall(ctx, acc, c, i, rec, perItem)
	})
	for i, from := range dupes {
		if from < 0 {
			continue
		}
		// The metadata of the answer, not the answer: the payload is already in
		// the list once.
		items[i] = APICallItem{
			Index: i, Target: items[from].Target, Status: items[from].Status,
			Truncated: items[from].Truncated, BytesTotal: items[from].BytesTotal,
			DefaultJQ: items[from].DefaultJQ, Redacted: items[from].Redacted,
			DedupOf: &from,
		}
		// A copy of the refusal, not a second pointer: the repeat loses its
		// cheat sheet below, and a shared value would strip the original too.
		if e := items[from].Error; e != nil {
			dup := *e
			items[i].Error = &dup
		}
		s.auditDedup(ctx, args.Calls[i], i, rec)
	}

	dropRepeatedSheets(items)
	s.fitAPIBatch(items, s.batchBudget(args))
	return items
}

// dropRepeatedSheets leaves the cheat sheet on the first refusal of each target
// and takes it off the rest: a bad list gets every item wrong at once, and the
// sheets are kilobytes.
func dropRepeatedSheets(items []APICallItem) {
	seen := make(map[string]bool, len(items))
	for i := range items {
		e := items[i].Error
		if e == nil || e.Help == "" {
			continue
		}
		if seen[items[i].Target] {
			e.Help = ""
			continue
		}
		seen[items[i].Target] = true
	}
}

// itemBytes is the cap one call of a batch is shaped to. Without it every item
// is filtered, redacted and encoded at the whole batch's budget, and most of
// that work is thrown away. The instance default is the floor.
func (s ToolsService) itemBytes(args APICallArgs) int {
	if args.MaxBytes <= 0 {
		return 0 // the profile and the instance default decide, as before
	}
	return max(args.MaxBytes/len(args.Calls), s.maxBytes)
}

// apiCallKey is what makes two calls in one batch the same call: everything that
// shapes the answer, headers included, since one can be the credential.
//
// A comparable struct rather than a joined string, so the fields are compared
// without copying them. Hashing the body would allocate less and answer wrongly
// on a collision, which a deduplicator does not get to trade away.
type apiCallKey struct {
	target, method, path, body, jq, headers string
}

func callKeyOf(c APICall) apiCallKey {
	return apiCallKey{
		target: c.Target, method: strings.ToUpper(c.Method), path: c.Path,
		body: c.Body, jq: c.JQ, headers: headersKey(c.Headers),
	}
}

// headersKey flattens the headers a call brought, in name order: a map is not
// comparable, and two spellings of the same set must produce one key.
func headersKey(h map[string]string) string {
	if len(h) == 0 {
		return ""
	}
	names := slices.Sorted(maps.Keys(h))
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(h[n])
		b.WriteByte(1)
	}
	return b.String()
}

// batchBudget is the size of the whole answer: a multiple of the single-answer
// default, because a fan-out over twenty projects is a legitimate answer.
func (s ToolsService) batchBudget(args APICallArgs) int {
	return ring.PickLimit(args.MaxBytes, 0, s.maxBytes*batchBudgetFactor)
}

// batchBudgetFactor turns the single-answer default into a batch budget.
const batchBudgetFactor = 4

// fitAPIBatch spends the budget over the answers, smallest first, and drops the
// ones that do not fit. Dropped whole rather than cut: an api_call answer is a
// JSON document, and half of one is a parse error rather than a smaller answer.
func (s ToolsService) fitAPIBatch(items []APICallItem, budget int) {
	sizes := make([]int, len(items))
	for i := range items {
		sizes[i] = itemSize(items[i])
	}
	fits := shareBudget(sizes, budget)
	for i := range items {
		if sizes[i] < 0 || fits[i] {
			continue
		}
		size := sizes[i]
		items[i].Data, items[i].Truncated = nil, true
		items[i].Error = &ToolError{
			Code: ErrCodeTruncated,
			Message: fmt.Sprintf("dropped: %d bytes would not fit the batch budget of %d left for the other answers; "+
				"narrow this call's jq, raise max_bytes or ask for it on its own", size, budget),
		}
	}
}

// itemSize is what one answer will cost the budget, -1 for an item with no
// payload to weigh. Read off the item rather than measured: ShapeJSON already
// sized this body (BytesTotal).
func itemSize(item APICallItem) int {
	switch {
	case item.Error != nil:
		return -1 // an error text is part of the envelope, not of the budget
	case item.DedupOf != nil:
		return -1 // the payload is counted once, on the item it was copied from
	case item.Data == nil:
		return -1
	}
	size := item.BytesTotal
	if text, ok := item.Data.(string); ok {
		// A cut answer travels as a marked string, and that is what is paid for.
		size = len(text)
	}
	return size + itemEnvelopeBytes
}

// itemEnvelopeBytes is what an item costs beside its payload: the index, the
// target, the status, the duration and the JSON around them.
const itemEnvelopeBytes = 96

// oneAPICall is a single call inside the batch: the checks, the upstream and its
// own audit record.
func (s ToolsService) oneAPICall(ctx context.Context, acc target.Access, call APICall, index int, parent *auditRecord, maxBytes int) APICallItem {
	rec, _, done := s.beginItemAudit(ctx, parent, index)
	defer done()
	rec.Target, rec.Method, rec.Path, rec.Intent = call.Target, call.Method, call.Path, call.Intent
	rec.JQ = call.JQ

	item := APICallItem{Index: index, Target: call.Target}
	prof, terr := s.prepareCall(ctx, acc, &call, rec)
	if terr != nil {
		item.Error = terr
		return item
	}

	start := time.Now()
	answer, callErr := s.proxy.Do(ctx, prof, proxy.Request{
		Method: call.Method, Path: call.Path, Body: call.Body, Headers: call.Headers,
		JQ: call.JQ, MaxBytes: maxBytes,
	})
	elapsed := time.Since(start)
	// Bill each call separately: the batch ran them at once, so the wall clock
	// alone would charge for the longest one instead of the work done. Labelled
	// by the target, which prepareCall has found in the catalogue: a name the
	// caller made up never gets this far, so it cannot become a series.
	ratelimit.ChargeFor(ctx, call.Target, elapsed)
	item.DurationMS = elapsed.Milliseconds()

	if callErr != nil {
		item.Error = s.upstreamError(ctx, callErr, call, rec)
		item.Status = callErr.Status
		return item
	}

	observe(ToolAPICall, call.Target, outcomeOK)
	observeRedactions(call.Target, answer.Redacted)
	// Which rules fired, not what they fired on.
	rec.Masked = answer.Redacted.Names()
	rec.UpstreamStatus = answer.Status
	rec.Decision, rec.DenyReason, rec.BytesOut = audit.DecisionAllow, "", answer.BytesTotal
	rec.Truncated = answer.Truncated

	item.Status, item.Truncated, item.BytesTotal = answer.Status, answer.Truncated, answer.BytesTotal
	item.DefaultJQ, item.Data, item.Redacted = answer.DefaultJQ, answer.Data, answer.Redacted.Names()
	if answer.AsText {
		item.Data = answer.Text
	}
	return item
}

// prepareCall runs everything that has to be true before a call may be made: the
// arguments, the target, the role, the allowlist, the headers and the write
// barrier. It rewrites the call's headers into their canonical form, which is
// why it takes a pointer.
func (s ToolsService) prepareCall(ctx context.Context, acc target.Access, call *APICall, rec *auditRecord) (*target.Profile, *ToolError) {
	if call.Target == "" || call.Method == "" || call.Path == "" {
		observe(ToolAPICall, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return nil, &ToolError{Code: ErrCodeBadArgs, Message: "target, method and path are required", Targets: acc.Targets}
	}
	prof, ok := s.targets.Profile(call.Target)
	if !ok {
		observe(ToolAPICall, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeTargetUnknown
		return nil, &ToolError{
			Code:    ErrCodeTargetUnknown,
			Message: fmt.Sprintf("unknown target %q", call.Target),
			Targets: acc.Targets,
		}
	}
	// The profile's redaction rules reach the record before the role check: a
	// refused call is logged too, and its path is no less the model's.
	rec.Redact, rec.RedactRules = prof.Redact, prof.RedactRules
	if !acc.HasTarget(call.Target) {
		observe(ToolAPICall, call.Target, outcomeForbidden)
		rec.DenyReason = ErrCodeForbiddenRole
		return nil, &ToolError{
			Code:    ErrCodeForbiddenRole,
			Message: fmt.Sprintf("target %q exists but your role does not grant it", call.Target),
			Targets: acc.Targets,
		}
	}
	if terr := s.checkRoute(prof, *call, rec); terr != nil {
		return nil, terr
	}
	if terr := s.checkHeaders(prof, call, rec); terr != nil {
		return nil, terr
	}

	// The barrier stands after the allowlist on purpose: a call refused by path
	// never happened, and charging the session for it spends the on-call budget
	// on typos.
	rec.Write = isWrite(prof, call.Method)
	if rec.Write {
		return prof, s.checkWriteBarrier(ctx, acc, *call, rec)
	}
	return prof, nil
}

// isWrite decides whether the write barrier applies: a non-GET call on a write
// profile. A function because the barrier and the batch refusal both ask, and
// they must never disagree about what a write is.
func isWrite(prof *target.Profile, method string) bool {
	return prof.Write && !strings.EqualFold(method, http.MethodGet)
}

// auditDedup records a call the batch answered from an identical earlier one: it
// is in the log, marked so it does not read as a second round trip.
func (s ToolsService) auditDedup(ctx context.Context, call APICall, index int, parent *auditRecord) {
	rec, _, done := s.beginItemAudit(ctx, parent, index)
	defer done()
	rec.Target, rec.Method, rec.Path, rec.Intent = call.Target, call.Method, call.Path, call.Intent
	rec.JQ = call.JQ
	if prof, ok := s.targets.Profile(call.Target); ok {
		rec.Redact, rec.RedactRules = prof.Redact, prof.RedactRules
	}
	rec.Decision, rec.DenyReason, rec.Cache = audit.DecisionAllow, "", cacheDedup
	observe(ToolAPICall, call.Target, outcomeCached)
}

// checkRoute runs the allowlist in the order the catalogue describes it: the
// method, the path, the parameters on that path, the method inside a JSON-RPC
// body. Every refusal carries what is allowed and the target's cheat sheet.
func (s ToolsService) checkRoute(prof *target.Profile, call APICall, rec *auditRecord) *ToolError {
	deny := func(e ToolError) *ToolError {
		observe(ToolAPICall, call.Target, outcomeOf(e.Code))
		rec.DenyReason = e.Code
		e.Help = s.sheetText(call.Target)
		return &e
	}
	if !prof.AllowsMethod(call.Method) {
		return deny(ToolError{
			Code:    ErrCodeMethodNotAllowed,
			Message: fmt.Sprintf("method %q is not allowed for target %q", call.Method, call.Target),
			Methods: prof.AllowMethods,
		})
	}
	// The method takes part in the path decision: a pattern may be bound to one.
	if !prof.Allows(call.Method, call.Path) {
		return deny(ToolError{
			Code:    ErrCodePathNotAllowed,
			Message: fmt.Sprintf("%s %q is not allowed for target %q", call.Method, call.Path, call.Target),
			Paths:   prof.AllowPaths,
		})
	}
	if err := prof.CheckQuery(call.Path); err != nil {
		return deny(ToolError{
			Code:        ErrCodeQueryParamNotAllowed,
			Message:     fmt.Sprintf("target %q: %s", call.Target, err),
			QueryParams: prof.AllowQueryParams,
		})
	}
	// A framework that reads the body into the same params as the query string
	// reads a credential out of it just the same.
	if err := target.CheckBody(call.Body); err != nil {
		return deny(ToolError{
			Code:    ErrCodeQueryParamNotAllowed,
			Message: fmt.Sprintf("target %q: %s", call.Target, err),
		})
	}
	// On an API that lives behind one path the allowlist is the method in the body.
	if err := prof.CheckRPC(call.Body); err != nil {
		return deny(ToolError{
			Code:       ErrCodeRPCMethodNotAllowed,
			Message:    fmt.Sprintf("target %q: %s", call.Target, err),
			RPCMethods: prof.AllowRPCMethods,
		})
	}
	return nil
}

// MaxCallHeaders caps how many headers one call may set: a credential and a
// marker or two, not a channel the catalogue should be describing instead.
const MaxCallHeaders = 8

// checkHeaders decides the headers a call brought; a name the profile did not
// list is refused with the names it did. Only the names reach the audit, never
// the values: a header set from a call usually carries a credential.
func (s ToolsService) checkHeaders(prof *target.Profile, call *APICall, rec *auditRecord) *ToolError {
	if len(call.Headers) == 0 {
		return nil
	}
	deny := func(code, msg string) *ToolError {
		observe(ToolAPICall, call.Target, outcomeOf(code))
		rec.DenyReason = code
		return &ToolError{Code: code, Message: msg, Headers: prof.AllowHeaders}
	}
	if len(call.Headers) > MaxCallHeaders {
		return deny(ErrCodeBadArgs, fmt.Sprintf("a call may set at most %d headers, this one sets %d", MaxCallHeaders, len(call.Headers)))
	}

	// Names are canonicalised first: the allowlist compares case-insensitively,
	// so "platform" and "Platform" are one header, but a map has room for both
	// and iteration order would decide which one travelled.
	out := make(map[string]string, len(call.Headers))
	names := make([]string, 0, len(call.Headers))
	for raw, value := range call.Headers {
		name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(raw))
		switch {
		case !target.ValidHeaderName(name):
			return deny(ErrCodeBadArgs, fmt.Sprintf("header name %q is not a valid header name", raw))
		case !prof.HeaderAllowed(name):
			return deny(ErrCodeHeaderNotAllowed, fmt.Sprintf("target %q does not allow the header %q", call.Target, name))
		case !target.ValidHeaderValue(value):
			return deny(ErrCodeBadArgs, fmt.Sprintf("header %q has an empty, over-long or non-printable value", name))
		case out[name] != "":
			return deny(ErrCodeBadArgs, fmt.Sprintf("header %q is set twice under different spellings", name))
		}
		out[name] = value
		names = append(names, name)
	}
	// Sorted so two identical calls produce one log line: map iteration is random.
	slices.Sort(names)
	rec.Headers, call.Headers = names, out
	return nil
}

// checkWriteBarrier guards the two things a write needs beyond a role that
// allows it: an intent and a session budget. Both are checked after the
// allowlist and before the request is built, so a refused write neither reaches
// the upstream nor costs the budget.
func (s ToolsService) checkWriteBarrier(ctx context.Context, acc target.Access, call APICall, rec *auditRecord) *ToolError {
	// An intent is a comment, not a proof — but a write nobody can explain is
	// worse than one nobody reads.
	if strings.TrimSpace(call.Intent) == "" {
		observe(ToolAPICall, call.Target, outcomeForbidden)
		rec.DenyReason = ErrCodeIntentRequired
		return &ToolError{
			Code:    ErrCodeIntentRequired,
			Message: "writing to " + call.Target + " requires intent: say what this call changes and why",
		}
	}

	if acc.MaxWrites <= 0 || s.sessions == nil {
		return nil // no budget configured, nothing to spend
	}
	spent := s.sessions.AddWrite(sessionKey(ctx))
	if spent > acc.MaxWrites {
		observe(ToolAPICall, call.Target, outcomeForbidden)
		rec.DenyReason = ErrCodeWriteBudget
		return &ToolError{
			Code:    ErrCodeWriteBudget,
			Message: fmt.Sprintf("write budget for this session is spent: %d of %d used", spent-1, acc.MaxWrites),
		}
	}
	return nil
}

// traceID returns the trace of the caller's session, creating one on first call.
func (s ToolsService) traceID(ctx context.Context) string {
	if s.sessions == nil {
		return ""
	}
	return s.sessions.Get(sessionKey(ctx)).TraceID
}

// sessionKey identifies the caller. Anonymous dev callers share one key on
// purpose: on an instance without auth there is nobody to tell apart.
func sessionKey(ctx context.Context) string {
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.UserID != "" {
		return p.UserID
	}
	return "anonymous"
}

// upstreamError maps a failed call. The outcome label matters as much as the
// code: the ratio of denials to upstream errors tells a broken skill from a
// broken backend.
func (s ToolsService) upstreamError(ctx context.Context, e *proxy.Error, call APICall, rec *auditRecord) *ToolError {
	rec.UpstreamStatus = e.Status

	out := ToolError{Message: e.Error(), Status: e.Status}
	switch e.Kind {
	case proxy.FailBadArgs:
		out.Code, rec.DenyReason = ErrCodeBadArgs, ErrCodeBadArgs
		out.Status = 0
		observe(ToolAPICall, call.Target, outcomeBadArgs)
	case proxy.FailTimeout:
		out.Code, rec.DenyReason = ErrCodeTimeout, ErrCodeTimeout
		out.Status = 0
		observe(ToolAPICall, call.Target, outcomeTimeout)
		s.Error(ctx, "upstream timeout", "target", call.Target, "path", rec.LogText(call.Path), "err", rec.LogText(e.Error()))
	case proxy.FailJQ:
		out.Code, rec.DenyReason = ErrCodeJQFailed, ErrCodeJQFailed
		out.Keys = e.Keys
		observe(ToolAPICall, call.Target, outcomeBadArgs)
	default:
		out.Code, rec.DenyReason = ErrCodeUpstream, ErrCodeUpstream
		observe(ToolAPICall, call.Target, outcomeError)
		// A backend that answered at all — 500 included — is not our failure;
		// one that could not be reached is.
		if e.Status == 0 {
			s.Error(ctx, "upstream failed", "target", call.Target, "path", rec.LogText(call.Path), "err", rec.LogText(e.Error()))
		}
	}
	return &out
}

// access resolves the caller's groups into what they may do. A dev instance
// without auth has no principal: it gets read access to everything so a laptop
// run is usable, and never the write bit.
func (s ToolsService) access(ctx context.Context) (target.Access, error) {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		acc := s.targets.Resolve(p.Groups)
		if acc.Empty() {
			return acc, ErrNoRole
		}
		return acc, nil
	}
	if !s.isDevel {
		return target.Access{}, ErrNoRole
	}
	return s.develAccess(), nil
}

func (s ToolsService) develAccess() target.Access {
	acc := target.Access{Roles: []string{"devel"}, Tools: target.KnownTools}
	for name, p := range s.targets.Profiles {
		if p.Write {
			continue
		}
		acc.Targets = append(acc.Targets, name)
	}
	for name := range s.targets.Repos {
		acc.Repos = append(acc.Repos, name)
	}
	// Every database too: they are read-only by construction.
	for name := range s.targets.Databases {
		acc.Databases = append(acc.Databases, name)
	}
	sort.Strings(acc.Targets)
	sort.Strings(acc.Repos)
	sort.Strings(acc.Databases)
	return acc
}

func (s ToolsService) targetTraits(acc target.Access) (writable, headed bool) {
	for _, name := range acc.Targets {
		p, ok := s.targets.Profile(name)
		if !ok {
			continue
		}
		writable = writable || p.Write
		headed = headed || p.AllowsAnyHeader()
	}
	return writable, headed
}

// apiCallDescription renders the catalogue the model reads on every request:
// names and one-liners here, the full AllowPaths and examples behind
// ringsrv://targets/<name>.md, pulled on demand.
func (s ToolsService) apiCallDescription(acc target.Access) string {
	var b strings.Builder
	// The batch form comes first and the single call as its special case: asking
	// for parallel calls in prose produced 4 batched pairs out of 341 in a day.
	fmt.Fprintf(&b, "%s · HTTP-запросы к таргетам из списка, пути — по allowlist. calls — до %d запросов разом; ответы в results[] по порядку, у каждого data или error. Один запрос — список из одного.\n\nТаргеты:\n",
		strings.ToUpper(s.env), maxAPICallBatch)
	for _, name := range acc.Targets {
		p, ok := s.targets.Profile(name)
		if !ok {
			continue
		}
		// Allowed header names are catalogue facts, so the static cheat sheets
		// cannot carry them; without them a header is discovered by being
		// refused. Only targets that take one pay for the line.
		fmt.Fprintf(&b, "  • %s — %s", name, s.shortDescription(p.Description))
		if len(p.AllowHeaders) > 0 {
			fmt.Fprintf(&b, " [headers: %s]", strings.Join(p.AllowHeaders, ", "))
		}
		b.WriteString("\n")
	}
	// Two habits the log showed missing: a fan-out over ids went one call per
	// turn (211 calls for three questions), and large answers went unfiltered.
	b.WriteString("\nВеер по id (проекты, MR, задачи) — одним вызовом. Ответы сужай jq; без jq — фильтр таргета (default_jq), \".\" — целиком; max_bytes — на весь вызов.\n")
	b.WriteString("\nПути, примеры, грабли: help(<name>), несколько имён сразу.\n")
	if acc.AllowWrite {
		b.WriteString("Write: intent обязателен, лимит на сессию, только отдельным вызовом.\n")
	}
	return b.String()
}

// shortDescription drops the contour from a catalogue description: the tool
// description's header already says it once.
func (s ToolsService) shortDescription(desc string) string {
	return strings.TrimPrefix(desc, s.env+" · ")
}

// repoMapDescription lists what the caller can ask about: without the names the
// model guesses a repository and gets a TargetUnknown for it.
func (s ToolsService) repoMapDescription(acc target.Access) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · как сервис называется в остальных таргетах: sentry_slug, nomad_job, prom_job, ветка по умолчанию.\n\n", strings.ToUpper(s.env))
	b.WriteString("Репозитории: " + strings.Join(acc.Repos, ", ") + ".\n")
	b.WriteString("С аргументом repo — плюс слои (db/domain/rpc), исключения и рукописное описание архитектуры.\n")
	return b.String()
}

// okResultJSON wraps any answer; env travels into the error so an encode failure
// still says which contour it happened on.
//
// Not mcptool.OKResult: that one repeats the answer in structuredContent, and an
// api_call answer runs to max_bytes — a quarter of a megabyte, paid for twice.
func okResultJSON(v any, env string) mcp.ToolCallResult {
	body, err := json.Marshal(v)
	if err != nil {
		return errResult(ToolError{Code: ErrCodeUpstream, Message: "encode result: " + err.Error(), Env: env})
	}
	return mcp.TextResult(string(body))
}

// errResult renders an error envelope as an MCP tool error. The payload stays
// JSON so the model reads the code and the hints without parsing prose.
func errResult(e ToolError) mcp.ToolCallResult {
	body, err := json.Marshal(e)
	if err != nil {
		return mcp.ErrorResult(e.Message)
	}
	return mcp.ErrorResult(string(body))
}
