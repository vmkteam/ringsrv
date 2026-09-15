package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/chdb"
	"github.com/vmkteam/ringsrv/pkg/db"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/go-pg/pg/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// The tools over the real drivers and the real transport, against the
// local stands the driver packages prepared: their fixtures
// leave the read-only users behind, so this test needs the same variables
// and runs after theirs. Skipped without them.
func TestDBQuery_Live(t *testing.T) {
	pgDSN, chDSN := os.Getenv("RINGSRV_TEST_PG"), os.Getenv("RINGSRV_TEST_CH")
	if pgDSN == "" && chDSN == "" {
		t.Skip("RINGSRV_TEST_PG and RINGSRV_TEST_CH are not set: no live database to test against")
	}

	// One row per stand that is up: the catalogue entry, the client and the
	// table the fixture left behind.
	type stand struct {
		name, driver, addr, table string
		client                    dbq.Client
		extra                     string
		schemas                   []string
	}
	var stands []stand
	if pgDSN != "" {
		opts, err := pg.ParseURL(pgDSN)
		require.NoError(t, err)
		stands = append(stands, stand{
			name: "pg", driver: target.DriverPostgres, addr: opts.Addr, table: "users",
			client: db.New(db.Options{
				Addr: opts.Addr, Database: opts.Database, User: "ringsrv_test_ro", Password: "ringsrv_test",
				Timeout: 2 * time.Second, Schemas: []string{"ringsrv_test"}, AppName: "ringsrv-test",
			}),
			extra:   "\nRedact      = \"warn\"\nRedactRules = [\"email\"]\nSchemas     = [\"ringsrv_test\"]",
			schemas: []string{"ringsrv_test"},
		})
	}
	if chDSN != "" {
		opts, err := clickhouse.ParseDSN(chDSN)
		require.NoError(t, err)
		c, err := chdb.New(chdb.Options{Addr: opts.Addr[0], Database: "ringsrv_test", User: "ringsrv_test_ro", Password: "ringsrv_test", Timeout: 2 * time.Second})
		require.NoError(t, err)
		stands = append(stands, stand{name: "ch", driver: target.DriverClickHouse, addr: opts.Addr[0], table: "events", client: c})
	}

	var toml strings.Builder
	toml.WriteString(`
Env = "dev"

[Profiles.prom]
Description  = "dev · Prometheus"
BaseURL      = "https://prom.example.com"
AllowMethods = ["GET"]
AllowPaths   = ["^/api/v1/query$"]

[Roles.analyst]
Description = "data"
Groups      = ["analysts"]
Tools       = ["db_query", "db_introspect"]
Targets     = [`)
	m := dbq.NewManager(dbq.Options{JQTimeout: time.Second, MaxBytes: 1 << 20})
	for _, st := range stands {
		fmt.Fprintf(&toml, "%q, ", st.name)
		require.NoError(t, m.Add(dbq.Target{
			Name: st.name, Driver: st.driver, MaxRows: 100, Timeout: 2 * time.Second, Client: st.client,
			Redact: redact.ModeWarn, RedactRules: []string{redact.RuleEmail},
			// The declared scope, as pkg/app wires it from the entry below:
			// the fixture puts its tables in a schema of its own, which for
			// ClickHouse is the database and for PostgreSQL the search path.
			Database: "ringsrv_test", Schemas: st.schemas,
		}))
	}
	toml.WriteString("]\n")
	for _, st := range stands {
		fmt.Fprintf(&toml, "\n[Databases.%s]\nDescription = \"dev · %s local stand\"\nDriver      = %q\nAddr        = %q\nDatabase    = \"ringsrv_test\"\nUser        = \"ringsrv_test_ro\"\nPassword    = \"ringsrv_test\"%s\n",
			st.name, st.driver, st.driver, st.addr, st.extra)
	}
	cat, err := target.Parse([]byte(toml.String()))
	require.NoError(t, err)
	require.NoError(t, m.Prove(t.Context()), "the fixtures' read-only users pass the proof")
	t.Cleanup(func() { _ = m.Close() })

	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)
	srv := mcpkit.NewServer(NewMCP(MCPDeps{
		Info: mcp.ServerInfo{Name: "ringsrv", Version: "test"}, Docs: lib, Targets: cat, DB: m,
		JQTimeout: time.Second, MaxBytes: 1 << 20, Sessions: ring.NewSessions(time.Hour, nil),
		Logger: embedlog.Logger{}, IsDevel: true,
	}), embedlog.Logger{})

	// A dev instance without a principal reaches every database.
	callTool := func(t *testing.T, name string, args map[string]any) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var env struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env), rr.Body.String())
		require.False(t, env.Result.IsError, env.Result.Content[0].Text)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(env.Result.Content[0].Text), &out))
		return out
	}

	for _, st := range stands {
		t.Run(st.name, func(t *testing.T) {
			n, table := st.name, st.table
			tables := callTool(t, ToolDBIntrospect, map[string]any{"target": n})
			assert.NotEmpty(t, tables["tables"])

			schema := callTool(t, ToolDBIntrospect, map[string]any{"target": n, "table": table})
			assert.NotEmpty(t, schema["columns"])
			assert.Equal(t, "dev", schema["env"])

			out := callTool(t, ToolDBQuery, map[string]any{"target": n, "sql": "SELECT id FROM " + table + " ORDER BY id", "max_rows": 1})
			assert.Equal(t, "dev", out["env"])
			assert.Contains(t, []any{"postgres", "clickhouse"}, out["driver"])
			assert.Equal(t, true, out["truncated"], "two rows in the fixture, one asked for")
			assert.EqualValues(t, 1, out["rows_returned"])
			data, ok := out["data"].(map[string]any)
			require.True(t, ok)
			assert.Len(t, data["rows"], 1)
			assert.NotEmpty(t, data["columns"])
		})
	}
}
