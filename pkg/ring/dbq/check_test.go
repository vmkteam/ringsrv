package dbq

import (
	"strings"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The text check is the first line and only that: it makes the refusal
// readable before the server is asked. The table is every form the check
// refuses, plus the ways a literal or a comment could fool it.
func TestCheckSQL(t *testing.T) {
	t.Parallel()
	pg, ch := target.DriverPostgres, target.DriverClickHouse
	ok := map[string][]string{
		pg: {
			"SELECT 1",
			"select id from users where email = 'a;b'",
			"  \n\t WITH t AS (SELECT 1) SELECT * FROM t;",
			"-- leading comment\nSELECT 1",
			"/* block\n comment */ SELECT 1",
			"EXPLAIN SELECT 1",
			"EXPLAIN (COSTS, VERBOSE) SELECT 1",
			"EXPLAIN VERBOSE SELECT 1",
			"SHOW search_path",
			"TABLE users",
			"VALUES (1), (2)",
			"SELECT 'it''s; fine'",
			"SELECT E'\\'; still one'",
			"SELECT $$a; b$$, $fn$c; d$fn$",
			"SELECT 1 -- trailing; comment",
			"SELECT \"weird;name\" FROM t",
			"SELECT $я$a; b$я$",
		},
		ch: {
			"SELECT 1",
			"WITH 1 AS x SELECT x",
			"DESCRIBE TABLE events",
			"DESC events",
			"EXISTS events",
			"SHOW TABLES",
			"EXPLAIN SELECT 1",
			"SELECT formatDateTime(now(), '%Y')",
			"SELECT 'a\\'; b'",
			"SELECT `odd;name` FROM t",
			"SELECT 1 # a; comment",
		},
	}
	for driver, list := range ok {
		for _, sql := range list {
			t.Run(driver+" accepts "+sql, func(t *testing.T) {
				t.Parallel()
				assert.NoError(t, CheckSQL(driver, sql))
			})
		}
	}

	bad := map[string][]struct{ sql, want string }{
		pg: {
			{"", "empty"},
			{"   -- only a comment", "empty"},
			{"INSERT INTO t VALUES (1)", "SELECT, WITH, EXPLAIN, SHOW, TABLE, VALUES"},
			{"DELETE FROM t", "starts with one of"},
			{"COPY t TO '/tmp/x'", "starts with one of"},
			{"SELECT 1; SELECT 2", "one statement"},
			{"SELECT 1; DROP TABLE t", "one statement"},
			{"EXPLAIN ANALYZE SELECT 1", "ANALYZE"},
			{"explain analyse update t set a = 1", "ANALYZE"},
			{"EXPLAIN (ANALYZE, BUFFERS) SELECT 1", "ANALYZE"},
			{"EXPLAIN (analyze true) SELECT 1", "ANALYZE"},
			{"EXPLAIN VERBOSE ANALYZE SELECT 1", "ANALYZE"},
			{"EXPLAIN (ANALYSE, BUFFERS) SELECT 1", "ANALYZE"},
			{"DESCRIBE t", "starts with one of"},
			{"(SELECT 1)", "starts with one of"},
			// A carriage return ends a line comment for the server, so the
			// second statement is real; the mask has to see it.
			{"SHOW search_path -- x\r; SET TRANSACTION READ WRITE", "one statement"},
			{"SELECT $я$'$я$; DROP TABLE t", "one statement"},
		},
		ch: {
			{"INSERT INTO t VALUES (1)", "SELECT, WITH, EXPLAIN, SHOW, DESCRIBE, DESC, EXISTS"},
			{"SELECT 1; SELECT 2", "one statement"},
			{"SELECT * FROM t INTO OUTFILE '/tmp/x'", "OUTFILE"},
			{"select * from t format JSON", "FORMAT"},
			{"SELECT 1 FORMAT Pretty;", "FORMAT"},
			{"TABLE t", "starts with one of"},
			{"SET max_execution_time = 600", "starts with one of"},
			{"SELECT 1 # x\r; DROP TABLE t", "one statement"},
		},
	}
	for driver, list := range bad {
		for _, tc := range list {
			t.Run(driver+" refuses "+tc.sql, func(t *testing.T) {
				t.Parallel()
				err := CheckSQL(driver, tc.sql)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.want)
			})
		}
	}

	require.Error(t, CheckSQL("mysql", "SELECT 1"), "an unknown driver is a wiring error, not a pass")
}

