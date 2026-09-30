package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
)

// dbCatalogTOML is a catalogue with two databases and three roles: one with
// both, one with one, one with the tools and no database at all.
const dbCatalogTOML = `
Env = "dev"

[Profiles.prom]
Description  = "dev · Prometheus"
BaseURL      = "https://prom.example.com"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Databases.pg]
Description = "dev · PostgreSQL: orders and users; enums live in pkg/db of apisrv"
Driver      = "postgres"
Addr        = "db.example.com:5432"
Database    = "app"
User        = "ringsrv_ro"
Password    = "s3cret-pw"
Redact      = "warn"
RedactRules = ["email"]
Schemas     = ["public", "billing", "audit"]
Repo        = "apisrv"

# public carries the models of two services, billing only of one, and audit
# falls back to Repo.
[Databases.pg.SchemaRepos]
public  = ["apisrv", "billsrv"]
billing = ["billsrv"]

[Databases.ch]
Description = "dev · ClickHouse: events; FINAL on ReplacingMergeTree"
Driver      = "clickhouse"
Addr        = "ch.example.com:9000"
Database    = "events"
User        = "ringsrv"
Password    = "ch-pw"
MaxRows     = 3

[Roles.analyst]
Description = "both"
Groups      = ["analysts"]
Tools       = ["db_query", "db_introspect", "api_call"]
Targets     = ["prom", "pg", "ch"]

[Roles.pgonly]
Description = "one"
Groups      = ["pg-readers"]
Tools       = ["db_query", "db_introspect"]
Targets     = ["pg"]

[Roles.nodata]
Description = "tools, no database"
Groups      = ["nodata"]
Tools       = ["db_query", "db_introspect"]
Targets     = ["prom"]

[Repos.apisrv]
CloneURL      = "https://git.example.com/backend/apisrv.git"
DefaultBranch = "master"

[Repos.apisrv.Layers]
db = "pkg/db"

[Repos.billsrv]
CloneURL      = "https://git.example.com/backend/billsrv.git"
DefaultBranch = "master"

[Repos.billsrv.Layers]
db = "internal/store"
`

// dbFixture is a tools service over dbCatalogTOML with scripted clients.
type dbFixture struct {
	ToolsService
	pg, ch *dbqtest.Client
	m      *dbq.Manager
}

func newDBFixture(t *testing.T, l embedlog.Logger) *dbFixture {
	t.Helper()
	cat, err := target.Parse([]byte(dbCatalogTOML))
	require.NoError(t, err)

	f := &dbFixture{pg: dbqtest.New(dbqtest.Rows(10)), ch: dbqtest.New(dbqtest.Rows(10))}
	f.pg.TableList = []dbq.Table{{Schema: "public", Name: "users", Kind: "table", RowsEstimate: 2, Comment: "Registered users"}}
	f.pg.Schemas = map[string]*dbq.Schema{
		"users": {
			Table: "users", Schema: "public", Comment: "Registered users", RowsEstimate: 2,
			Columns:    []dbq.ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "status", Type: "order_status", Enum: []string{"new", "paid"}}},
			PrimaryKey: []string{"id"},
		},
		"billing.invoices": {
			Table: "billing.invoices", Schema: "billing",
			Columns: []dbq.ColumnInfo{{Name: "id", Type: "bigint"}},
		},
		"audit.log": {
			Table: "audit.log", Schema: "audit",
			Columns: []dbq.ColumnInfo{{Name: "id", Type: "bigint"}},
		},
	}
	f.ch.TableList = []dbq.Table{{Name: "events", Kind: "ReplacingMergeTree", RowsEstimate: 5}}
	f.ch.Schemas = map[string]*dbq.Schema{"events": {Table: "events", Engine: "ReplacingMergeTree", SortingKey: "id", Columns: []dbq.ColumnInfo{{Name: "id", Type: "UInt64"}}}}
	f.m = dbq.NewManager(dbq.Options{JQTimeout: time.Second, MaxBytes: 1 << 20, RetryAfter: time.Millisecond})
	for name, d := range cat.Databases {
		c := f.pg
		if d.Driver == target.DriverClickHouse {
			c = f.ch
		}
		maxRows := d.MaxRows
		if maxRows == 0 {
			maxRows = 200
		}
		require.NoError(t, f.m.Add(dbq.Target{
			Name: name, Driver: d.Driver, Client: c, MaxRows: maxRows, Timeout: time.Second,
			Redact: d.Redact, RedactRules: d.RedactRules, Password: d.Password,
			// The declared scope, as pkg/app wires it: without it every
			// qualified name is out of scope.
			Database: d.Database, Schemas: d.Schemas,
		}))
	}
	require.NoError(t, f.m.Prove(t.Context()))

	f.ToolsService = NewToolsService(ToolsDeps{
		Targets: cat, DB: f.m, JQTimeout: time.Second, MaxBytes: 1 << 20,
		Docs: testDocs(t), Sessions: ring.NewSessions(time.Hour, nil), Logger: l,
	})
	return f
}

