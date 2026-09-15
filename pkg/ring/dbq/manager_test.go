package dbq_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newManager(t *testing.T, c *dbqtest.Client, tgt dbq.Target, onState func(name string, proven bool)) (*dbq.Manager, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	// The callback takes the whole state; the tests that watch it care about
	// the verdict alone, so the adapter lives here rather than in each of them.
	var hook func(string, dbq.State)
	if onState != nil {
		hook = func(name string, st dbq.State) { onState(name, st.Proven) }
	}
	m := dbq.NewManager(dbq.Options{JQTimeout: time.Second, MaxBytes: 1 << 20, Now: clk.now, OnState: hook})
	tgt.Name, tgt.Client = "pg", c
	if tgt.Driver == "" {
		tgt.Driver = target.DriverPostgres
	}
	if tgt.MaxRows == 0 {
		tgt.MaxRows = 200
	}
	if tgt.Timeout == 0 {
		tgt.Timeout = time.Second
	}
	require.NoError(t, m.Add(tgt))
	return m, clk
}

// A target with an unresolved limit is refused at Add: a zero timeout
// would refuse every query on the spot, a zero row cap would return none.
func TestManager_AddValidates(t *testing.T) {
	t.Parallel()
	m := dbq.NewManager(dbq.Options{})
	c := dbqtest.New(nil)
	require.Error(t, m.Add(dbq.Target{Name: "pg", Client: c, MaxRows: 10}))
	require.Error(t, m.Add(dbq.Target{Name: "pg", Client: c, Timeout: time.Second}))
	require.Error(t, m.Add(dbq.Target{Name: "pg", Timeout: time.Second, MaxRows: 10}))
	require.Error(t, m.Add(dbq.Target{Client: c, Timeout: time.Second, MaxRows: 10}))
	require.NoError(t, m.Add(dbq.Target{Name: "pg", Client: c, Timeout: time.Second, MaxRows: 10}))
}

// The proof is a state, not a check: a database that cannot
// be reached leaves the target unproven and every call refused, until a
// probe succeeds; a database that answers and can write refuses the start.
func TestManager_State(t *testing.T) {
	t.Parallel()

	t.Run("unreachable at start, reachable later", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(dbqtest.Rows(3))
		c.SetProof(dbq.Proof{}, errors.New("dial tcp: connection refused"))
		var changes []string
		m, clk := newManager(t, c, dbq.Target{}, func(name string, proven bool) { changes = append(changes, fmt.Sprintf("%s=%v", name, proven)) })

		require.NoError(t, m.Prove(t.Context()), "a database being down is not a catalogue error")
		st, ok := m.State("pg")
		require.True(t, ok)
		assert.False(t, st.Proven)
		assert.Contains(t, st.Reason, "connection refused")

		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailUnproven, ferr.Kind)
		assert.Contains(t, ferr.Error(), "not proven read-only")
		assert.Equal(t, 1, c.Proves(), "the retry waits for its minute")

		c.SetProof(dbq.Proof{ReadOnly: true}, nil)
		clk.advance(dbq.DefaultRetryAfter)
		ans, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
		require.Nil(t, ferr)
		assert.Equal(t, 3, ans.RowsReturned)
		assert.Equal(t, 2, c.Proves())
		assert.Equal(t, []string{"pg=false", "pg=true"}, changes, "every probe reports, so the gauge heals itself")
	})

	t.Run("concurrent retries share one probe", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(dbqtest.Rows(1))
		c.SetProof(dbq.Proof{}, errors.New("dial tcp: connection refused"))
		m, clk := newManager(t, c, dbq.Target{}, nil)
		require.NoError(t, m.Prove(t.Context()))
		c.SetProof(dbq.Proof{ReadOnly: true}, nil)
		c.ProveDelay = 50 * time.Millisecond // long enough for every caller to meet the same probe
		clk.advance(dbq.DefaultRetryAfter)

		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
				assert.Nil(t, ferr)
			})
		}
		wg.Wait()
		assert.Equal(t, 2, c.Proves(), "the one at start and one shared by the twenty")
	})

	t.Run("reachable and writable refuses the start", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(nil)
		c.SetProof(dbq.Proof{ReadOnly: false, Reason: "role has INSERT on 3 tables"}, nil)
		m, _ := newManager(t, c, dbq.Target{}, nil)
		err := m.Prove(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), `database "pg" is reachable but not read-only: role has INSERT`)
		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailUnproven, ferr.Kind)
	})

	t.Run("reprove notices a user that gained writes", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(dbqtest.Rows(1))
		var states []bool
		m, _ := newManager(t, c, dbq.Target{}, func(_ string, proven bool) { states = append(states, proven) })
		require.NoError(t, m.Prove(t.Context()))
		c.SetProof(dbq.Proof{ReadOnly: false, Reason: "allow_ddl = 1"}, nil)
		require.Error(t, m.Reprove(t.Context()), "the cron job reports the loss")
		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailUnproven, ferr.Kind)
		assert.Contains(t, ferr.Error(), "allow_ddl")
		assert.Equal(t, []bool{true, false}, states, "the gauge follows the loss")
	})

	t.Run("unknown target", func(t *testing.T) {
		t.Parallel()
		m, _ := newManager(t, dbqtest.New(nil), dbq.Target{}, nil)
		_, ferr := m.Query(t.Context(), "nope", dbq.Request{SQL: "SELECT 1"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailBadArgs, ferr.Kind)
		assert.Equal(t, []string{"pg"}, m.Names())
	})
}

