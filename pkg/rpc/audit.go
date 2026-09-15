package rpc

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/auth"
	"github.com/vmkteam/mcpkit/redact"
)

// auditMessage is what a Loki query matches on before it parses a field, so it
// keeps the string this service wrote before the library existed.
const auditMessage = "api call"

// logKeyHeaders names the header-names field. The keys of Extra are as fixed as
// the core ones — saved queries are written against them — so the ones used in
// more than one place are named rather than spelled out at each.
const logKeyHeaders = "headers"

// cacheDedup marks a call answered from an identical one in the same batch.
const cacheDedup = "dedup"

// newAuditWriter builds the record writer. The mode is not a setting: warn on a
// target means "show the model, count the hits", and the log is read by everyone
// with Loki, so there it always masks.
func newAuditWriter(l embedlog.Logger) audit.Writer {
	return audit.NewWriter(l, audit.Options{
		Message: auditMessage,
		Redact:  redact.Options{Mode: redact.ModeOn, Rules: logRules},
	})
}

// logRules are masked out of every record whatever the target says: a token the
// model wrote into a query is personal data in Loki even when the target's own
// answers carry none. The target's rules add to these; card and ip stay the
// target's choice, because on a path they eat order numbers and node addresses.
var logRules = []string{redact.RuleEmail, redact.RulePhone, redact.RuleToken, redact.RuleJWT, redact.RulePEM}

// auditRecord is one call as this service sees it: audit.Record plus the fields
// only a catalogue proxy has. The domain half stays a struct rather than going
// straight into Extra because it is filled in pieces — the route check knows the
// target, the proxy knows the upstream status. core() turns it into Extra once.
type auditRecord struct {
	audit.Record

	Target string
	Method string
	// Path is the model's path for api_call, the file for the code tools and the
	// table for db_introspect. It travels as the record's Query.
	Path string
	// Headers are the names of the headers the call set, sorted. Values are
	// never written: they are credentials as often as not.
	Headers []string
	// JQ is the filter the call brought; empty means the profile's default ran.
	// The ratio says whether the model narrows answers or pays for them in full.
	JQ string
	// SQL is the query as the model sent it, travelling as Query like Path. The
	// hash is taken from the full text, so a cut query still matches a repeat.
	SQL string
	// RowsReturned is what db_query handed back, after the cap.
	RowsReturned int
	// Redact and RedactRules are the target's settings, selecting the extra
	// rules core() runs over Path, SQL and JQ. The mode is ignored: the log
	// always masks.
	Redact      string
	RedactRules []string
	Write       bool
	// Cache says the answer was served from an identical call in the same batch.
	Cache          string
	UpstreamStatus int
}

// core is the record as the library writes it: model-authored text in Query, the
// target in Source, the catalogue-specific fields in Extra. Query takes SQL when
// there is one and Path otherwise — a call has one or the other, and two fields
// for "what the model asked for" would be two fields every query has to check.
func (r auditRecord) core() audit.Record {
	out := r.Record
	out.Source = r.Target
	out.Rows = r.RowsReturned

	text := r.SQL
	if text == "" {
		text = r.Path
	}
	// Hashed before masking and before the cut, so two records of the same query
	// agree even when neither holds all of it.
	out.QueryHash = audit.Hash(text)
	out.Query = r.maskForLog(text)
	out.Extra = []any{
		logKeyMethod, r.Method,
		"jq", r.maskForLog(r.JQ),
		logKeyHeaders, strings.Join(r.Headers, ","),
		"write", r.Write,
		"cache", r.Cache,
		"upstream_status", r.UpstreamStatus,
	}
	return out
}

// maskForLog is the one rule for a model-authored string on its way into the
// log: logRules always, plus the target's own when it masks at all. It happens
// here rather than in the writer because only the record knows which target it
// is about; the writer's own pass then finds nothing left to mask.
func (r auditRecord) maskForLog(s string) string {
	if s == "" {
		return ""
	}
	rules := logRules
	switch {
	case r.Redact == "" || r.Redact == redact.ModeOff:
	case len(r.RedactRules) == 0:
		rules = redact.RuleNames()
	default:
		rules = append(slices.Clone(logRules), r.RedactRules...)
	}
	out, _ := redact.Text(s, redact.Options{Mode: redact.ModeOn, Rules: rules})
	return out
}

// LogText is maskForLog with the cap, for the error lines a tool writes beside
// its record. Masking goes first, so the cut cannot split a match and leak half.
func (r auditRecord) LogText(s string) string {
	if s == "" {
		return ""
	}
	return audit.SanitizeText(r.maskForLog(s), audit.MaxTextLen)
}

// beginAudit opens the record every tool call writes and returns it with the
// function that writes it. Who asked goes in before anything is decided, so a
// refused call names its caller the way an answered one does. Every path out of
// a dispatcher writes exactly one record, denials included: without them the
// ratio of refusals to real calls cannot be recovered.
func (s ToolsService) beginAudit(ctx context.Context, tool string) (*auditRecord, func()) {
	started := time.Now()
	rec := &auditRecord{}
	rec.Tool, rec.Env, rec.Decision = tool, s.env, audit.DecisionDeny
	p, _ := auth.PrincipalFromContext(ctx)
	rec.Subject, rec.Email, rec.Groups = p.UserID, p.Email, p.Groups
	rec.TraceID = s.traceID(ctx)
	return rec, func() {
		rec.Duration = time.Since(started)
		s.auditor.Write(ctx, rec.core())
	}
}

// beginItemAudit opens the record of one item of a batched call. What the item
// inherits — who asked, which roles answered, the trace, the list size, the
// target's redaction rules — is copied from the parent rather than resolved
// again: re-deriving the trace takes the session lock once per item, and the
// roles used to go missing from most item records.
func (s ToolsService) beginItemAudit(ctx context.Context, parent *auditRecord, index int) (*auditRecord, time.Time, func()) {
	started := time.Now()
	rec := &auditRecord{
		Target:      parent.Target,
		Redact:      parent.Redact,
		RedactRules: parent.RedactRules,
	}
	rec.Tool, rec.Env, rec.Decision = parent.Tool, s.env, audit.DecisionDeny
	rec.Subject, rec.Email, rec.Groups, rec.Roles = parent.Subject, parent.Email, parent.Groups, parent.Roles
	rec.TraceID, rec.Intent = parent.TraceID, parent.Intent
	rec.BatchSize, rec.BatchIndex = parent.BatchSize, index
	return rec, started, func() {
		rec.Duration = time.Since(started)
		s.auditor.Write(ctx, rec.core())
	}
}

// grant resolves the caller's roles into the record and refuses the call when
// there are none. Roles are written even when the answer is a refusal: "which
// role did this" is the question the audit exists for.
func (s ToolsService) grant(ctx context.Context, rec *auditRecord) (target.Access, *ToolError) {
	acc, err := s.access(ctx)
	rec.Roles = acc.Roles
	if err != nil {
		rec.DenyReason = ErrCodeForbiddenRole
		return acc, &ToolError{Code: ErrCodeForbiddenRole, Message: err.Error(), Env: s.env}
	}
	return acc, nil
}