func decodeText[T any](t *testing.T, res mcp.ToolCallResult) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out), res.Content[0].Text)
	return out
}

// dbArgs turns one statement into the list db_query takes, lifting what belongs
// to the call — the database, the intent, the budget — out of it.
func dbArgs(call map[string]any) map[string]any {
	args, q := map[string]any{}, map[string]any{}
	for k, v := range call {
		switch k {
		case "target", "intent", "max_bytes":
			args[k] = v
		default:
			q[k] = v
		}
	}
	args["queries"] = []any{q}
	return args
}

// introArgs is the same for db_introspect, where one table became a list.
func introArgs(call map[string]any) map[string]any {
	args := map[string]any{}
	for k, v := range call {
		if k == "table" {
			args["tables"] = []any{v}
			continue
		}
		args[k] = v
	}
	return args
}

// dbItem reads the single answer of a one-statement batch, with the batch it
// came in: the driver and the trace live there now.
func dbItem(t *testing.T, res mcp.ToolCallResult) (DBQueryBatch, DBQueryItem) {
	t.Helper()
	require.False(t, res.IsError, res.Content[0].Text)
	out := decodeText[DBQueryBatch](t, res)
	require.Len(t, out.Results, 1)
	require.Nil(t, out.Results[0].Error, "expected an answer, got a refusal")
	return out, out.Results[0]
}

// dbSchema reads the single schema of a one-table introspection.
func dbSchema(t *testing.T, res mcp.ToolCallResult) DBSchemaResult {
	t.Helper()
	require.False(t, res.IsError, res.Content[0].Text)
	out := decodeText[DBSchemaBatch](t, res)
	require.Len(t, out.Results, 1)
	require.NotNil(t, out.Results[0].Schema, "expected a schema, got: "+res.Content[0].Text)
	return *out.Results[0].Schema
}

// The tools appear when the role names a database and refuse when it does
// not — the list and the call say the same thing.
func TestList_DBToolsFollowDatabases(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})

	names := func(ctx context.Context) []string {
		list, err := f.List(ctx, "")
		require.NoError(t, err)
		out := make([]string, len(list.Tools))
		for i, tool := range list.Tools {
			out[i] = tool.Name
		}
		return out
	}
	assert.Equal(t, []string{ToolAPICall, ToolDBQuery, ToolDBIntrospect, ToolHelp}, names(ctxWithGroups("analysts")))
	assert.Equal(t, []string{ToolDBQuery, ToolDBIntrospect, ToolHelp}, names(ctxWithGroups("pg-readers")))
	assert.Empty(t, names(ctxWithGroups("nodata")), "the tools without a database are not tools")

	list, err := f.List(ctxWithGroups("pg-readers"), "")
	require.NoError(t, err)
	query := list.Tools[0]
	assert.Contains(t, query.Description, "DEV ·")
	assert.Contains(t, query.Description, "pg — PostgreSQL: orders and users")
	assert.NotContains(t, query.Description, "ClickHouse", "a caller never reads about a database they cannot query")
	assert.Contains(t, query.Description, "help(db)")
	assert.True(t, *query.Annotations.ReadOnlyHint)
	assert.False(t, *query.Annotations.DestructiveHint)
	assert.True(t, *query.Annotations.OpenWorldHint)
	assert.Contains(t, string(query.InputSchema), `"additionalProperties":false`)

	res, err := f.Call(ctxWithGroups("nodata"), ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT 1"}))
	require.NoError(t, err)
	assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)

	// Without the manager — an instance whose catalogue names no database —
	// the tools are not offered even to a role that names them.
	bare := NewToolsService(ToolsDeps{Targets: f.targets, Logger: embedlog.Logger{}})
	list, err = bare.List(ctxWithGroups("analysts"), "")
	require.NoError(t, err)
	assert.Len(t, list.Tools, 2, "api_call and help")
}