func proven(t *testing.T, c *dbqtest.Client, tgt dbq.Target) *dbq.Manager {
	t.Helper()
	c.SetProof(dbq.Proof{ReadOnly: true}, nil)
	m, _ := newManager(t, c, tgt, nil)
	require.NoError(t, m.Prove(t.Context()))
	return m
}

// The answer is shaped like an api_call one: rows under the cap with the
// extra row turned into a flag, jq over the object, the target's redaction,
// the size limit last.
func TestManager_Query(t *testing.T) {
	t.Parallel()

	t.Run("cap and truncated flag", func(t *testing.T) {
		t.Parallel()
		var got dbq.Statement
		c := dbqtest.New(func(req dbq.Statement) (*dbq.Result, error) { got = req; return dbqtest.Rows(1000)(req) })
		m := proven(t, c, dbq.Target{MaxRows: 5})

		ans, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT * FROM users", MaxRows: 3, Caller: "ringsrv:alice"})
		require.Nil(t, ferr)
		assert.Equal(t, 4, got.MaxRows, "one more than the answer holds")
		assert.Contains(t, got.SQL, "LIMIT 4")
		assert.Equal(t, "ringsrv:alice", got.Caller)
		assert.Equal(t, time.Second, got.Timeout)
		assert.True(t, ans.Truncated)
		assert.Equal(t, 3, ans.RowsReturned)

		data, ok := ans.Data.(map[string]any)
		require.True(t, ok)
		assert.Len(t, data["rows"], 3)
		assert.Equal(t, true, data["truncated"])
		assert.EqualValues(t, 3, data["rows_returned"])
		assert.Equal(t, []any{map[string]any{"name": "id", "type": "bigint"}, map[string]any{"name": "name", "type": "text"}}, data["columns"])

		ans, ferr = m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT * FROM users", MaxRows: 50})
		require.Nil(t, ferr)
		assert.Equal(t, 5, ans.RowsReturned, "the call never rises above the target")

		c.SetQuery(dbqtest.Rows(2))
		ans, ferr = m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT * FROM users"})
		require.Nil(t, ferr)
		assert.False(t, ans.Truncated)
		assert.Equal(t, 2, ans.RowsReturned)
	})

	t.Run("bad text never reaches the client", func(t *testing.T) {
		t.Parallel()
		called := false
		c := dbqtest.New(func(dbq.Statement) (*dbq.Result, error) { called = true; return nil, nil })
		m := proven(t, c, dbq.Target{})
		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "DELETE FROM users"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailBadArgs, ferr.Kind)
		assert.False(t, called)
	})

	t.Run("jq and redaction", func(t *testing.T) {
		t.Parallel()
		m := proven(t, dbqtest.New(dbqtest.Rows(2)), dbq.Target{Redact: redact.ModeOn, RedactRules: []string{redact.RuleEmail}})
		ans, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1", JQ: ".rows | map(.[1])"})
		require.Nil(t, ferr)
		assert.Equal(t, []any{"u***@example.com", "u***@example.com"}, ans.Data)
		assert.Equal(t, 2, ans.Redacted.Hits[redact.RuleEmail])
		assert.Equal(t, 2, ans.RowsReturned, "the audit count survives the filter")

		_, ferr = m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1", JQ: ".rows.foo"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailJQ, ferr.Kind)
		assert.ElementsMatch(t, []string{"columns", "rows", "truncated", "rows_returned", "elapsed_ms"}, ferr.Keys)
	})

	t.Run("size limit cuts the encoded text", func(t *testing.T) {
		t.Parallel()
		m := proven(t, dbqtest.New(dbqtest.Rows(100)), dbq.Target{MaxBytes: 200})
		ans, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
		require.Nil(t, ferr)
		assert.True(t, ans.AsText)
		assert.True(t, ans.Truncated)
		assert.True(t, strings.HasSuffix(ans.Text, mcp.TruncateMarker))
		assert.Greater(t, ans.BytesTotal, 200)

		ans, ferr = m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1", MaxBytes: 1 << 30})
		require.Nil(t, ferr)
		assert.False(t, ans.AsText, "the call may lift the target's cap, up to MaxAnswerBytes")
	})
}

