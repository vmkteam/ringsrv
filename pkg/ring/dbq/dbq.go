// Package dbq is the domain of the database targets: what is the same for every
// caller and every DBMS. It checks the text of a query, puts it under a LIMIT,
// keeps the read-only proof of each target, runs the query through a client and
// shapes the answer — jq, redaction, size — the way proxy shapes an HTTP one.
//
// Who may ask is decided above, in pkg/rpc; how to ask a server is decided
// below, in pkg/db and pkg/chdb, which satisfy Client. The types the interface
// names live here too (client.go).
package dbq

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/redact"
	"golang.org/x/sync/singleflight"
)

// systemScopes are the databases and schemas a server keeps for itself: present
// in every installation, readable by everyone, and no part of what a catalogue
// entry declares. They are neither drift to report nor a boundary a caller
// crossed on purpose.
var systemScopes = map[string]map[string]bool{
	target.DriverClickHouse: {"system": true, "information_schema": true, "INFORMATION_SCHEMA": true},
	target.DriverPostgres:   {"pg_catalog": true, "information_schema": true},
}

// scopeWord is what a prefix before the dot means to a driver: another database
// in ClickHouse, another schema in PostgreSQL.
func scopeWord(driver string) string {
	if driver == target.DriverClickHouse {
		return "database"
	}
	return "schema"
}

// Client is what a driver offers the domain. A timeout is reported by wrapping
// ErrTimeout or context.DeadlineExceeded, a table that is not there by wrapping
// ErrUnknownTable, a result past the byte budget by ErrResultTooLarge;
// everything else is the server's own text, which travels to the model as
// documentation after the password is scrubbed from it.
type Client interface {
	Query(ctx context.Context, req Statement) (*Result, error)
	Tables(ctx context.Context) ([]Table, error)
	Introspect(ctx context.Context, table string) (*Schema, error)
	// Scope asks what this connection can see. It is asked when an answer was
	// empty or a table was not found, because those two are the answers a grant
	// and an empty base give alike.
	Scope(ctx context.Context) (Scope, error)
	// ProveReadOnly asks the server whether this user can write. An error means
	// the server could not be asked.
	ProveReadOnly(ctx context.Context) (Proof, error)
	Close() error
}

// Target is one database the manager serves: the catalogue entry with its client
// attached. The manager does not build clients — which driver answers is
// pkg/app's decision.
type Target struct {
	Name        string
	Driver      string
	Client      Client
	MaxRows     int
	Timeout     time.Duration
	MaxBytes    int
	Redact      string
	RedactRules []string
	// Password is kept for one purpose: to be scrubbed from every error text
	// before it leaves.
	Password string
	// Database and Schemas are the scope the catalogue declared. They are kept
	// here so a refusal can name where it looked without asking the server: an
	// "unknown table" that does not say which base it looked in makes a missing
	// grant and a missing table the same sentence.
	Database string
	Schemas  []string
}

// State is where a target stands with its proof.
type State struct {
	Proven bool
	// Reason is the last failure: why the probe could not run, or what it found.
	// Empty when proven.
	Reason string
	At     time.Time
	// Tables is how many tables the last probe saw, or -1 when it never got that
	// far. A target that proves read-only and sees none is a failure this server
	// used to report as silence.
	Tables int
	// Undeclared are the databases or schemas the role can read that the
	// catalogue never named — drift between the grants and the entry, logged
	// once per probe rather than discovered by hand.
	Undeclared []string
}

// Kinds of failure, so the layer above maps one value to its error code and
// metric label instead of re-deriving the reason from a string.
const (
	FailBadArgs  = "bad_args"
	FailTimeout  = "timeout"
	FailUpstream = "upstream"
	FailUnproven = "unproven"
	FailJQ       = "jq"
)