// Every row of the error table, on a scripted client.
func TestDBQuery_Errors(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})
	ctx := ctxWithGroups("pg-readers")

	call := func(ctx context.Context, args map[string]any) ToolError {
		res, err := f.Call(ctx, ToolDBQuery, dbArgs(args))
		require.NoError(t, err)
		return decodeErr(t, res)
	}

	t.Run("bad args", func(t *testing.T) {
		t.Parallel()
		e := call(ctx, map[string]any{"sql": "SELECT 1"})
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		assert.Equal(t, []string{"pg"}, e.Targets, "the databases the role may query")
		e = call(ctx, map[string]any{"target": "pg"})
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		e = call(ctx, map[string]any{"target": "pg", "sql": "DELETE FROM users"})
		assert.Equal(t, ErrCodeBadArgs, e.Code)
		assert.Contains(t, e.Message, "SELECT, WITH, EXPLAIN")
		e = call(ctx, map[string]any{"target": "pg", "sql": "SELECT 1", "max_rows": "many"})
		assert.Equal(t, ErrCodeBadArgs, e.Code)
	})

	t.Run("unknown and forbidden targets", func(t *testing.T) {
		t.Parallel()
		e := call(ctx, map[string]any{"target": "nope", "sql": "SELECT 1"})
		assert.Equal(t, ErrCodeTargetUnknown, e.Code)
		assert.Equal(t, []string{"pg"}, e.Targets)
		e = call(ctx, map[string]any{"target": "ch", "sql": "SELECT 1"})
		assert.Equal(t, ErrCodeForbiddenRole, e.Code)
		assert.Equal(t, []string{"pg"}, e.Targets, "the list never names the refused one")
		e = call(ctx, map[string]any{"target": "prom", "sql": "SELECT 1"})
		assert.Equal(t, ErrCodeTargetUnknown, e.Code, "a profile is not a database")
	})

	t.Run("timeout, upstream, jq", func(t *testing.T) {
		t.Parallel()
		f := newDBFixture(t, embedlog.Logger{})
		f.pg.SetQuery(func(dbq.Statement) (*dbq.Result, error) { return nil, fmt.Errorf("cancelled: %w", dbq.ErrTimeout) })
		res, err := f.Call(ctx, ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT pg_sleep(60)"}))
		require.NoError(t, err)
		assert.Equal(t, ErrCodeTimeout, decodeErr(t, res).Code)

		f.pg.SetQuery(func(dbq.Statement) (*dbq.Result, error) {
			return nil, errors.New(`ERROR #42703 column "nmae" does not exist`)
		})
		res, _ = f.Call(ctx, ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT nmae FROM users"}))
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeUpstream, e.Code)
		assert.Contains(t, e.Message, `column "nmae" does not exist`, "the server's text is the documentation")

		f.pg.SetQuery(dbqtest.Rows(2))
		res, _ = f.Call(ctx, ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT 1", "jq": ".rows.nope"}))
		e = decodeErr(t, res)
		assert.Equal(t, ErrCodeJQFailed, e.Code)
		assert.Contains(t, e.Keys, "rows")
	})

	t.Run("unproven target", func(t *testing.T) {
		t.Parallel()
		f := newDBFixture(t, embedlog.Logger{})
		f.pg.SetProof(dbq.Proof{}, errors.New("dial tcp: connection refused"))
		f.m.Reprove(t.Context())
		res, err := f.Call(ctx, ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT 1"}))
		require.NoError(t, err)
		e := decodeErr(t, res)
		assert.Equal(t, ErrCodeUpstream, e.Code)
		assert.Contains(t, e.Message, "not proven read-only")
	})

	t.Run("max_rows above the target is cut quietly", func(t *testing.T) {
		t.Parallel()
		f := newDBFixture(t, embedlog.Logger{})
		res, err := f.Call(ctxWithGroups("analysts"), ToolDBQuery, dbArgs(map[string]any{"target": "ch", "sql": "SELECT 1", "max_rows": 1000}))
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		batch, out := dbItem(t, res)
		assert.Equal(t, 3, out.RowsReturned, "the catalogue's MaxRows")
		assert.True(t, out.Truncated)
		assert.Equal(t, "clickhouse", batch.Driver)
	})
}

// The answer's shape: env and driver, the counts outside the
// data, the rows inside it, the caller's name on its way to the server.
func TestDBQuery_Answer(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})
	var got dbq.Statement
	f.pg.SetQuery(func(req dbq.Statement) (*dbq.Result, error) { got = req; return dbqtest.Rows(2)(req) })

	res, err := f.Call(ctxWithUser("alice", "pg-readers"), ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT id, name FROM users", "intent": "who is there"}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	batch, out := dbItem(t, res)
	assert.Equal(t, "dev", batch.Env)
	assert.Equal(t, "postgres", batch.Driver)
	assert.NotEmpty(t, batch.TraceID)
	assert.Equal(t, 2, out.RowsReturned)
	assert.False(t, out.Truncated)
	assert.Equal(t, []string{"email"}, out.Redacted, "warn counts and shows")
	data, ok := out.Data.(map[string]any)
	require.True(t, ok)
	assert.Len(t, data["rows"], 2)
	assert.Equal(t, "ringsrv:alice", got.Caller)
	assert.Contains(t, got.SQL, "LIMIT 201")

	// Past max_bytes the data travels as a marked string: the model sees what
	// it got and that there was more.
	res, err = f.Call(ctxWithUser("alice", "pg-readers"), ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT id, name FROM users", "max_bytes": 100}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	_, out = dbItem(t, res)
	assert.True(t, out.Truncated)
	text, ok := out.Data.(string)
	require.True(t, ok, "cut JSON is not JSON, so it goes as text")
	assert.True(t, strings.HasSuffix(text, mcp.TruncateMarker))

	// An id past 2^53 survives the pipeline exactly: a bigint decoded into
	// a float64 would come back with its last digits changed.
	f.pg.SetQuery(func(req dbq.Statement) (*dbq.Result, error) {
		return &dbq.Result{Columns: []dbq.Column{{Name: "id", Type: "bigint"}}, Rows: [][]any{{int64(9007199254740993)}, {uint64(18446744073709551615)}}}, nil
	})
	res, err = f.Call(ctxWithUser("alice", "pg-readers"), ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT id FROM users", "jq": ".rows | map(.[0])"}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	assert.Contains(t, res.Content[0].Text, "9007199254740993")
	assert.Contains(t, res.Content[0].Text, "18446744073709551615")
}

// The proof gauge reads the manager's state, whatever order the service and
// the manager were built in: a zero written at construction would stand
// until the next hourly probe.
func TestDBProvenGauge(t *testing.T) {
	t.Parallel()
	cat, err := target.Parse([]byte(dbCatalogTOML))
	require.NoError(t, err)
	m := dbq.NewManager(dbq.Options{OnState: RecordDBState})
	pg := dbqtest.New(nil)
	pg.TableList = []dbq.Table{{Schema: "public", Name: "users"}, {Schema: "public", Name: "orders"}}
	require.NoError(t, m.Add(dbq.Target{Name: "pg", Driver: target.DriverPostgres, Client: pg, MaxRows: 1, Timeout: time.Second}))
	ch := dbqtest.New(nil)
	ch.SetProof(dbq.Proof{}, errors.New("connection refused"))
	require.NoError(t, m.Add(dbq.Target{Name: "ch", Driver: target.DriverClickHouse, Client: ch, MaxRows: 1, Timeout: time.Second}))
	require.NoError(t, m.Prove(t.Context()))

	NewToolsService(ToolsDeps{Targets: cat, DB: m, Logger: embedlog.Logger{}})
	assert.InDelta(t, 1, DBProven("pg"), 0, "proven before the service was built, still proven after")
	assert.InDelta(t, 0, DBProven("ch"), 0)
	// The table gauge rides the same probe, and survives the same
	// construction order: one OnState writes both.
	assert.InDelta(t, 2, DBTables("pg"), 0)
	assert.InDelta(t, -1, DBTables("ch"), 0, "a probe that could not ask reports unknown, not an empty base")
}

// Both levels of db_introspect, and the pointer to the code.
func TestDBIntrospect(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})
	ctx := ctxWithGroups("pg-readers")

	res, err := f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg"}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	tables := decodeText[DBTablesResult](t, res)
	assert.Equal(t, []string{"public", "billing", "audit"}, tables.Schemas)
	assert.Equal(t, "app", tables.Database, "the answer names the base it looked in, empty or not")
	require.Len(t, tables.Tables, 1)
	assert.Equal(t, "Registered users", tables.Tables[0].Comment)
	assert.Empty(t, tables.Note, "a list that found something explains itself")
	assert.Empty(t, tables.Visible, "and costs no second question to the server")

	res, err = f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "users"}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	schema := dbSchema(t, res)
	assert.Equal(t, []string{"id"}, schema.PrimaryKey)
	assert.Equal(t, []string{"new", "paid"}, schema.Columns[1].Enum)
	require.NotNil(t, schema.Code)
	assert.Equal(t, []DBCodeRepo{{Repo: "apisrv", Layer: "pkg/db"}, {Repo: "billsrv", Layer: "internal/store"}}, schema.Code.Repos,
		"public belongs to two services — both are named, in catalogue order")
	assert.Contains(t, schema.Code.Hint, `code_search("users", repos: ["apisrv"], path_glob: "pkg/db/**")`)
	assert.Contains(t, schema.Code.Hint, `code_search("users", repos: ["billsrv"], path_glob: "internal/store/**")`,
		"the layers differ, so one path_glob cannot cover both")

	// A schema of its own service, and a schema nobody claimed: the second
	// reads the base's Repo.
	res, err = f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "billing.invoices"}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	invoices := dbSchema(t, res)
	require.NotNil(t, invoices.Code)
	assert.Equal(t, []DBCodeRepo{{Repo: "billsrv", Layer: "internal/store"}}, invoices.Code.Repos)
	assert.Contains(t, invoices.Code.Hint, `code_search("invoices"`, "the hint searches the bare table name")

	res, err = f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "audit.log"}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	audit := dbSchema(t, res)
	require.NotNil(t, audit.Code)
	assert.Equal(t, []DBCodeRepo{{Repo: "apisrv", Layer: "pkg/db"}}, audit.Code.Repos, "a schema SchemaRepos does not name falls back to Repo")

	res, err = f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "userz"}))
	require.NoError(t, err)
	e := decodeErr(t, res)
	assert.Equal(t, ErrCodeBadArgs, e.Code)
	assert.Equal(t, []string{"public.users"}, e.Keys, "the tables that are there, as a call may name them")

	res, err = f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "users; drop table users"}))
	require.NoError(t, err)
	assert.Equal(t, ErrCodeBadArgs, decodeErr(t, res).Code)

	res, err = f.Call(ctxWithGroups("analysts"), ToolDBIntrospect, introArgs(map[string]any{"target": "ch", "table": "events"}))
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	ch := dbSchema(t, res)
	assert.Nil(t, ch.Code, "no Repo, no pointer")
	assert.Equal(t, "ReplacingMergeTree", ch.Engine)
	assert.Equal(t, "id", ch.SortingKey)
}