// The LIMIT is written as given — the caller asks for one row more than
// the answer will hold, and that row is the truncated flag. The newline
// before the bracket survives a trailing comment; the trailing ';' does not
// survive at all.
func TestWrapLimit(t *testing.T) {
	t.Parallel()
	pg := target.DriverPostgres
	cases := map[string]struct{ sql, want string }{
		"plain":            {"SELECT 1", "SELECT * FROM (\nSELECT 1\n) AS ringsrv_q LIMIT 201"},
		"trailing ;":       {"SELECT 1;", "SELECT * FROM (\nSELECT 1\n) AS ringsrv_q LIMIT 201"},
		"; and whitespace": {"SELECT 1 ;  \n", "SELECT * FROM (\nSELECT 1\n) AS ringsrv_q LIMIT 201"},
		"trailing comment": {"SELECT 1 -- all; of it", "SELECT * FROM (\nSELECT 1 -- all; of it\n) AS ringsrv_q LIMIT 201"},
		"; in a literal":   {"SELECT 'a;'", "SELECT * FROM (\nSELECT 'a;'\n) AS ringsrv_q LIMIT 201"},
		"with":             {"WITH t AS (SELECT 1) SELECT * FROM t", "SELECT * FROM (\nWITH t AS (SELECT 1) SELECT * FROM t\n) AS ringsrv_q LIMIT 201"},
		"lower case":       {"select 1", "SELECT * FROM (\nselect 1\n) AS ringsrv_q LIMIT 201"},
		"leading comment":  {"-- why\nSELECT 1", "SELECT * FROM (\n-- why\nSELECT 1\n) AS ringsrv_q LIMIT 201"},
		"table":            {"TABLE t", "SELECT * FROM (\nTABLE t\n) AS ringsrv_q LIMIT 201"},
		"explain as is":    {"EXPLAIN SELECT 1", "EXPLAIN SELECT 1"},
		"show as is":       {"SHOW search_path;", "SHOW search_path;"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, WrapLimit(pg, tc.sql, 201))
		})
	}
	assert.Equal(t, "DESCRIBE TABLE t", WrapLimit(target.DriverClickHouse, "DESCRIBE TABLE t", 11))
	assert.True(t, strings.HasSuffix(WrapLimit(target.DriverClickHouse, "SELECT 1", 11), "LIMIT 11"))
}

func TestValidTable(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"users", "public.users", "_x1", "ringsrv_test.orders"} {
		assert.True(t, ValidTable(ok), ok)
	}
	for _, bad := range []string{"", "a.b.c", "users;", "1abc", "a-b", "pg_class where 1=1", `"users"`} {
		assert.False(t, ValidTable(bad), bad)
	}
}

// mask keeps every length, so an index into the masked text is an index
// into the original.
func TestMask(t *testing.T) {
	t.Parallel()
	in := "SELECT 'a;b', \"c;d\" -- e;f\nFROM t /* g;h */ WHERE x = E'\\';' AND y = $$i;j$$"
	out := mask(in, target.DriverPostgres)
	assert.Len(t, out, len(in))
	assert.NotContains(t, out, ";")
	assert.Contains(t, out, "SELECT")
	assert.Contains(t, out, "FROM t")
	assert.Contains(t, out, "WHERE x =")
	assert.Equal(t, strings.Count(in, "\n"), strings.Count(out, "\n"), "line structure is kept")

	ch := mask("SELECT `a;b`, 'c\\';d' FROM t # e;f", target.DriverClickHouse)
	assert.NotContains(t, ch, ";")

	cr := mask("SELECT 1 -- a\r; DROP TABLE t", target.DriverPostgres)
	assert.Contains(t, cr, "; DROP TABLE t", "a carriage return ends the comment, as it does for the server")
}