// A server error is documentation — after the password is gone
// from it and the target's redaction has run.
func TestManager_Errors(t *testing.T) {
	t.Parallel()
	const password = "s3cret-pw"

	t.Run("password never leaves", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(func(dbq.Statement) (*dbq.Result, error) {
			return nil, errors.New(`pg: password authentication failed for "ringsrv_ro" with ` + password)
		})
		m := proven(t, c, dbq.Target{Password: password})
		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailUpstream, ferr.Kind)
		assert.NotContains(t, ferr.Error(), password)
		assert.Contains(t, ferr.Error(), "[masked:password]")

		c.SetProof(dbq.Proof{}, errors.New("dial "+password))
		_ = m.Reprove(t.Context())
		st, _ := m.State("pg")
		assert.NotContains(t, st.Reason, password, "nor through the proof's reason")
	})

	t.Run("timeout by sentinel and by context", func(t *testing.T) {
		t.Parallel()
		for _, cause := range []error{dbq.ErrTimeout, context.DeadlineExceeded} {
			c := dbqtest.New(func(dbq.Statement) (*dbq.Result, error) {
				return nil, fmt.Errorf("pg: canceling statement due to statement timeout: %w", cause)
			})
			m := proven(t, c, dbq.Target{Timeout: 3 * time.Second})
			_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT pg_sleep(60)"})
			require.NotNil(t, ferr)
			assert.Equal(t, dbq.FailTimeout, ferr.Kind)
			assert.Contains(t, ferr.Error(), "3s")
		}
	})

	t.Run("unknown table points at db_introspect", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(func(dbq.Statement) (*dbq.Result, error) {
			return nil, fmt.Errorf(`%w: relation "userz" does not exist`, dbq.ErrUnknownTable)
		})
		m := proven(t, c, dbq.Target{})
		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT * FROM userz"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailBadArgs, ferr.Kind)
		assert.Contains(t, ferr.Error(), `relation "userz" does not exist`)
		assert.Contains(t, ferr.Error(), `db_introspect("pg")`)
	})

	// A server says "unknown table" both for a table that was never created and
	// for one whose schema this role holds no grant on. The second is not the
	// model's to fix, so the answer names where it looked and offers both.
	t.Run("unknown table names the base it looked in", func(t *testing.T) {
		t.Parallel()
		fail := func(dbq.Statement) (*dbq.Result, error) {
			return nil, fmt.Errorf("%w: Unknown table expression identifier 'default.userEvents'", dbq.ErrUnknownTable)
		}

		ch := proven(t, dbqtest.New(fail), dbq.Target{Driver: target.DriverClickHouse, Database: "events"})
		_, ferr := ch.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT * FROM default.userEvents"})
		require.NotNil(t, ferr)
		assert.Contains(t, ferr.Error(), `this target queries database "events"`)
		assert.Contains(t, ferr.Error(), "its database is not granted to this role")

		pg := proven(t, dbqtest.New(fail), dbq.Target{Database: "app", Schemas: []string{"public", "billing"}})
		_, ferr = pg.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT * FROM analytics.events"})
		require.NotNil(t, ferr)
		assert.Contains(t, ferr.Error(), `database "app" with search_path public, billing`)
		assert.Contains(t, ferr.Error(), "its schema is not granted to this role")
	})

	t.Run("a result past the budget is the model's to narrow", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(func(dbq.Statement) (*dbq.Result, error) {
			return nil, fmt.Errorf("%w: more than 1048576 bytes read", dbq.ErrResultTooLarge)
		})
		m := proven(t, c, dbq.Target{})
		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT * FROM documents"})
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailBadArgs, ferr.Kind)
		assert.Contains(t, ferr.Error(), "lower max_rows")
	})

	t.Run("server error text travels redacted", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(func(dbq.Statement) (*dbq.Result, error) {
			return nil, errors.New(`invalid input syntax for type integer: "bob@example.com"`)
		})
		m := proven(t, c, dbq.Target{Redact: redact.ModeWarn, RedactRules: []string{redact.RuleEmail}})
		_, ferr := m.Query(t.Context(), "pg", dbq.Request{SQL: "SELECT 1"})
		require.NotNil(t, ferr)
		assert.Contains(t, ferr.Error(), "bob@example.com", "warn shows the model the text and counts")
	})
}