// An empty table list is what three different faults give: a base that holds
// nothing, a role granted nothing in it, and a catalogue naming a base this role
// cannot reach. Telling them apart used to take a dozen calls, so the empty
// answer carries the diagnosis.
func TestDBIntrospectEmpty(t *testing.T) {
	t.Parallel()

	t.Run("clickhouse: the base is visible and holds nothing", func(t *testing.T) {
		t.Parallel()
		f := newDBFixture(t, embedlog.Logger{})
		f.ch.TableList = nil
		f.ch.VisibleScope = dbq.Scope{Database: "events", Visible: []string{"default", "events", "system"}}

		res, err := f.Call(ctxWithGroups("analysts"), ToolDBIntrospect, introArgs(map[string]any{"target": "ch"}))
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content[0].Text)
		out := decodeText[DBTablesResult](t, res)
		assert.Equal(t, "events", out.Database)
		assert.Empty(t, out.Tables)
		assert.Equal(t, []string{"default", "events", "system"}, out.Visible)
		assert.Contains(t, out.Note, `database "events" is visible to this role and holds no tables`)
	})

	t.Run("clickhouse: the base is not visible at all", func(t *testing.T) {
		t.Parallel()
		f := newDBFixture(t, embedlog.Logger{})
		f.ch.TableList = nil
		f.ch.VisibleScope = dbq.Scope{Database: "events", Visible: []string{"default", "system"}}

		res, err := f.Call(ctxWithGroups("analysts"), ToolDBIntrospect, introArgs(map[string]any{"target": "ch"}))
		require.NoError(t, err)
		out := decodeText[DBTablesResult](t, res)
		assert.Contains(t, out.Note, "not visible to this role")
		assert.Contains(t, out.Note, `answers "unknown table"`,
			"which is what every query against it will say, and why it says nothing about the query")
	})

	t.Run("postgres: a declared schema without USAGE is named", func(t *testing.T) {
		t.Parallel()
		f := newDBFixture(t, embedlog.Logger{})
		f.pg.TableList = nil
		f.pg.VisibleScope = dbq.Scope{Database: "app", Visible: []string{"public"}}

		res, err := f.Call(ctxWithGroups("pg-readers"), ToolDBIntrospect, introArgs(map[string]any{"target": "pg"}))
		require.NoError(t, err)
		out := decodeText[DBTablesResult](t, res)
		assert.Contains(t, out.Note, "no USAGE on schemas billing, audit")
		assert.Equal(t, []string{"public"}, out.Visible)
	})

	t.Run("a server that cannot be asked says so", func(t *testing.T) {
		t.Parallel()
		f := newDBFixture(t, embedlog.Logger{})
		f.pg.TableList = nil
		f.pg.ScopeErr = errors.New("connection reset by peer")

		res, err := f.Call(ctxWithGroups("pg-readers"), ToolDBIntrospect, introArgs(map[string]any{"target": "pg"}))
		require.NoError(t, err)
		require.False(t, res.IsError, "an empty list is still an answer")
		out := decodeText[DBTablesResult](t, res)
		assert.Contains(t, out.Note, "could not be asked")
		assert.Equal(t, "app", out.Database, "the base is known from the catalogue, not from the server")
	})
}

