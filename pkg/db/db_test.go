package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/go-pg/pg/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests run against a live PostgreSQL: RINGSRV_TEST_PG is the DSN of a user
// who may create roles and schemas on the local stand, and without it the
// package is skipped. The fixture in testdata is applied once per run and leaves
// ringsrv_test_ro and ringsrv_test_rw behind for the clients under test.
const envDSN = "RINGSRV_TEST_PG"

var super *pg.Options

func TestMain(m *testing.M) {
	if dsn := os.Getenv(envDSN); dsn != "" {
		opts, err := pg.ParseURL(dsn)
		if err != nil {
			panic(fmt.Sprintf("%s: %v", envDSN, err))
		}
		super = opts
		schema, err := os.ReadFile("testdata/schema.sql")
		if err != nil {
			panic(err)
		}
		admin := pg.Connect(opts)
		if _, err := admin.Exec(string(schema)); err != nil {
			panic(fmt.Sprintf("apply fixture: %v", err))
		}
		_ = admin.Close()
	}
	os.Exit(m.Run())
}

func live(t *testing.T) {
	t.Helper()
	if super == nil {
		t.Skipf("%s is not set: no live PostgreSQL to test against", envDSN)
	}
}

// client opens a pool as one of the fixture's roles.
func client(t *testing.T, user string, timeout time.Duration) *Client {
	t.Helper()
	live(t)
	c := New(Options{
		Addr: super.Addr, Database: super.Database,
		User: user, Password: "ringsrv_test",
		PoolSize: 2, Timeout: timeout,
		Schemas: []string{"ringsrv_test"}, AppName: "ringsrv-test",
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func query(t *testing.T, c *Client, sql string) (*dbq.Result, error) {
	t.Helper()
	return c.Query(t.Context(), dbq.Statement{SQL: sql, MaxRows: 100, Timeout: 2 * time.Second, Caller: "ringsrv:test"})
}

// The proof is what lets a connection be used at all: the
// read-only role passes it, the one with INSERT does not, and neither does
// the superuser that applied the fixture.
func TestProve(t *testing.T) {
	t.Parallel()

	t.Run("read-only role passes", func(t *testing.T) {
		t.Parallel()
		c := client(t, "ringsrv_test_ro", 2*time.Second)
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.True(t, proof.ReadOnly, proof.Reason)
	})

	t.Run("writing role is a verdict, not a fault", func(t *testing.T) {
		t.Parallel()
		c := client(t, "ringsrv_test_rw", 2*time.Second)
		// The connection hook refuses the connection; the proof reports that
		// as "reachable and writable", which is what refuses a start — an
		// error would read as "could not be asked".
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.False(t, proof.ReadOnly)
		assert.Contains(t, proof.Reason, "other than SELECT")
		_, err = query(t, c, "SELECT 1")
		require.Error(t, err, "and no pooled connection ever exists to query on")
	})

	t.Run("superuser is a verdict too", func(t *testing.T) {
		t.Parallel()
		live(t)
		c := New(Options{Addr: super.Addr, Database: super.Database, User: super.User, Password: super.Password, Timeout: time.Second})
		t.Cleanup(func() { _ = c.Close() })
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.False(t, proof.ReadOnly)
		assert.Contains(t, proof.Reason, "superuser")
	})
}

// Group roles are how a DBA grants SELECT on a dozen bases without a grant per
// base per role, and the proof used to refuse them wholesale: it asked whether
// the user was a member of anything, not what the membership could do. It asks
// the second question now, and that has to hold both ways — a group that reads
// passes, a group that writes is refused even when NOINHERIT hides it.
func TestProve_RoleMembership(t *testing.T) {
	t.Parallel()

	t.Run("a group that holds only SELECT passes", func(t *testing.T) {
		t.Parallel()
		c := client(t, "ringsrv_test_group_ro", 2*time.Second)
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.True(t, proof.ReadOnly, proof.Reason)

		// And the membership is real: the role reads through the group alone.
		res, err := query(t, c, "SELECT count(*) FROM orders")
		require.NoError(t, err)
		require.Len(t, res.Rows, 1)
	})

	t.Run("a writing group is refused through NOINHERIT", func(t *testing.T) {
		t.Parallel()
		c := client(t, "ringsrv_test_hidden_rw", 2*time.Second)
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.False(t, proof.ReadOnly, "SET ROLE reaches the writers group in one statement")
		assert.Contains(t, proof.Reason, "one it may become")
	})

	t.Run("a write on one column is a write", func(t *testing.T) {
		t.Parallel()
		c := client(t, "ringsrv_test_col_rw", 2*time.Second)
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.False(t, proof.ReadOnly)
		assert.Contains(t, proof.Reason, "column privilege")
	})
}

// The server refuses every write inside the READ ONLY transaction — the
// guarantee the text check above only echoes.
func TestReadOnly_Refuses(t *testing.T) {
	c := client(t, "ringsrv_test_ro", 2*time.Second)

	// The writes run one after another, so the count below runs after every
	// one of them has been refused — a parallel subtest would only start
	// once this body returned.
	for _, sql := range []string{
		"INSERT INTO orders (user_id, status, amount) VALUES (1, 'new', 1)",
		"UPDATE orders SET amount = 0",
		"DELETE FROM orders",
		"WITH d AS (DELETE FROM orders RETURNING *) SELECT count(*) FROM d",
		"SELECT * INTO orders_copy FROM orders",
		"CREATE TEMP TABLE t (id int)",
		"EXPLAIN ANALYZE UPDATE orders SET amount = 0",
		"COPY orders TO '/tmp/ringsrv-test.csv'",
		"SELECT nextval('orders_id_seq')",
		"SELECT * FROM orders FOR UPDATE",
		// Two commands in one string: the extended protocol takes one,
		// however the text check was fooled (\r ends a line comment for
		// the server, not for a naive scanner).
		"SHOW search_path -- x\r; SET TRANSACTION READ WRITE; CREATE TEMP TABLE zz(a int); INSERT INTO zz VALUES (1); SELECT count(*) FROM zz",
		// A session setting smuggled into a SELECT dies with the
		// rollback (see below), and an advisory lock is released.
		"SELECT set_config('DateStyle', 'SQL, DMY', false), pg_advisory_lock(424242)",
	} {
		_, err := query(t, c, sql)
		if strings.HasPrefix(sql, "SELECT set_config") {
			require.NoError(t, err, "a SELECT with side effects runs; the effects must not outlive it")
			continue
		}
		require.Error(t, err, sql)
		require.NotErrorIs(t, err, dbq.ErrTimeout, "a refusal, not a hang: %s", sql)
	}

	res, err := query(t, c, "SELECT count(*) FROM orders")
	require.NoError(t, err)
	assert.Equal(t, []any{int64(3)}, res.Rows[0], "nothing changed")

	// The session the SELECT above poisoned went back to the pool clean.
	for range 3 {
		res, err = query(t, c, "SELECT current_setting('DateStyle'), (SELECT count(*) FROM pg_locks WHERE locktype = 'advisory')")
		require.NoError(t, err)
		assert.Equal(t, []any{"ISO, MDY", int64(0)}, res.Rows[0], "the transaction was rolled back and the lock released")
	}
}

// statement_timeout fires on the server, the client reports a timeout the
// domain can name, and the connection goes back to the pool usable.
func TestTimeout(t *testing.T) {
	t.Parallel()
	c := client(t, "ringsrv_test_ro", time.Second)

	started := time.Now()
	_, err := c.Query(t.Context(), dbq.Statement{SQL: "SELECT pg_sleep(60)", Timeout: time.Second})
	require.ErrorIs(t, err, dbq.ErrTimeout)
	assert.Less(t, time.Since(started), 3*time.Second)

	res, err := query(t, c, "SELECT 1 AS one")
	require.NoError(t, err)
	assert.Equal(t, []any{int32(1)}, res.Rows[0])

	// The context's own deadline is the same kind of failure.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = c.Query(ctx, dbq.Statement{SQL: "SELECT pg_sleep(5)", Timeout: 10 * time.Second})
	require.ErrorIs(t, err, dbq.ErrTimeout)
}

// Columns come back in the order the server sent them, typed by OID: the
// shapes the domain then normalises.
func TestQuery_Columns(t *testing.T) {
	t.Parallel()
	c := client(t, "ringsrv_test_ro", 2*time.Second)

	res, err := query(t, c, "SELECT 1 AS b, 2 AS a")
	require.NoError(t, err)
	assert.Equal(t, []dbq.Column{{Name: "b", Type: "integer"}, {Name: "a", Type: "integer"}}, res.Columns)
	assert.Equal(t, [][]any{{int32(1), int32(2)}}, res.Rows, "int4 comes as int32; the domain does not care")

	res, err = query(t, c, "SELECT id, email, balance, created_at, avatar, tags, meta, phone FROM users ORDER BY id")
	require.NoError(t, err)
	names := make([]string, len(res.Columns))
	types := make([]string, len(res.Columns))
	for i, col := range res.Columns {
		names[i], types[i] = col.Name, col.Type
	}
	assert.Equal(t, []string{"id", "email", "balance", "created_at", "avatar", "tags", "meta", "phone"}, names)
	assert.Equal(t, []string{"bigint", "text", "numeric", "timestamp with time zone", "bytea", "text[]", "jsonb", "text"}, types)
	require.Len(t, res.Rows, 2)

	alice := res.Rows[0]
	assert.Equal(t, int64(1), alice[0])
	assert.Equal(t, "alice@example.com", alice[1])
	assert.Equal(t, "10.50", alice[2], "numeric stays the server's text: no digit is lost")
	at, ok := alice[3].(time.Time)
	require.True(t, ok)
	assert.Equal(t, "2026-09-02T12:04:05Z", at.UTC().Format(time.RFC3339))
	assert.Equal(t, []byte{0x00, 0xff}, alice[4])
	assert.Equal(t, []string{"a", "b"}, alice[5])
	assert.JSONEq(t, `{"k": [1, 2]}`, string(alice[6].(json.RawMessage)))
	assert.Equal(t, "+7 999 123-45-67", alice[7])

	bob := res.Rows[1]
	assert.Nil(t, bob[4], "NULL is nil")
	assert.Nil(t, bob[7])

	res, err = c.Query(t.Context(), dbq.Statement{SQL: "SELECT id FROM users ORDER BY id", MaxRows: 1, Timeout: time.Second})
	require.NoError(t, err)
	assert.Len(t, res.Rows, 1, "the cap stops keeping rows")

	res, err = query(t, c, "SELECT id FROM users WHERE false")
	require.NoError(t, err)
	assert.Empty(t, res.Rows)
	assert.NotNil(t, res.Rows, "an empty result is an empty array, not null")
}

// Everything the introspection promises, read from the
// catalogue tables of a schema whose owner wrote it down.
func TestIntrospect(t *testing.T) {
	t.Parallel()
	c := client(t, "ringsrv_test_ro", 2*time.Second)

	tables, err := c.Tables(t.Context())
	require.NoError(t, err)
	byName := map[string]dbq.Table{}
	for _, tb := range tables {
		byName[tb.Name] = tb
	}
	require.Len(t, tables, 3)
	assert.Equal(t, "table", byName["users"].Kind)
	assert.Equal(t, "view", byName["paid_orders"].Kind)
	assert.Equal(t, "Registered users", byName["users"].Comment)
	assert.EqualValues(t, 2, byName["users"].RowsEstimate, "reltuples after ANALYZE, not count(*)")
	assert.Equal(t, "ringsrv_test", byName["orders"].Schema)

	schema, err := c.Introspect(t.Context(), "orders")
	require.NoError(t, err)
	assert.Equal(t, "orders", schema.Table, "bare in the first schema of the search path, as a query names it")
	assert.Equal(t, "ringsrv_test", schema.Schema, "the schema is unabbreviated even when Table drops it — the pointer to the code is resolved by it")
	assert.Equal(t, "Orders; amount in rubles", schema.Comment)
	assert.EqualValues(t, 3, schema.RowsEstimate)
	assert.Equal(t, []string{"id"}, schema.PrimaryKey)
	assert.Equal(t, []dbq.Relation{{Column: "user_id", References: "ringsrv_test.users(id)"}}, schema.Relations)
	require.Len(t, schema.Checks, 1)
	assert.Contains(t, schema.Checks[0], "amount >=")

	require.Len(t, schema.Columns, 4)
	status := schema.Columns[2]
	assert.Equal(t, "status", status.Name)
	assert.Equal(t, "order_status", status.Type, "named as a query names it: the schema is on the search path")
	assert.False(t, status.Nullable)
	assert.Equal(t, "Lifecycle of an order", status.Comment)
	assert.Equal(t, []string{"new", "paid", "cancelled"}, status.Enum, "labels in declaration order")
	assert.True(t, schema.Columns[3].Nullable == false && schema.Columns[3].Name == "amount")

	users, err := c.Introspect(t.Context(), "ringsrv_test.users")
	require.NoError(t, err)
	assert.Equal(t, "ringsrv_test", users.Schema)
	assert.Equal(t, "Login e-mail, unique per user", users.Columns[1].Comment)
	assert.True(t, users.Columns[2].Nullable, "phone may be NULL")
	assert.Empty(t, users.Relations)

	_, err = c.Introspect(t.Context(), "nope")
	require.ErrorIs(t, err, dbq.ErrUnknownTable)
	_, err = c.Introspect(t.Context(), "nosuch.users")
	require.ErrorIs(t, err, dbq.ErrUnknownTable, "a schema that is not there")

	// What the role can read, asked of the server rather than assumed: a schema
	// declared in the entry and missing here is a grant that was never given,
	// and its tables answer "does not exist" like a table nobody created.
	scope, err := c.Scope(t.Context())
	require.NoError(t, err)
	assert.Equal(t, super.Database, scope.Database)
	assert.Contains(t, scope.Visible, "ringsrv_test")
	assert.NotContains(t, scope.Visible, "pg_catalog", "the server's own schemas say nothing about this database")
	assert.NotContains(t, scope.Visible, "information_schema")

	_, err = query(t, c, "SELECT * FROM nope")
	require.ErrorIs(t, err, dbq.ErrUnknownTable, "42P01 from a query is the same finding")
}

// The values the domain has to carry exactly: an id past 2^53, NaN, and a
// result wider than the budget.
func TestQuery_Extremes(t *testing.T) {
	t.Parallel()
	c := client(t, "ringsrv_test_ro", 2*time.Second)

	res, err := query(t, c, "SELECT 9223372036854775807::bigint AS big, 'NaN'::float8 AS nan")
	require.NoError(t, err)
	assert.Equal(t, int64(9223372036854775807), res.Rows[0][0])
	f, ok := res.Rows[0][1].(float64)
	require.True(t, ok)
	assert.True(t, math.IsNaN(f))

	_, err = c.Query(t.Context(), dbq.Statement{
		SQL: "SELECT repeat('x', 100000) FROM generate_series(1, 100)", MaxRows: 1000, MaxBytes: 1 << 20, Timeout: 5 * time.Second,
	})
	require.ErrorIs(t, err, dbq.ErrResultTooLarge, "ten megabytes against a one-megabyte budget")

	res, err = query(t, c, "SELECT 1")
	require.NoError(t, err)
	assert.Len(t, res.Rows, 1, "the pool is usable after the cancel")
}

// The caller's name reaches pg_stat_activity for the duration of the query
// and does not outlive the transaction.
func TestAppName(t *testing.T) {
	t.Parallel()
	c := client(t, "ringsrv_test_ro", 2*time.Second)

	res, err := c.Query(t.Context(), dbq.Statement{
		SQL:     "SELECT application_name FROM pg_stat_activity WHERE pid = pg_backend_pid()",
		Timeout: time.Second, Caller: "ringsrv:alice",
	})
	require.NoError(t, err)
	assert.Equal(t, []any{"ringsrv:alice"}, res.Rows[0])

	res, err = c.Query(t.Context(), dbq.Statement{
		SQL:     "SELECT application_name FROM pg_stat_activity WHERE pid = pg_backend_pid()",
		Timeout: time.Second,
	})
	require.NoError(t, err)
	assert.Equal(t, []any{"ringsrv-test"}, res.Rows[0], "SET LOCAL ended with the transaction")
}

// Sanity of the classification without a server.
func TestClassify(t *testing.T) {
	t.Parallel()
	require.NoError(t, classify(nil))
	plain := errors.New("x")
	assert.Equal(t, plain, classify(plain))
	assert.ErrorIs(t, classify(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)), dbq.ErrTimeout)
}