func TestManager_Introspect(t *testing.T) {
	t.Parallel()
	c := dbqtest.New(nil)
	c.TableList = []dbq.Table{{Schema: "public", Name: "users", Kind: "table"}, {Schema: "billing", Name: "invoices", Kind: "view"}}
	c.Schemas = map[string]*dbq.Schema{"users": {Table: "users", Columns: []dbq.ColumnInfo{{Name: "id", Type: "bigint"}}}}
	m := proven(t, c, dbq.Target{})

	tables, ferr := m.Tables(t.Context(), "pg")
	require.Nil(t, ferr)
	assert.Len(t, tables, 2)

	schema, ferr := m.Introspect(t.Context(), "pg", "users")
	require.Nil(t, ferr)
	assert.Equal(t, "users", schema.Table)

	_, ferr = m.Introspect(t.Context(), "pg", "userz")
	require.NotNil(t, ferr)
	assert.Equal(t, dbq.FailBadArgs, ferr.Kind)
	assert.Equal(t, []string{"public.users", "billing.invoices"}, ferr.Keys, "the names that are there, qualified as a call may name them")

	_, ferr = m.Introspect(t.Context(), "pg", "users; drop table users")
	require.NotNil(t, ferr)
	assert.Equal(t, dbq.FailBadArgs, ferr.Kind)
	assert.Contains(t, ferr.Error(), "identifier")
}