// The sheet is one per driver and ships inside the binary, so its examples are
// written against a schema nobody has, and a model that took them for the
// catalogue built hypotheses on tables that do not exist. The facts of this
// instance are generated per call and appended.
func TestHelpDBCarriesInstanceFacts(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})
	f.ch.TableList = nil
	require.NoError(t, f.m.Reprove(t.Context()))

	res, err := f.Call(ctxWithGroups("analysts"), ToolHelp, map[string]any{"names": []any{sheetDB}})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content[0].Text)
	out := decodeText[HelpResult](t, res)
	require.Len(t, out.Results, 1)
	text := out.Results[0].Text

	assert.Contains(t, text, "FINAL", "the compiled sheet is still there")
	assert.Contains(t, text, "вымышленные", "and says its own examples are illustrations")
	assert.Contains(t, text, "## Базы этого инстанса")
	assert.Contains(t, text, "| `pg` | postgres | `app` | public, billing, audit | 1 |")
	assert.Contains(t, text, "| `ch` | clickhouse | `events` | — | 0 |",
		"a base with nothing in it, named on the one surface read before the first query")
}

// A base whose role sees no tables is marked where the model picks a target;
// in the ordinary case the mark costs nothing, and tools/list is paid for on
// every request.
func TestDBQueryDescriptionMarksEmptyBases(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})
	list, err := f.List(ctxWithGroups("analysts"), "")
	require.NoError(t, err)
	for _, tool := range list.Tools {
		if tool.Name == ToolDBQuery {
			assert.NotContains(t, tool.Description, "0 таблиц", "both bases have tables")
		}
	}

	empty := newDBFixture(t, embedlog.Logger{})
	empty.ch.TableList = nil
	require.NoError(t, empty.m.Reprove(t.Context()))
	list, err = empty.List(ctxWithGroups("analysts"), "")
	require.NoError(t, err)
	for _, tool := range list.Tools {
		if tool.Name == ToolDBQuery {
			assert.Contains(t, tool.Description, "ch — ClickHouse: events; FINAL on ReplacingMergeTree [сейчас 0 таблиц]")
			assert.NotContains(t, tool.Description, "of apisrv [сейчас", "the base with tables is unmarked")
		}
	}
}

