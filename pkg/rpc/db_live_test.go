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
	"github.com/vmkteam/ringsrv/pkg/ring/kb"
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
		// column is one the note describes, on the table the fixture made.
		column  string
		client  dbq.Client
		extra   string
		schemas []string
	}
	var stands []stand
	if pgDSN != "" {
		opts, err := pg.ParseURL(pgDSN)
		require.NoError(t, err)
		stands = append(stands, stand{
			name: "pg", driver: target.DriverPostgres, addr: opts.Addr, table: "users", column: "email",
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
		stands = append(stands, stand{name: "ch", driver: target.DriverClickHouse, addr: opts.Addr[0], table: "events", column: "kind", client: c})
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
	// A note on each stand's table, and a decoy beside it: a note on a table
	// of the same name in another schema, which must not be taken for it.
	files, sources := map[string]string{}, map[string]string{}
	for _, st := range stands {
		sources[st.name] = st.name
		files["schema/"+st.name+"/"+st.table+".md"] = fmt.Sprintf("---\ntable: %s\nsource: %s\npurpose: \"Из пакета: %s\"\ncolumns:\n"+
			"  - name: %s\n    description: \"Из пакета: %s\"\n  - name: ghost\n    description: Колонка, которой в таблице нет\n---\n",
			st.table, st.name, st.table, st.column, st.column)
		files["schema/"+st.name+"/elsewhere_"+st.table+".md"] = fmt.Sprintf("---\ntable: elsewhere.%s\nsource: %s\npurpose: Таблица другой схемы\n---\n", st.table, st.name)
	}
	pack, err := kb.Load(kb.Options{Dir: writeKB(t, files), URIScheme: kbScheme, Sources: sources}, kb.Deps{Catalog: cat, ReservedTopics: ReservedSheetNames(cat)})
	require.NoError(t, err)
	srv := mcpkit.NewServer(NewMCP(MCPDeps{
		Info: mcp.ServerInfo{Name: "ringsrv", Version: "test"}, Docs: lib, KB: pack, Targets: cat, DB: m,
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
			topic := "schema/" + n + "/" + table

			// The list names the note of the table the server reported, under
			// the schema the driver put it in.
			tables := callTool(t, ToolDBIntrospect, map[string]any{"target": n})
			list, ok := tables["tables"].([]any)
			require.True(t, ok, "%v", tables)
			var listed map[string]any
			for _, it := range list {
				if row, _ := it.(map[string]any); row["name"] == table {
					listed = row
				}
			}
			require.NotNil(t, listed, "%s is in the list", table)
			assert.Equal(t, topic, listed["doc"], "the list names the note")

			batch := callTool(t, ToolDBIntrospect, map[string]any{"target": n, "tables": []any{table}})
			assert.Equal(t, "dev", batch["env"])
			schema, ok := liveItem(t, batch)["schema"].(map[string]any)
			require.True(t, ok, "%v", batch)
			assert.NotEmpty(t, schema["columns"])

			// The note beside the base's own schema: the pack's meaning, the
			// columns it names and the table lacks, and the base's comment kept.
			note, ok := schema["doc"].(map[string]any)
			require.True(t, ok, "the note is merged: %v", schema)
			assert.Equal(t, topic, note["topic"])
			assert.Equal(t, "Из пакета: "+table, note["purpose"])
			assert.Equal(t, []any{"ghost"}, note["unmatched"], "a described column the live table lacks is named")
			assert.NotEmpty(t, schema["comment"], "the base's own comment stays")
			var described map[string]any
			for _, c := range schema["columns"].([]any) {
				if col, _ := c.(map[string]any); col["name"] == st.column {
					described, _ = col["doc"].(map[string]any)
				}
			}
			require.NotNil(t, described, "column %s carries its note", st.column)
			assert.Equal(t, "Из пакета: "+st.column, described["description"])

			out := callTool(t, ToolDBQuery, map[string]any{"target": n, "queries": []any{
				map[string]any{"sql": "SELECT id FROM " + table + " ORDER BY id", "max_rows": 1},
			}})
			assert.Equal(t, "dev", out["env"])
			assert.Contains(t, []any{"postgres", "clickhouse"}, out["driver"])
			item := liveItem(t, out)
			assert.Equal(t, true, item["truncated"], "two rows in the fixture, one asked for")
			assert.EqualValues(t, 1, item["rows_returned"])
			data, ok := item["data"].(map[string]any)
			require.True(t, ok)
			assert.Len(t, data["rows"], 1)
			assert.NotEmpty(t, data["columns"])
		})
	}
}

// liveItem is the one result of a batch of one, which has to have answered.
func liveItem(t *testing.T, batch map[string]any) map[string]any {
	t.Helper()
	results, ok := batch["results"].([]any)
	require.True(t, ok, "%v", batch)
	require.Len(t, results, 1)
	item, ok := results[0].(map[string]any)
	require.True(t, ok)
	require.Nil(t, item["error"], "%v", item["error"])
	return item
}