// Error is a call that produced no answer.
type Error struct {
	Kind string
	Err  error
	// Keys are the top-level keys of the answer when a jq expression found
	// nothing, or the table names when a table was not found: the next attempt
	// needs to know what was actually there.
	Keys []string
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Request is one query as the tool received it.
type Request struct {
	SQL string
	// MaxRows is the call's cap; zero means the target's. It never rises above
	// the target's.
	MaxRows int
	JQ      string
	// MaxBytes overrides the target's cap and the instance default.
	MaxBytes int
	// Caller is who asks, for the server's own records.
	Caller string
}

// Answer is a successful query. Either Data or Text carries the body: Text is
// what an oversized answer travels as, because cutting JSON stops it from being
// JSON. RowsReturned and Truncated are kept apart from the shaped body because
// jq may have removed them from it and the audit still needs them.
type Answer struct {
	Data         any
	Text         string
	AsText       bool
	Truncated    bool
	BytesTotal   int
	Redacted     redact.Result
	RowsReturned int
	Elapsed      time.Duration
}

// Options configure the manager.
type Options struct {
	JQTimeout time.Duration
	// MaxBytes is the instance default for the answer size.
	MaxBytes int
	// RetryAfter is how long an unproven target waits before the next probe;
	// zero means a minute.
	RetryAfter time.Duration
	Logger     embedlog.Logger
	// OnState is told after every probe what it found; the metrics gauges hang
	// on it. The whole state travels, not the verdict alone: "proven, and it
	// sees no tables" is a different alert from "unproven".
	OnState func(name string, st State)
	// Now is the clock, replaceable in tests.
	Now func() time.Time
}

// Manager keeps the targets and their proofs and runs the queries.
type Manager struct {
	embedlog.Logger
	opts    Options
	mu      sync.Mutex
	targets map[string]*entry
	// probes folds concurrent probes of one target into one: a cron re-proof and
	// two calls retrying an unproven target must not dial three times.
	probes singleflight.Group
}

type entry struct {
	Target
	state State
}

// declared is the scope the catalogue gave this target, as one list: the schemas
// for postgres, the single database for ClickHouse. Every question about the
// boundary is answered from here, so they cannot drift apart.
func (e *entry) declared() []string {
	if e.Driver == target.DriverClickHouse {
		return []string{e.Database}
	}
	return e.Schemas
}

// scopeText is where this target's queries land, in words: the base, and for
// postgres the search path, because a bare name is looked up along it.
func (e *entry) scopeText() string {
	if e.Driver == target.DriverClickHouse {
		return fmt.Sprintf("this target queries database %q", e.Database)
	}
	return fmt.Sprintf("this target queries database %q with search_path %s", e.Database, strings.Join(e.Schemas, ", "))
}

// emptyNote turns an empty table list into the sentence it is missing: whether
// the scope is unreadable by this role, or readable and empty. The wording is
// per driver because the grant is: a ClickHouse database is granted whole, a
// PostgreSQL schema needs USAGE.
func (e *entry) emptyNote(scope Scope) string {
	declared := e.declared()
	missing := ring.Missing(declared, scope.Visible)
	if e.Driver == target.DriverClickHouse {
		if len(missing) > 0 {
			return fmt.Sprintf("database %q is not visible to this role: no grant on it, or it is not on this server. "+
				"Every query against its tables answers \"unknown table\"; visible lists what the role can read", e.Database)
		}
		return fmt.Sprintf("database %q is visible to this role and holds no tables; visible lists the databases "+
			"the role can read, and another database is another catalogue target", e.Database)
	}
	if len(missing) > 0 {
		return fmt.Sprintf("this role holds no USAGE on schemas %s of database %q: their tables show up neither here "+
			"nor in a query; visible lists the schemas the role can read", strings.Join(missing, ", "), e.Database)
	}
	return fmt.Sprintf("schemas %s of database %q are visible to this role and hold no tables, or hold no table "+
		"this role has SELECT on", strings.Join(declared, ", "), e.Database)
}

// DefaultRetryAfter is how long an unproven target waits between probes.
const DefaultRetryAfter = time.Minute

// resultBudget is the most a query may pull off the wire before it is cancelled.
// The answer itself is cut to MaxBytes afterwards; the budget is what keeps one
// call from reading a table into the memory of the only MCP gateway.
const resultBudget = 4 * ring.MaxAnswerBytes

// NewManager makes an empty manager; targets are added one by one.
func NewManager(opts Options) *Manager {
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger.Log() == nil {
		opts.Logger = embedlog.NewLogger(false, false)
	}
	return &Manager{Logger: opts.Logger, opts: opts, targets: map[string]*entry{}}
}

// Add registers a target. It starts unproven: Prove or the first call proves it.
// The limits have to be resolved by then — a zero timeout would refuse every
// query on the spot and a zero row cap would return none.
func (m *Manager) Add(t Target) error {
	switch {
	case t.Name == "":
		return errors.New("dbq: target without a name")
	case t.Client == nil:
		return fmt.Errorf("dbq: target %q without a client", t.Name)
	case t.Timeout <= 0:
		return fmt.Errorf("dbq: target %q: Timeout %s must be positive", t.Name, t.Timeout)
	case t.MaxRows <= 0:
		return fmt.Errorf("dbq: target %q: MaxRows %d must be positive", t.Name, t.MaxRows)
	}
	// An entry that named no schema is on public. Resolving it here keeps the
	// scope one answer: an empty list would otherwise mean "no schema is in
	// scope" to the check below and "public" to the server.
	if t.Driver == target.DriverPostgres && len(t.Schemas) == 0 {
		t.Schemas = []string{target.DefaultSchema}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[t.Name] = &entry{Target: t, state: State{Reason: "not probed yet", Tables: -1}}
	return nil
}

// Names lists the targets in order.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.targets))
}

// State reports where a target stands.
func (m *Manager) State(name string) (State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.targets[name]
	if !ok {
		return State{}, false
	}
	return e.state, true
}

// Prove probes every target at start. A target that cannot be reached is logged
// and left unproven — the next call retries — because a database being down is
// not a catalogue error. A target that answers and can write is returned as an
// error: that is a wrong user or a wrong role.
func (m *Manager) Prove(ctx context.Context) error {
	var errs []error
	for name, err := range m.proveAll(ctx) {
		var w *writableError
		if errors.As(err, &w) {
			errs = append(errs, fmt.Errorf("database %q is reachable but not read-only: %s", name, w.reason))
			continue
		}
		m.Print(ctx, "database target unproven", "target", name, "err", err.Error())
	}
	return errors.Join(errs...)
}

// Reprove runs the probe on every target again — the hourly cron for ClickHouse,
// whose client has no per-connection hook. A target that turned writable is
// marked unproven: a running instance cannot refuse to start, but it can refuse
// to query.
func (m *Manager) Reprove(ctx context.Context) error {
	fails := m.proveAll(ctx)
	errs := make([]error, 0, len(fails))
	for name, err := range fails {
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}
	return errors.Join(errs...)
}

// proveAll probes the targets at once and returns the failures by name. At once,
// because the probe is a dial: two databases down would otherwise cost the boot
// two dial timeouts in a row.
func (m *Manager) proveAll(ctx context.Context) map[string]error {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		fails = map[string]error{}
	)
	for _, name := range m.Names() {
		wg.Go(func() {
			if err := m.prove(ctx, name); err != nil {
				mu.Lock()
				fails[name] = err
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return fails
}

// writableError is the probe's one finding that is not a transient state.
type writableError struct{ reason string }

func (e *writableError) Error() string { return "not read-only: " + e.reason }

// prove runs the probe once and records the outcome. Concurrent probes of one
// target share a single run, bounded by the target's timeout.
func (m *Manager) prove(ctx context.Context, name string) error {
	_, err, _ := m.probes.Do(name, func() (any, error) {
		return nil, m.proveOnce(ctx, name)
	})
	return err
}

func (m *Manager) proveOnce(ctx context.Context, name string) error {
	m.mu.Lock()
	e, ok := m.targets[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown database %q", name)
	}

	pctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	proof, err := e.Client.ProveReadOnly(pctx)
	switch {
	case err != nil:
		err = m.scrub(e, err)
	case !proof.ReadOnly:
		err = &writableError{reason: proof.Reason}
	}

	// What the role can see is asked while the connection is warm and only of a
	// target that proved: a probe that could not ask about writes has nothing to
	// say about tables either.
	next := State{Proven: err == nil, At: m.opts.Now(), Tables: -1}
	if err != nil {
		next.Reason = err.Error()
	} else {
		next.Tables, next.Undeclared = m.survey(pctx, e)
	}

	// The state and the gauge change together, under the lock: two probes
	// finishing out of order must not leave the gauge saying the opposite of the
	// state.
	m.mu.Lock()
	was, sawTables := e.state.Proven, e.state.Tables
	e.state = next
	if m.opts.OnState != nil {
		m.opts.OnState(name, next)
	}
	m.mu.Unlock()

	switch {
	case was && err != nil:
		m.Error(ctx, "database target lost its proof", "target", name, "err", err.Error())
	case !was && err == nil:
		m.Print(ctx, "database target proven", "target", name, "tables", next.Tables)
	}
	// Transitions, like the proof above: the probe is hourly, and an ERROR
	// repeated every hour about a base that is knowingly empty is how a team
	// learns to skim past errors. The steady state is the gauge's job.
	switch {
	case err == nil && next.Tables == 0 && sawTables != 0:
		m.Error(ctx, "database target has no tables this role can see", "target", name, "database", e.Database)
	case err == nil && next.Tables > 0 && sawTables == 0:
		m.Print(ctx, "database target has tables again", "target", name, "tables", next.Tables)
	}
	if len(next.Undeclared) > 0 {
		m.Print(ctx, "database target sees more than the catalogue declares", "target", name,
			scopeWord(e.Driver)+"s", strings.Join(next.Undeclared, ", "))
	}
	return err
}

// survey is what a probe learns past the proof: how many tables the role can
// list, and what it can reach beyond the entry. Neither is a reason to fail the
// probe — a base can be legitimately empty and a grant legitimately wider — so a
// failure to ask leaves the counters unknown rather than the target unproven.
func (m *Manager) survey(ctx context.Context, e *entry) (int, []string) {
	tables, err := e.Client.Tables(ctx)
	if err != nil {
		return -1, nil
	}
	scope, err := e.Client.Scope(ctx)
	if err != nil {
		return len(tables), nil
	}
	return len(tables), undeclared(e, scope)
}

// undeclared names what the role can read and the catalogue never did. The
// server's own metadata is not drift: it is in every install and granted to
// everyone.
func undeclared(e *entry, scope Scope) []string {
	var out []string
	for _, name := range ring.Missing(scope.Visible, e.declared()) {
		if systemScopes[e.Driver][name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ensure hands out the target if it is proven, probing again when the last
// attempt is old enough. No query runs on an unproven target.
func (m *Manager) ensure(ctx context.Context, name string) (*entry, *Error) {
	m.mu.Lock()
	e, ok := m.targets[name]
	if !ok {
		m.mu.Unlock()
		return nil, &Error{Kind: FailBadArgs, Err: fmt.Errorf("unknown database %q", name)}
	}
	st := e.state
	m.mu.Unlock()

	if st.Proven {
		return e, nil
	}
	reason := errors.New(st.Reason)
	if m.opts.Now().Sub(st.At) >= m.opts.RetryAfter {
		err := m.prove(ctx, name)
		if err == nil {
			return e, nil
		}
		reason = err
	}
	return nil, &Error{Kind: FailUnproven, Err: fmt.Errorf("target not proven read-only: %w", reason)}
}

// driverOf answers the driver of a target without proving it, for the checks
// that do not touch the server.
func (m *Manager) driverOf(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.targets[name]
	if !ok {
		return "", false
	}
	return e.Driver, true
}

// Query checks the text, wraps it, runs it and shapes the answer. The text is
// checked before the target is proved: a refusal that needs no server should not
// wait for one.
func (m *Manager) Query(ctx context.Context, name string, req Request) (*Answer, *Error) {
	driver, ok := m.driverOf(name)
	if !ok {
		return nil, &Error{Kind: FailBadArgs, Err: fmt.Errorf("unknown database %q", name)}
	}
	if err := CheckSQL(driver, req.SQL); err != nil {
		return nil, &Error{Kind: FailBadArgs, Err: err}
	}
	e, ferr := m.ensure(ctx, name)
	if ferr != nil {
		return nil, ferr
	}

	maxRows := e.MaxRows
	if req.MaxRows > 0 && req.MaxRows < maxRows {
		maxRows = req.MaxRows
	}
	// One row past the cap is the truncated flag; it is asked for once here and
	// never returned.
	limit := maxRows + 1
	sql := WrapLimit(e.Driver, req.SQL, limit)

	// The context gets a second over the server's own cap, so the server's
	// timeout fires first and its text — not a cancelled socket — is what the
	// model reads.
	qctx, cancel := context.WithTimeout(ctx, e.Timeout+time.Second)
	defer cancel()
	started := m.opts.Now()
	res, err := e.Client.Query(qctx, Statement{SQL: sql, MaxRows: limit, MaxBytes: resultBudget, Timeout: e.Timeout, Caller: req.Caller})
	elapsed := m.opts.Now().Sub(started)
	if err != nil {
		return nil, m.classify(e, err)
	}

	truncated := len(res.Rows) > maxRows
	if truncated {
		res.Rows = res.Rows[:maxRows]
	}
	return m.shape(ctx, e, req, res, truncated, elapsed)
}

// Tables lists what the target has.
func (m *Manager) Tables(ctx context.Context, name string) ([]Table, *Error) {
	e, ferr := m.ensure(ctx, name)
	if ferr != nil {
		return nil, ferr
	}
	qctx, cancel := context.WithTimeout(ctx, e.Timeout+time.Second)
	defer cancel()
	tables, err := e.Client.Tables(qctx)
	if err != nil {
		return nil, m.classify(e, err)
	}
	return tables, nil
}

// Diagnosis is why a table list came back empty: what the role can see, and
// which of the three causes this is. The verdict is composed here rather than by
// the caller because the scope is this layer's.
type Diagnosis struct {
	Scope Scope
	Note  string
}

// DiagnoseEmpty asks the server what this target's role can read and says what
// the empty answer means. One round trip, made only when a list came back empty.
func (m *Manager) DiagnoseEmpty(ctx context.Context, name string) (Diagnosis, *Error) {
	e, ferr := m.ensure(ctx, name)
	if ferr != nil {
		return Diagnosis{}, ferr
	}
	qctx, cancel := context.WithTimeout(ctx, e.Timeout+time.Second)
	defer cancel()
	scope, err := e.Client.Scope(qctx)
	if err != nil {
		return Diagnosis{}, m.classify(e, err)
	}
	if scope.Database == "" {
		scope.Database = e.Database
	}
	return Diagnosis{Scope: scope, Note: e.emptyNote(scope)}, nil
}

// Introspect describes one table. A table that is not there answers with the
// names that are, the way an unknown target does.
func (m *Manager) Introspect(ctx context.Context, name, table string) (*Schema, *Error) {
	if !ValidTable(table) {
		return nil, &Error{Kind: FailBadArgs, Err: fmt.Errorf("table %q must be an identifier, optionally schema-qualified", table)}
	}
	e, ferr := m.ensure(ctx, name)
	if ferr != nil {
		return nil, ferr
	}
	qctx, cancel := context.WithTimeout(ctx, e.Timeout+time.Second)
	defer cancel()
	if ferr := m.checkScope(qctx, e, table); ferr != nil {
		return nil, ferr
	}
	schema, err := e.Client.Introspect(qctx, table)
	if errors.Is(err, ErrUnknownTable) {
		out := &Error{Kind: FailBadArgs, Err: fmt.Errorf("unknown table %q", table)}
		if tables, terr := e.Client.Tables(qctx); terr == nil {
			for _, t := range tables {
				out.Keys = append(out.Keys, tableName(t))
			}
		}
		return nil, out
	}
	if err != nil {
		return nil, m.classify(e, err)
	}
	return schema, nil
}

// checkScope refuses a qualified name that points outside what the catalogue
// declared for this target. Tables reads the target's database and its schemas,
// while Introspect used to follow any prefix it was handed, so a name from
// another base was described from a place the tool never lists. Another database
// is another [Databases.*] entry; the refusal names both what is in scope and
// what the role can actually reach.
func (m *Manager) checkScope(ctx context.Context, e *entry, table string) *Error {
	prefix, _, ok := strings.Cut(table, ".")
	if !ok || systemScopes[e.Driver][prefix] {
		return nil
	}
	declared := e.declared()
	if slices.Contains(declared, prefix) {
		return nil
	}

	word := scopeWord(e.Driver)
	out := &Error{Kind: FailBadArgs, Keys: declared, Err: fmt.Errorf(
		"%s %q is outside target %q, which covers %s %s — another %s is another catalogue entry",
		word, prefix, e.Name, word+"s", strings.Join(declared, ", "), word)}
	// What the role can see is worth one query on a path that is already a
	// refusal: it separates "the catalogue does not cover it" from "it is not
	// there at all", and those need different people to fix them.
	if scope, err := e.Client.Scope(ctx); err == nil {
		out.Err = fmt.Errorf("%w; this role can read %s %s", out.Err, word+"s", strings.Join(scope.Visible, ", "))
	}
	return out
}

// tableName is a table as a call may name it: schema-qualified wherever the
// driver has schemas. Which schema is the default is the client's knowledge.
func tableName(t Table) string {
	if t.Schema == "" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// Close closes every client. The pools are closed outside the lock: a close that
// hangs must not take State and Names down with it.
func (m *Manager) Close() error {
	m.mu.Lock()
	clients := make([]Client, 0, len(m.targets))
	for _, e := range m.targets {
		clients = append(clients, e.Client)
	}
	m.mu.Unlock()

	var errs []error
	for _, c := range clients {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// classify maps a client error onto a kind. The text travels as the server wrote
// it, after the password is scrubbed and the target's redaction has run: an
// error is documentation, and a column name is no secret to the role that reads
// the column.
func (m *Manager) classify(e *entry, err error) *Error {
	err = m.scrub(e, err)
	switch {
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return &Error{Kind: FailTimeout, Err: fmt.Errorf("query exceeded the target's timeout of %s: %w", e.Timeout, err)}
	case errors.Is(err, ErrUnknownTable):
		// Where the query landed has to be in the sentence: a server answers the
		// same "unknown table" for a table that does not exist and for one whose
		// database or schema this role was never granted, and the second is not
		// the model's to fix.
		return &Error{Kind: FailBadArgs, Err: fmt.Errorf(
			"%w — %s; either the table is not there, or its %s is not granted to this role. db_introspect(%q) lists what is",
			err, e.scopeText(), scopeWord(e.Driver), e.Name)}
	case errors.Is(err, ErrResultTooLarge):
		return &Error{Kind: FailBadArgs, Err: fmt.Errorf("%w — select fewer columns, narrow the rows or lower max_rows", err)}
	default:
		return &Error{Kind: FailUpstream, Err: err}
	}
}

// scrub removes the password from an error text and runs the target's redaction
// over it. The sentinel identity is kept, so errors.Is still works.
func (m *Manager) scrub(e *entry, err error) error {
	msg := err.Error()
	if e.Password != "" {
		msg = strings.ReplaceAll(msg, e.Password, redact.Marker("password"))
	}
	msg, _ = redact.Text(msg, redact.Options{Mode: e.Redact, Rules: e.RedactRules})
	if msg == err.Error() {
		return err
	}
	return &scrubbedError{msg: msg, cause: err}
}

// scrubbedError carries the cleaned text and answers errors.Is for the original.
// It has no Unwrap on purpose: errors.As must not reach the driver's error with
// the raw text still in it.
type scrubbedError struct {
	msg   string
	cause error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Is(target error) bool {
	return errors.Is(e.cause, target)
}