// Repositories that keep their models in the same directory are one search;
// the hint splits only when a single path_glob would have to mean two.
func TestCodeHint(t *testing.T) {
	t.Parallel()
	same := codeHint("users", []target.CodeRef{{Repo: "apisrv", Layer: "pkg/db"}, {Repo: "paysrv", Layer: "pkg/db"}})
	assert.Equal(t, `enum'ы и модели — code_search("users", repos: ["apisrv", "paysrv"], path_glob: "pkg/db/**")`, same)

	one := codeHint("users", []target.CodeRef{{Repo: "apisrv", Layer: "pkg/db"}})
	assert.Equal(t, `enum'ы и модели — code_search("users", repos: ["apisrv"], path_glob: "pkg/db/**")`, one)
}

// A password inside a driver error must not reach the model, through
// a query or through the proof's reason.
func TestDBQuery_ErrorHasNoSecret(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})
	f.pg.SetQuery(func(dbq.Statement) (*dbq.Result, error) {
		return nil, errors.New("pg: FATAL password authentication failed (tried s3cret-pw)")
	})
	res, err := f.Call(ctxWithGroups("pg-readers"), ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT 1"}))
	require.NoError(t, err)
	assert.NotContains(t, res.Content[0].Text, "s3cret-pw")
	assert.Contains(t, res.Content[0].Text, "[masked:password]")

	f.pg.SetProof(dbq.Proof{}, errors.New("dial s3cret-pw@db.example.com"))
	f.m.Reprove(t.Context())
	res, err = f.Call(ctxWithGroups("pg-readers"), ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": "SELECT 1"}))
	require.NoError(t, err)
	assert.NotContains(t, res.Content[0].Text, "s3cret-pw")
}

// One record per call, denials included; the SQL whole, sanitised and
// redacted, with the hash of the full text; never the rows.
func TestAudit_DBQuery(t *testing.T) {
	const sql = "SELECT id FROM users\nWHERE email = 'ivan@example.com'"
	recs := captureLog(t, func(l embedlog.Logger) {
		f := newDBFixture(t, l)
		_, err := f.Call(ctxWithUser("alice", "pg-readers"), ToolDBQuery, dbArgs(map[string]any{"target": "pg", "sql": sql, "intent": "find ivan"}))
		require.NoError(t, err)
		_, err = f.Call(ctxWithUser("alice", "pg-readers"), ToolDBQuery, dbArgs(map[string]any{"target": "ch", "sql": sql}))
		require.NoError(t, err)
		_, err = f.Call(ctxWithUser("alice", "pg-readers"), ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "table": "users"}))
		require.NoError(t, err)
	})
	// One record per statement — the refused one included, because the SQL of
	// a refusal is only written here — plus one per tools/call.
	records := itemRecords(recs)
	require.Len(t, records, 3, "one record per statement, the refusal included")
	require.Len(t, callRecords(recs), 3, "and one per tools/call")

	ok := records[0]
	assert.Equal(t, ToolDBQuery, ok["tool"])
	assert.Equal(t, audit.DecisionAllow, ok["decision"])
	assert.Equal(t, "pg", ok["source"])
	assert.Equal(t, "alice", ok["sub"])
	assert.Equal(t, "find ivan", ok["intent"])
	assert.Equal(t, "SELECT id FROM users WHERE email = 'i***@example.com'", ok["query"], "one line, the literal masked even in warn")
	assert.Equal(t, audit.Hash(sql), ok["query_sha256"])
	assert.EqualValues(t, 10, ok["rows"])
	assert.Positive(t, ok["bytes_out"])
	line, err := json.Marshal(ok)
	require.NoError(t, err)
	assert.NotContains(t, string(line), "user1@example.com", "rows never reach the log")

	denied := records[1]
	assert.Equal(t, audit.DecisionDeny, denied["decision"])
	assert.Equal(t, ErrCodeForbiddenRole, denied["deny_reason"])
	assert.Equal(t, audit.Hash(sql), denied["query_sha256"], "a refused query is still on record")
	assert.Equal(t, "SELECT id FROM users WHERE email = 'i***@example.com'", denied["query"],
		"a target without redaction of its own still gets the log's base rules")

	introspect := records[2]
	assert.Equal(t, ToolDBIntrospect, introspect["tool"])
	assert.Equal(t, "users", introspect["query"], "the table travels as the path")
	assert.Equal(t, audit.DecisionAllow, introspect["decision"])
}