// The list and the lookup answer about the same place: Tables reads the target's
// database and its declared schemas, so a prefix pointing elsewhere described a
// table from a scope the tool never lists. The refusal names the boundary and
// what the role can actually read — two failures fixed by different people.
func TestManager_IntrospectScope(t *testing.T) {
	t.Parallel()

	t.Run("a schema outside the catalogue is refused by name", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(nil)
		c.Schemas = map[string]*dbq.Schema{"analytics.events": {Table: "analytics.events"}}
		c.VisibleScope = dbq.Scope{Database: "app", Visible: []string{"public", "billing", "analytics"}}
		m := proven(t, c, dbq.Target{Database: "app", Schemas: []string{"public", "billing"}})

		_, ferr := m.Introspect(t.Context(), "pg", "analytics.events")
		require.NotNil(t, ferr)
		assert.Equal(t, dbq.FailBadArgs, ferr.Kind)
		assert.Contains(t, ferr.Error(), `schema "analytics" is outside target "pg"`)
		assert.Contains(t, ferr.Error(), "covers schemas public, billing")
		assert.Contains(t, ferr.Error(), "this role can read schemas public, billing, analytics",
			"a schema that exists and is readable is a catalogue gap, not a typo")
		assert.Equal(t, []string{"public", "billing"}, ferr.Keys)
	})

	t.Run("another ClickHouse database is another target", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(nil)
		c.VisibleScope = dbq.Scope{Database: "events", Visible: []string{"default", "events", "system"}}
		m := proven(t, c, dbq.Target{Driver: target.DriverClickHouse, Database: "events"})

		_, ferr := m.Introspect(t.Context(), "pg", "default.userEvents")
		require.NotNil(t, ferr)
		assert.Contains(t, ferr.Error(), `database "default" is outside target "pg"`)
		assert.Contains(t, ferr.Error(), "another database is another catalogue entry")
	})

	// The server's own metadata is in every installation and readable by
	// everyone, and db_query reads it already: refusing to describe it would
	// be the very asymmetry this check exists to remove.
	t.Run("system metadata is in scope for both drivers", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			driver, table string
		}{
			{target.DriverClickHouse, "system.parts"},
			{target.DriverPostgres, "pg_catalog.pg_class"},
			{target.DriverPostgres, "information_schema.columns"},
		} {
			c := dbqtest.New(nil)
			c.Schemas = map[string]*dbq.Schema{tc.table: {Table: tc.table}}
			m := proven(t, c, dbq.Target{Driver: tc.driver, Database: "app", Schemas: []string{"public"}})

			schema, ferr := m.Introspect(t.Context(), "pg", tc.table)
			require.Nil(t, ferr, tc.table)
			assert.Equal(t, tc.table, schema.Table)
		}
	})

	// Nothing was declared, so the scope is what the driver falls back to;
	// an empty list must not mean "no schema is in scope".
	t.Run("a target without schemas is on public", func(t *testing.T) {
		t.Parallel()
		c := dbqtest.New(nil)
		c.Schemas = map[string]*dbq.Schema{"public.users": {Table: "users"}}
		m := proven(t, c, dbq.Target{Database: "app"})

		_, ferr := m.Introspect(t.Context(), "pg", "public.users")
		require.Nil(t, ferr)
	})
}

// The probe is the one thing that runs on every target every hour, so it is
// where "this base has nothing in it" is noticed — and where the grants are
// compared with what the catalogue claims.
func TestManager_ProbeSurveysScope(t *testing.T) {
	t.Parallel()

	c := dbqtest.New(nil)
	c.TableList = []dbq.Table{{Schema: "public", Name: "users"}}
	c.VisibleScope = dbq.Scope{Database: "app", Visible: []string{"public", "analytics", "pg_catalog"}}
	m := proven(t, c, dbq.Target{Database: "app", Schemas: []string{"public"}})

	st, ok := m.State("pg")
	require.True(t, ok)
	assert.Equal(t, 1, st.Tables)
	assert.Equal(t, []string{"analytics"}, st.Undeclared,
		"a schema the role can read and the catalogue never named; pg_catalog is the server's own")

	// The same scope, asked the way an empty list asks it: with the verdict
	// already composed, so the tool layer renders rather than re-derives.
	diag, ferr := m.DiagnoseEmpty(t.Context(), "pg")
	require.Nil(t, ferr)
	assert.Equal(t, "app", diag.Scope.Database)
	assert.Contains(t, diag.Note, `schemas public of database "app" are visible to this role`)

	// An empty base still proves read-only: it is a state to report, not a
	// reason to refuse every query against it.
	empty := dbqtest.New(nil)
	empty.VisibleScope = dbq.Scope{Database: "events", Visible: []string{"system"}}
	m2 := proven(t, empty, dbq.Target{Driver: target.DriverClickHouse, Database: "events"})
	st, ok = m2.State("pg")
	require.True(t, ok)
	assert.True(t, st.Proven)
	assert.Equal(t, 0, st.Tables, "proven and empty is the state that used to be invisible")

	// A server that cannot answer the survey leaves the count unknown rather
	// than reporting a confident zero.
	quiet := dbqtest.New(nil)
	quiet.ScopeErr = errors.New("connection reset")
	quiet.TableList = nil
	m3 := proven(t, quiet, dbq.Target{Database: "app"})
	st, _ = m3.State("pg")
	assert.Empty(t, st.Undeclared)
}
