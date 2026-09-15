package chdb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests run against a live ClickHouse: RINGSRV_TEST_CH is the DSN of a user
// with access management on the local stand, and without it the package is
// skipped. The fixture in testdata is applied once per run.
const envDSN = "RINGSRV_TEST_CH"

var admin *clickhouse.Options

func TestMain(m *testing.M) {
	if dsn := os.Getenv(envDSN); dsn != "" {
		applyFixture(dsn)
	}
	os.Exit(m.Run())
}

// applyFixture runs testdata/schema.sql statement by statement — the native
// protocol takes one per query — and keeps the admin options for the tests.
func applyFixture(dsn string) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", envDSN, err))
	}
	admin = opts
	schema, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		panic(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	for stmt := range strings.SplitSeq(string(schema), ";\n") {
		if strings.TrimSpace(stripComments(stmt)) == "" {
			continue
		}
		if err := conn.Exec(context.Background(), stmt); err != nil {
			panic(fmt.Sprintf("apply fixture: %v\n%s", err, stmt))
		}
	}
}

func stripComments(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func live(t *testing.T) {
	t.Helper()
	if admin == nil {
		t.Skipf("%s is not set: no live ClickHouse to test against", envDSN)
	}
}

// client opens a pool as one of the fixture's users.
func client(t *testing.T, user string) *Client {
	t.Helper()
	live(t)
	c, err := New(Options{Addr: admin.Addr[0], Database: "ringsrv_test", User: user, Password: "ringsrv_test", MaxOpenConns: 2, Timeout: 2 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// proven opens the pool and runs the probe, which is what a query needs to
// know its settings.
func proven(t *testing.T, user string) *Client {
	t.Helper()
	c := client(t, user)
	proof, err := c.ProveReadOnly(t.Context())
	require.NoError(t, err)
	require.True(t, proof.ReadOnly, proof.Reason)
	return c
}

func query(t *testing.T, c *Client, sql string) (*dbq.Result, error) {
	t.Helper()
	return c.Query(t.Context(), dbq.Statement{SQL: sql, MaxRows: 100, Timeout: 2 * time.Second, Caller: "ringsrv:test"})
}

// The probe tells the four users apart and reports which
// settings the profile lets the read-only one change.
func TestProve(t *testing.T) {
	t.Parallel()

	t.Run("read-only user passes, settings are READONLY", func(t *testing.T) {
		t.Parallel()
		c := proven(t, "ringsrv_test_ro")
		assert.Equal(t, []string{settingMaxResultBytes, settingMaxResultRows, settingResultOverflowMode}, c.Changeable(),
			"max_execution_time, readonly and allow_ddl are pinned by the profile")
	})

	t.Run("read-only user with free settings", func(t *testing.T) {
		t.Parallel()
		c := proven(t, "ringsrv_test_free")
		assert.Contains(t, c.Changeable(), settingMaxExecutionTime)
		assert.NotContains(t, c.Changeable(), settingReadonly)
	})

	t.Run("INSERT grant is refused", func(t *testing.T) {
		t.Parallel()
		c := client(t, "ringsrv_test_rw")
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.False(t, proof.ReadOnly)
		assert.Contains(t, proof.Reason, "INSERT on ringsrv_test.*")
	})

	t.Run("allow_ddl is refused", func(t *testing.T) {
		t.Parallel()
		c := client(t, "ringsrv_test_ddl")
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.False(t, proof.ReadOnly)
		assert.Contains(t, proof.Reason, "allow_ddl = 1")
	})

	t.Run("admin is refused on readonly", func(t *testing.T) {
		t.Parallel()
		live(t)
		c, err := New(Options{Addr: admin.Addr[0], Database: admin.Auth.Database, User: admin.Auth.Username, Password: admin.Auth.Password})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		proof, err := c.ProveReadOnly(t.Context())
		require.NoError(t, err)
		assert.False(t, proof.ReadOnly)
		assert.Contains(t, proof.Reason, "readonly = 0")
	})

	t.Run("unreachable server is an error, not a verdict", func(t *testing.T) {
		t.Parallel()
		live(t)
		c, err := New(Options{Addr: "127.0.0.1:1", Database: "x", User: "x", Password: "x"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		_, err = c.ProveReadOnly(t.Context())
		require.Error(t, err)
	})
}

// The server refuses what the user may not do; the client only carries the
// text.
func TestReadOnly_Refuses(t *testing.T) {
	c := proven(t, "ringsrv_test_ro")

	// The writes run one after another, so the count below runs after every
	// one of them has been refused.
	for _, sql := range []string{
		"INSERT INTO events (id) VALUES (3)",
		"CREATE TABLE t (id UInt8) ENGINE = Memory",
		"ALTER TABLE events DELETE WHERE id = 1",
		"SELECT * FROM url('http://127.0.0.1:1/', 'CSV', 'a String')",
		"SET max_execution_time = 600",
	} {
		_, err := query(t, c, sql)
		require.Error(t, err, sql)
		require.NotErrorIs(t, err, dbq.ErrTimeout, sql)
	}

	// INTO OUTFILE is a feature of the client, not the server: over the
	// native protocol the server answers the rows and writes nothing. The
	// domain refuses the clause anyway, for the clarity of the refusal.
	res, err := query(t, c, "SELECT id FROM events INTO OUTFILE '/tmp/ringsrv-test.csv'")
	require.NoError(t, err)
	assert.Len(t, res.Rows, 2)

	res, err = query(t, c, "SELECT count() FROM events")
	require.NoError(t, err)
	assert.Equal(t, []any{uint64(2)}, res.Rows[0], "nothing changed")
}

// A slow query is cut by the server when the profile lets the client ask
// for it, and by the context when it does not; both read as a timeout.
func TestTimeout(t *testing.T) {
	t.Parallel()

	t.Run("max_execution_time on the server", func(t *testing.T) {
		t.Parallel()
		c := proven(t, "ringsrv_test_free")
		started := time.Now()
		_, err := c.Query(t.Context(), dbq.Statement{SQL: "SELECT sleep(3), sleep(3), sleep(3)", Timeout: time.Second})
		require.ErrorIs(t, err, dbq.ErrTimeout)
		assert.Less(t, time.Since(started), 5*time.Second)
		assert.Contains(t, err.Error(), "Timeout exceeded", "the server's own text is kept")
	})

	t.Run("context deadline when the profile pins the setting", func(t *testing.T) {
		t.Parallel()
		c := proven(t, "ringsrv_test_ro")
		// Two seconds, not one: past a second clickhouse-go would turn the
		// deadline into a max_execution_time of its own, which this profile
		// refuses — detachDeadline is what keeps that from happening.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		started := time.Now()
		_, err := c.Query(ctx, dbq.Statement{SQL: "SELECT sleep(3), sleep(3), sleep(3)", Timeout: 2 * time.Second})
		require.ErrorIs(t, err, dbq.ErrTimeout)
		assert.Less(t, time.Since(started), 5*time.Second)

		ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		res, qerr := c.Query(ctx, dbq.Statement{SQL: "SELECT 1", MaxRows: 1, Timeout: 2 * time.Second})
		require.NoError(t, qerr, "a deadline on the context is not a setting on the wire")
		assert.Len(t, res.Rows, 1)

		res, err = query(t, c, "SELECT 1")
		require.NoError(t, err)
		assert.Len(t, res.Rows, 1, "the pool is usable after")
	})
}

// Columns keep the server's order and types; values come typed the way
// clickhouse-go scans them, for the domain to normalise.
func TestQuery_Columns(t *testing.T) {
	t.Parallel()
	c := proven(t, "ringsrv_test_ro")

	res, err := query(t, c, "SELECT 1 AS b, 2 AS a")
	require.NoError(t, err)
	assert.Equal(t, []dbq.Column{{Name: "b", Type: "UInt8"}, {Name: "a", Type: "UInt8"}}, res.Columns)

	res, err = query(t, c, "SELECT id, ts, kind, amount, tags, attrs, note, ip FROM events ORDER BY id")
	require.NoError(t, err)
	types := make([]string, len(res.Columns))
	for i, col := range res.Columns {
		types[i] = col.Type
	}
	assert.Equal(t, []string{"UInt64", "DateTime64(3, 'UTC')", "LowCardinality(String)", "Decimal(12, 2)", "Array(String)", "Map(String, String)", "Nullable(String)", "IPv4"}, types)
	require.Len(t, res.Rows, 2)

	first := res.Rows[0]
	assert.Equal(t, uint64(1), first[0])
	ts, ok := first[1].(time.Time)
	require.True(t, ok)
	assert.Equal(t, "2026-09-02T12:04:05.123Z", ts.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	assert.Equal(t, "scan", first[2])
	assert.Equal(t, "10.5", fmt.Sprint(first[3]), "a decimal with a String method")
	assert.Equal(t, []string{"a", "b"}, first[4])
	assert.Equal(t, map[string]string{"k": "v"}, first[5])
	note, ok := first[6].(*string)
	require.True(t, ok, "Nullable comes as a pointer")
	assert.Equal(t, "note", *note)
	assert.Equal(t, "10.0.0.1", fmt.Sprint(first[7]))

	second := res.Rows[1]
	assert.Nil(t, second[6], "NULL is nil")

	res, err = c.Query(t.Context(), dbq.Statement{SQL: "SELECT number FROM numbers(1000)", MaxRows: 5, Timeout: time.Second})
	require.NoError(t, err)
	assert.Len(t, res.Rows, 5, "MaxRows stops the read")

	res, err = query(t, c, "SELECT id FROM events WHERE id = 42")
	require.NoError(t, err)
	assert.Empty(t, res.Rows)
	assert.Equal(t, "id", res.Columns[0].Name, "columns are known even without rows")

	res, err = query(t, c, "SELECT toUInt64(18446744073709551615) AS big, nan AS n")
	require.NoError(t, err)
	assert.Equal(t, uint64(18446744073709551615), res.Rows[0][0], "the widest id comes through exactly")

	_, err = c.Query(t.Context(), dbq.Statement{SQL: "SELECT repeat('x', 100000) FROM numbers(100)", MaxRows: 1000, MaxBytes: 1 << 20, Timeout: 5 * time.Second})
	require.ErrorIs(t, err, dbq.ErrResultTooLarge, "ten megabytes against a one-megabyte budget")
}

// Everything the introspection promises, from system.tables
// and system.columns.
func TestIntrospect(t *testing.T) {
	t.Parallel()
	c := proven(t, "ringsrv_test_ro")

	tables, err := c.Tables(t.Context())
	require.NoError(t, err)
	require.Len(t, tables, 1)
	assert.Equal(t, "events", tables[0].Name)
	assert.Equal(t, "ReplacingMergeTree", tables[0].Kind)
	assert.Equal(t, "Events of the scanner", tables[0].Comment)
	assert.EqualValues(t, 2, tables[0].RowsEstimate)

	schema, err := c.Introspect(t.Context(), "events")
	require.NoError(t, err)
	assert.Equal(t, "events", schema.Table)
	assert.Equal(t, "ReplacingMergeTree", schema.Engine)
	assert.Equal(t, "id, ts", schema.SortingKey)
	assert.Equal(t, "toYYYYMM(ts)", schema.PartitionKey)
	assert.Equal(t, []string{"id", "ts"}, schema.PrimaryKey)
	assert.EqualValues(t, 2, schema.RowsEstimate)
	require.Len(t, schema.Columns, 8)
	assert.Equal(t, dbq.ColumnInfo{Name: "ts", Type: "DateTime64(3, 'UTC')", Comment: "When it happened"}, schema.Columns[1])
	assert.True(t, schema.Columns[6].Nullable)
	assert.False(t, schema.Columns[0].Nullable)

	_, err = c.Introspect(t.Context(), "nope")
	require.ErrorIs(t, err, dbq.ErrUnknownTable)

	_, err = query(t, c, "SELECT * FROM nope")
	require.ErrorIs(t, err, dbq.ErrUnknownTable, "UNKNOWN_TABLE from a query is the same finding")
	assert.Contains(t, strings.ToLower(err.Error()), "nope", "the server names the table it did not find")

	// The databases the user may read, straight from the server: a base missing
	// here answers UNKNOWN_TABLE for every table it holds — indistinguishable,
	// without this list, from a base that is simply empty.
	scope, err := c.Scope(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "ringsrv_test", scope.Database)
	assert.Equal(t, []string{"ringsrv_test"}, scope.Visible,
		"the server filters system.databases by grant, and a read-only user of one base is granted one base — "+
			"which is why a base missing from this list answers UNKNOWN_TABLE for every table it holds")
}