// A role that does not grant db_query is stopped by the registry, before the
// tool runs — and the statements the call carried still reach the log. It is the
// only place the SQL of a refusal is written: a refusal rendered without the
// arguments said deny and nothing about what was asked.
func TestAudit_RefusedToolStillRecordsItsStatements(t *testing.T) {
	const sql = "SELECT id FROM users WHERE email = 'ivan@example.com'"
	recs := captureLog(t, func(l embedlog.Logger) {
		f := newDBFixture(t, l)
		res, err := f.Call(ctxWithGroups("nodata"), ToolDBQuery, map[string]any{
			"target":  "pg",
			"queries": []any{map[string]any{"sql": sql}},
		})
		require.NoError(t, err)
		assert.Equal(t, ErrCodeForbiddenRole, decodeErr(t, res).Code)
	})

	items := itemRecords(recs)
	require.Len(t, items, 1, "one statement, one item record")
	assert.Equal(t, "SELECT id FROM users WHERE email = 'i***@example.com'", items[0]["query"])
	assert.Equal(t, audit.Hash(sql), items[0]["query_sha256"], "hashed before masking, so a repeat still matches")
	assert.EqualValues(t, 1, items[0]["batch_size"], "batch_size = 0 would read as a tool that takes no list")
	assert.Equal(t, audit.DecisionDeny, items[0]["decision"])
	assert.Equal(t, ErrCodeForbiddenRole, items[0]["deny_reason"])
}

// The registry hands a refused call back to its tool so the record is written
// where the arguments are known — and the tool still refuses on the same
// condition that hid it from tools/list.
func TestCall_ToolTheRoleDoesNotGrantIsRefused(t *testing.T) {
	t.Parallel()
	f := newDBFixture(t, embedlog.Logger{})

	// pg-readers gets the database tools and help, never api_call.
	res, err := f.Call(ctxWithGroups("pg-readers"), ToolAPICall, apiArgs(map[string]any{
		"target": "pg", "method": "GET", "path": "/api/x",
	}))
	require.NoError(t, err)
	e := decodeErr(t, res)
	assert.Equal(t, ErrCodeForbiddenRole, e.Code)
	assert.Contains(t, e.Message, ToolAPICall)
}

// The instructions promise the tools by name and the cheat sheet by URI.
func TestInstructions_NameDBTools(t *testing.T) {
	t.Parallel()
	text := instructions("dev", false)
	for _, want := range []string{ToolDBQuery, ToolDBIntrospect, "ringsrv://tools/db.md"} {
		assert.Contains(t, text, want)
	}
	assert.Contains(t, text, "вызови db_introspect", "the one habit that saves the first query: introspect first")
}
