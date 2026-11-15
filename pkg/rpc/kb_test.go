package rpc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/git"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/kb"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/mcp"
)

const kbScheme = "kb://test/"

// kbFiles is a pack over dbCatalogTOML: a note on a table of each database,
// rules for everyone, and more canned queries than a listing shows whole.
func kbFiles() map[string]string {
	files := map[string]string{
		"instructions.md":   "---\nmode: replace\n---\nАналитика: сначала help([\"playbook/rules\"]).\n",
		"playbook/rules.md": "---\ndescription: Правила расчёта\n---\n# Правила\n\nВыручка — 10% суммы. Таблица: kb://test/schema/app/users.md.\n",
		"schema/app/users.md": "---\ntable: users\nsource: app\npurpose: Пользователи\ncolumns:\n" +
			"  - name: id\n    description: Идентификатор\n  - name: status\n    description: Статус заказа пользователя\n    enum: [new, paid]\n" +
			"  - name: legacy_flag\n    description: Колонка, которой давно нет\n" +
			"notes:\n  - Удалённые не показываются\nrelations:\n  - users.id → orders.user_id\n---\n",
		"schema/events/events.md": "---\ntable: events\nsource: events\npurpose: События\n---\n",
		"queries/README.md":       "---\ndescription: Готовые запросы дашбордов\n---\n",
		"data/logo.png":           "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR",
		"prompts/report.md":       "---\nname: report\ndescription: Отчёт\n---\nСобери отчёт.\n",
	}
	for i := 1; i <= 17; i++ {
		files[fmt.Sprintf("queries/q%02d.md", i)] = fmt.Sprintf("---\ndescription: Запрос %d\n---\n", i)
	}
	return files
}

func writeKB(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	return dir
}

func loadKB(t *testing.T, cat *target.Catalog, files map[string]string) *kb.Pack {
	t.Helper()
	pack, err := kb.Load(kb.Options{
		Dir: writeKB(t, files), URIScheme: kbScheme,
		Sources: map[string]string{"app": "pg", "events": "ch"},
	}, kb.Deps{Catalog: cat, ReservedTopics: ReservedSheetNames(cat)})
	require.NoError(t, err)
	return pack
}

// kbFixture is the database fixture with a knowledge pack.
func kbFixture(t *testing.T) *dbFixture {
	t.Helper()
	return kbFixtureWith(t, kbFiles())
}

// kbFixtureWith is kbFixture over a pack of its own.
func kbFixtureWith(t *testing.T, files map[string]string) *dbFixture {
	t.Helper()
	f := newDBFixture(t, embedlog.Logger{})
	pack := loadKB(t, f.targets, files)
	f.ToolsService = NewToolsService(ToolsDeps{
		Targets: f.targets, DB: f.m, JQTimeout: time.Second, MaxBytes: 1 << 20,
		Docs: testDocs(t), KB: pack, Sessions: ring.NewSessions(time.Hour, nil), Logger: embedlog.Logger{},
	})
	return f
}

// A note names its schema, and a table of another schema is another table: the
// base holds billing.invoices, the pack describes public.invoices, and the
// answer carries no note rather than the wrong one.
func TestDBIntrospect_NoteOfAnotherSchema(t *testing.T) {
	t.Parallel()
	files := kbFiles()
	files["schema/app/invoices.md"] = "---\ntable: public.invoices\nsource: app\npurpose: Счета витрины\n---\n"
	f := kbFixtureWith(t, files)

	res, err := f.Call(ctxWithGroups("analysts"), ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "tables": []any{"billing.invoices", "users"}}))
	require.NoError(t, err)
	batch := decodeText[DBSchemaBatch](t, res)
	require.Len(t, batch.Results, 2)
	require.NotNil(t, batch.Results[0].Schema)
	assert.Nil(t, batch.Results[0].Schema.Doc, "public.invoices is not billing.invoices")
	require.NotNil(t, batch.Results[1].Schema.Doc)
	assert.Equal(t, "schema/app/users", batch.Results[1].Schema.Doc.Topic, "a bare note still finds its table")
}

func helpCall(t *testing.T, s ToolsService, groups string, names ...string) HelpResult {
	t.Helper()
	args := map[string]any{}
	if len(names) > 0 {
		list := make([]any, len(names))
		for i, n := range names {
			list[i] = n
		}
		args["names"] = list
	}
	res, err := s.Call(ctxWithGroups(groups), ToolHelp, args)
	require.NoError(t, err)
	return decodeHelp(t, res)
}

func TestHelp_KnowledgeListing(t *testing.T) {
	t.Parallel()
	f := kbFixture(t)

	out := helpCall(t, f.ToolsService, "pg-readers")
	names := sheetNamesOf(out)
	assert.Contains(t, names, sheetDB, "the built-in sheets stay")
	assert.Contains(t, names, "playbook/rules")
	assert.Contains(t, names, "schema/app/users", "the note on a granted database")
	assert.NotContains(t, names, "schema/events/events", "a note follows its database")
	assert.NotContains(t, names, "queries/q01", "eighteen entries are folded")
	assert.NotContains(t, names, "instructions", "the instructions go into initialize")
	assert.NotContains(t, names, "prompts/report")

	var folded HelpSheet
	for _, sh := range out.Sheets {
		if sh.Name == "queries/" {
			folded = sh
		}
	}
	assert.Equal(t, 18, folded.Count)
	assert.Equal(t, "Готовые запросы дашбордов", folded.Description, "the README speaks for its directory")

	analyst := sheetNamesOf(helpCall(t, f.ToolsService, "analysts"))
	assert.Contains(t, analyst, "schema/events/events")
}

func TestHelp_KnowledgeTopics(t *testing.T) {
	t.Parallel()
	f := kbFixture(t)

	// The texts link with URIs, and the model copies a link as it is.
	for _, name := range []string{"playbook/rules", "playbook/rules.md", kbScheme + "playbook/rules.md"} {
		out := helpCall(t, f.ToolsService, "pg-readers", name)
		require.Len(t, out.Results, 1)
		require.Nil(t, out.Results[0].Error, name)
		assert.Contains(t, out.Results[0].Text, "Выручка — 10% суммы", name)
		assert.Equal(t, name, out.Results[0].Name, "answered under the name asked")
	}

	dir := helpCall(t, f.ToolsService, "pg-readers", "queries/").Results[0]
	require.Nil(t, dir.Error)
	assert.Len(t, dir.Entries, 18)
	assert.Empty(t, dir.Text)
	schema := helpCall(t, f.ToolsService, "pg-readers", "schema/").Results[0]
	require.Nil(t, schema.Error)
	require.Len(t, schema.Entries, 1, "a directory lists what the caller may read")
	assert.Equal(t, "schema/app/users", schema.Entries[0].Name)

	hidden := helpCall(t, f.ToolsService, "pg-readers", "schema/events/events").Results[0]
	require.NotNil(t, hidden.Error)
	assert.Equal(t, ErrCodeSheetUnknown, hidden.Error.Code, "hidden reads as absent")
	assert.Contains(t, hidden.Error.Sheets, "playbook/rules")
	none := helpCall(t, f.ToolsService, "pg-readers", "nosuch/").Results[0]
	require.NotNil(t, none.Error)
	assert.Equal(t, ErrCodeSheetUnknown, none.Error.Code)

	png := helpCall(t, f.ToolsService, "pg-readers", "data/logo.png").Results[0]
	require.NotNil(t, png.Error)
	assert.Equal(t, ErrCodeTopicUnreadable, png.Error.Code)
	assert.Contains(t, png.Error.Message, "resources/read "+kbScheme+"data/logo.png")

	db := helpCall(t, f.ToolsService, "pg-readers", sheetDB).Results[0]
	require.Nil(t, db.Error)
	assert.Contains(t, db.Text, "Базы этого инстанса", "the built-in sheet keeps its name")
}

func TestHelp_DescriptionNamesKnowledge(t *testing.T) {
	t.Parallel()
	f := kbFixture(t)
	list, err := f.List(ctxWithGroups("pg-readers"), "")
	require.NoError(t, err)
	help := list.Tools[len(list.Tools)-1]
	require.Equal(t, ToolHelp, help.Name)
	assert.Contains(t, help.Description, "база знаний инстанса")

	plain, err := newDBFixture(t, embedlog.Logger{}).List(ctxWithGroups("pg-readers"), "")
	require.NoError(t, err)
	assert.NotContains(t, plain.Tools[len(plain.Tools)-1].Description, "база знаний", "an instance without a pack says nothing of one")
	assert.True(t, strings.HasPrefix(help.Description, "DEV ·"))
}

// analystCatalogTOML is the catalogue of an instance for data questions: a
// database and a role that names the two database tools and nothing else.
// help is not a tool a role grants — it follows the others.
const analystCatalogTOML = `
Env = "prod"

[Databases.pg]
Description = "prod · PostgreSQL app"
Driver      = "postgres"
Addr        = "db.example.com:5432"
Database    = "app"
User        = "ringsrv_ro"
Password    = "pw"
Redact      = "off"

[Databases.ch]
Description = "prod · ClickHouse events"
Driver      = "clickhouse"
Addr        = "ch.example.com:9000"
Database    = "events"
User        = "ringsrv_ro"
Password    = "pw"

[Roles.analyst]
Description = "data"
Groups      = ["analysts"]
Tools       = ["db_query", "db_introspect"]
Targets     = ["pg", "ch"]
`

// The analytics instance end to end: databases alone, a role naming the two
// database tools, a pack. help appears without being granted and reads the pack.
func TestHelp_AnalystInstance(t *testing.T) {
	t.Parallel()
	cat, err := target.Parse([]byte(analystCatalogTOML))
	require.NoError(t, err)
	pg, ch := dbqtest.New(dbqtest.Rows(1)), dbqtest.New(dbqtest.Rows(1))
	pg.TableList = []dbq.Table{{Schema: "public", Name: "users", Kind: "table"}}
	ch.TableList = []dbq.Table{{Name: "events", Kind: "MergeTree"}}
	m := dbq.NewManager(dbq.Options{JQTimeout: time.Second, MaxBytes: 1 << 20, RetryAfter: time.Millisecond})
	require.NoError(t, m.Add(dbq.Target{Name: "pg", Driver: target.DriverPostgres, Client: pg, MaxRows: 10, Timeout: time.Second, Redact: "off", Password: "pw", Database: "app", Schemas: []string{"public"}}))
	require.NoError(t, m.Add(dbq.Target{Name: "ch", Driver: target.DriverClickHouse, Client: ch, MaxRows: 10, Timeout: time.Second, Password: "pw", Database: "events"}))
	require.NoError(t, m.Prove(t.Context()))

	s := NewToolsService(ToolsDeps{
		Targets: cat, DB: m, JQTimeout: time.Second, MaxBytes: 1 << 20,
		Docs: testDocs(t), KB: loadKB(t, cat, kbFiles()), Sessions: ring.NewSessions(time.Hour, nil), Logger: embedlog.Logger{},
	})

	list, err := s.List(ctxWithGroups("analysts"), "")
	require.NoError(t, err)
	names := make([]string, len(list.Tools))
	for i, tool := range list.Tools {
		names[i] = tool.Name
	}
	assert.Equal(t, []string{ToolDBQuery, ToolDBIntrospect, ToolHelp}, names)

	out := helpCall(t, s, "analysts", "playbook/rules", "schema/events/events")
	for _, item := range out.Results {
		require.Nil(t, item.Error, item.Name)
		assert.NotEmpty(t, item.Text, item.Name)
	}
}

func dbCatalog(t *testing.T) *target.Catalog {
	t.Helper()
	cat, err := target.Parse([]byte(dbCatalogTOML))
	require.NoError(t, err)
	return cat
}

func TestInstructions_Modes(t *testing.T) {
	t.Parallel()
	cat := dbCatalog(t)
	base := instructions("prod", false, nil)
	assert.Equal(t, fmt.Sprintf(initInstructions, "prod"), base, "without a pack the sheet is what it was")

	files := kbFiles()
	files["instructions.md"] = "---\nmode: replace\n---\nАналитика контура {{env}}: сначала help([\"playbook/rules\"]).\n"
	replace := instructions("prod", true, loadKB(t, cat, files))
	assert.True(t, strings.HasPrefix(replace, "Аналитика контура prod: сначала"), "the pack speaks first, the contour filled in")
	assert.Contains(t, replace, "Окружение инстанса — prod")
	assert.Contains(t, replace, "Инструменты принимают СПИСОК")
	assert.NotContains(t, replace, "api_call", "the built-in body is gone")
	assert.NotContains(t, replace, "{{env}}")
	assert.Contains(t, replace, "поле budget", "the budget is told about in every mode")

	files["instructions.md"] = "---\nmode: append\n---\nЕщё про аналитику.\n"
	appended := instructions("prod", false, loadKB(t, cat, files))
	assert.True(t, strings.HasPrefix(appended, base), "append keeps the sheet word for word")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(appended), "Ещё про аналитику."))

	delete(files, "instructions.md")
	assert.Equal(t, base, instructions("prod", false, loadKB(t, cat, files)), "a pack without instructions changes nothing")

	// The rule of TestInstructions_DoNotLie holds for the core too: every tool
	// it names is a tool.
	for word := range strings.FieldsSeq(strings.NewReplacer(",", " ", ".", " ", "(", " ", ")", " ").Replace(coreInstructions)) {
		if strings.Contains(word, "_") {
			assert.Contains(t, target.KnownTools, word)
		}
	}
}

// resources/* and prompts/* serve both trees, each under its own scheme.
func TestMCP_KnowledgeResourcesAndPrompts(t *testing.T) {
	t.Parallel()
	cat := dbCatalog(t)
	pack := loadKB(t, cat, kbFiles())
	srv := mcpkit.NewServer(NewMCP(MCPDeps{
		Info: mcp.ServerInfo{Name: "ringsrv", Version: "test"}, Docs: testDocs(t), KB: pack,
		Targets: cat, Logger: embedlog.Logger{}, IsDevel: true,
	}), embedlog.Logger{})

	list := call(t, srv, "resources/list", nil, "")
	items, ok := list["resources"].([]any)
	require.True(t, ok)
	uris := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		uri, _ := m["uri"].(string)
		uris = append(uris, uri)
	}
	assert.Contains(t, uris, "ringsrv://targets/prom.md")
	assert.Contains(t, uris, kbScheme+"playbook/rules.md")
	assert.NotContains(t, uris, kbScheme+"instructions.md")

	for _, uri := range []string{kbScheme + "playbook/rules.md", "playbook/rules.md"} {
		read := call(t, srv, "resources/read", map[string]any{"uri": uri}, "")
		contents, _ := read["contents"].([]any)
		require.Len(t, contents, 1, uri)
		first, _ := contents[0].(map[string]any)
		assert.Equal(t, kbScheme+"playbook/rules.md", first["uri"], "a bare path is answered under the pack's URI")
		assert.Contains(t, first["text"], "Выручка", uri)
	}
	read := call(t, srv, "resources/read", map[string]any{"uri": "ringsrv://targets/prom.md"}, "")
	assert.NotEmpty(t, read["contents"])

	prompts := call(t, srv, "prompts/list", nil, "")
	entries, _ := prompts["prompts"].([]any)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	assert.Contains(t, names, "errors")
	assert.Contains(t, names, "report")
	got := call(t, srv, "prompts/get", map[string]any{"name": "report"}, "")
	assert.NotEmpty(t, got["messages"])

	init := call(t, srv, "initialize", map[string]any{"protocolVersion": "2025-06-18"}, "")
	assert.True(t, strings.HasPrefix(init["instructions"].(string), "Аналитика: сначала"), "initialize carries the pack's voice")
}

// The note on a table travels with its schema, beside what the database said.
func TestDBIntrospect_KnowledgeNotes(t *testing.T) {
	t.Parallel()
	f := kbFixture(t)
	ctx := ctxWithGroups("analysts")

	res, err := f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg", "tables": []any{"users", "billing.invoices"}}))
	require.NoError(t, err)
	batch := decodeText[DBSchemaBatch](t, res)
	require.Len(t, batch.Results, 2)
	users := batch.Results[0].Schema
	require.NotNil(t, users)
	require.NotNil(t, users.Doc)
	assert.Equal(t, "schema/app/users", users.Doc.Topic)
	assert.Equal(t, "Пользователи", users.Doc.Purpose)
	assert.Equal(t, []string{"Удалённые не показываются"}, users.Doc.Notes)
	assert.Equal(t, []string{"users.id → orders.user_id"}, users.Doc.Relations)
	assert.Equal(t, []string{"legacy_flag"}, users.Doc.Unmatched, "a described column the table lacks is named")
	assert.Equal(t, "Registered users", users.Comment, "the database's comment is kept")
	status := users.Columns[1]
	assert.Equal(t, []string{"new", "paid"}, status.Enum, "the database's enum")
	require.NotNil(t, status.Doc)
	assert.Equal(t, "Статус заказа пользователя", status.Doc.Description)
	assert.NotNil(t, users.Code, "the pointer to the code stays")
	assert.Nil(t, batch.Results[1].Schema.Doc, "no note, no doc")

	res, err = f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "ch"}))
	require.NoError(t, err)
	list := decodeText[DBTablesResult](t, res)
	require.Len(t, list.Tables, 1)
	assert.Equal(t, "schema/events/events", list.Tables[0].Doc)
	assert.Equal(t, "События", list.Tables[0].Purpose, "the note speaks where the database has no comment")

	res, err = f.Call(ctx, ToolDBIntrospect, introArgs(map[string]any{"target": "pg"}))
	require.NoError(t, err)
	pgList := decodeText[DBTablesResult](t, res)
	require.Len(t, pgList.Tables, 1)
	assert.Equal(t, "schema/app/users", pgList.Tables[0].Doc)
	assert.Empty(t, pgList.Tables[0].Purpose, "the database's comment wins")
}

// analystTools is the analytics instance: databases alone, the analyst role,
// and a pack when one is given.
func analystTools(t *testing.T, pack bool) ToolsService {
	t.Helper()
	cat, err := target.Parse([]byte(analystCatalogTOML))
	require.NoError(t, err)
	m := dbq.NewManager(dbq.Options{JQTimeout: time.Second, MaxBytes: 1 << 20})
	for name, d := range cat.Databases {
		require.NoError(t, m.Add(dbq.Target{Name: name, Driver: d.Driver, Client: dbqtest.New(nil), MaxRows: 1, Timeout: time.Second, Redact: d.Redact, Password: d.Password, Database: d.Database, Schemas: d.Schemas}))
	}
	deps := ToolsDeps{Targets: cat, DB: m, Docs: testDocs(t), Logger: embedlog.Logger{}}
	if pack {
		deps.KB = loadKB(t, cat, kbFiles())
	}
	return NewToolsService(deps)
}

// The help description names what the instance has: an instance of databases
// alone says nothing of api_call, code or files.
func TestHelp_DescriptionFollowsTheInstance(t *testing.T) {
	t.Parallel()

	plain := analystTools(t, false).helpDescription()
	assert.Contains(t, plain, "инструментов (db)")
	for _, absent := range []string{"api_call", "code", "files", "таргет", "база знаний"} {
		assert.NotContains(t, plain, absent)
	}

	withPack := analystTools(t, true).helpDescription()
	assert.Contains(t, withPack, "инструментов (db). Здесь же база знаний инстанса")
	assert.NotContains(t, withPack, "api_call")

	// A catalogue with profiles, repositories and databases names all of it.
	cat := testCatalog(t)
	m := dbq.NewManager(dbq.Options{})
	for name, d := range cat.Databases {
		require.NoError(t, m.Add(dbq.Target{Name: name, Driver: d.Driver, Client: dbqtest.New(nil), MaxRows: 1, Timeout: time.Second}))
	}
	full := NewToolsService(ToolsDeps{
		Targets: cat, DB: m, Docs: testDocs(t),
		Repos: git.New(git.Options{ReposDir: t.TempDir(), WorktreesDir: t.TempDir()}),
	}).helpDescription()
	assert.Contains(t, full, "таргетов api_call или инструментов (code")
	assert.Contains(t, full, "Прочитай перед первым вызовом к таргету.")

	// Without databases wired in, the db family is not the instance's either.
	bare := NewToolsService(ToolsDeps{Targets: cat, Docs: testDocs(t)}).helpDescription()
	assert.NotContains(t, bare, "db")
}

// analystMCP is the analytics instance served over MCP.
func analystMCP(t *testing.T) *mcpkit.Server {
	t.Helper()
	tools := analystTools(t, true)
	return mcpkit.NewServer(NewMCP(MCPDeps{
		Info: mcp.ServerInfo{Name: "ringsrv", Version: "test"}, Docs: testDocs(t), KB: tools.kb,
		Targets: tools.targets, DB: tools.db, Logger: embedlog.Logger{}, IsDevel: true,
	}), embedlog.Logger{})
}

func promptNames(t *testing.T, srv *mcpkit.Server) []string {
	t.Helper()
	res := call(t, srv, "prompts/list", nil, "")
	entries, _ := res["prompts"].([]any)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	return names
}

// The built-in prompts are scenarios over Sentry, metrics and code. An instance
// that has none of it offers the pack's prompts alone.
func TestPrompts_FollowTheInstance(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"report"}, promptNames(t, analystMCP(t)))

	cat := testCatalog(t)
	full := mcpkit.NewServer(NewMCP(MCPDeps{
		Info: mcp.ServerInfo{Name: "ringsrv", Version: "test"}, Docs: testDocs(t), Targets: cat,
		Repos: git.New(git.Options{ReposDir: t.TempDir(), WorktreesDir: t.TempDir()}), Logger: embedlog.Logger{},
	}), embedlog.Logger{})
	assert.Equal(t, []string{"errors", "explain", "incident-triage", "investigate"}, promptNames(t, full))

	noCode := mcpkit.NewServer(NewMCP(MCPDeps{
		Info: mcp.ServerInfo{Name: "ringsrv", Version: "test"}, Docs: testDocs(t), Targets: cat, Logger: embedlog.Logger{},
	}), embedlog.Logger{})
	assert.NotContains(t, promptNames(t, noCode), "explain", "explain requires the code")

	// A hidden prompt is unknown to prompts/get too, not just missing from the list.
	tools := analystTools(t, true)
	c := newCatalogue(testDocs(t), md.URIScheme, tools.kb, tools.surface().builtinFilter(embedlog.Logger{}))
	_, _, err := c.Render("investigate", map[string]string{"symptom": "x"})
	require.ErrorIs(t, err, mcp.ErrInvalidParams)
	_, text, err := c.Render("report", nil)
	require.NoError(t, err)
	assert.Contains(t, text, "Собери отчёт")
}

func resourceURIs(t *testing.T, srv *mcpkit.Server) []string {
	t.Helper()
	res := call(t, srv, "resources/list", nil, "")
	items, _ := res["resources"].([]any)
	uris := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		uri, _ := m["uri"].(string)
		uris = append(uris, uri)
	}
	return uris
}

// resources/list lists the built-in sheets of what the catalogue has: a target
// sheet for each profile, a tool sheet for each family the instance serves.
func TestResources_FollowTheCatalogue(t *testing.T) {
	t.Parallel()
	var builtin []string
	for _, uri := range resourceURIs(t, analystMCP(t)) {
		if strings.HasPrefix(uri, md.URIScheme) {
			builtin = append(builtin, uri)
		}
	}
	assert.Equal(t, []string{md.URIScheme + "tools/db.md"}, builtin, "databases alone: the db sheet and nothing else")

	cat := testCatalog(t)
	m := dbq.NewManager(dbq.Options{})
	for name, d := range cat.Databases {
		require.NoError(t, m.Add(dbq.Target{Name: name, Driver: d.Driver, Client: dbqtest.New(nil), MaxRows: 1, Timeout: time.Second}))
	}
	full := resourceURIs(t, mcpkit.NewServer(NewMCP(MCPDeps{
		Info: mcp.ServerInfo{Name: "ringsrv", Version: "test"}, Docs: testDocs(t), Targets: cat, DB: m,
		Repos: git.New(git.Options{ReposDir: t.TempDir(), WorktreesDir: t.TempDir()}), Logger: embedlog.Logger{},
	}), embedlog.Logger{}))
	for _, name := range []string{"targets/prom.md", "targets/slack.md", "tools/code.md", "tools/db.md", "tools/files.md"} {
		assert.Contains(t, full, md.URIScheme+name)
	}
	assert.NotContains(t, full, md.URIScheme+"targets/mattermost.md", "no such profile in this catalogue")

	// What the list hides, read refuses: the two say the same thing.
	tools := analystTools(t, true)
	c := newCatalogue(testDocs(t), md.URIScheme, tools.kb, tools.surface().builtinFilter(embedlog.Logger{}))
	_, _, err := c.Read(md.URIScheme + "targets/prom.md")
	require.ErrorIs(t, err, mcp.ErrInvalidParams)
	_, _, err = c.Read("targets/prom.md")
	require.ErrorIs(t, err, mcp.ErrInvalidParams, "a bare path too")
	data, _, err := c.Read(md.URIScheme + "tools/db.md")
	require.NoError(t, err)
	assert.NotEmpty(t, data)
}

// db_introspect promises the code pointer only where a database names a
// repository, and the note only where the pack has notes.
func TestDBIntrospect_DescriptionFollowsTheInstance(t *testing.T) {
	t.Parallel()
	plain := analystTools(t, false).dbIntrospectDescription()
	assert.NotContains(t, plain, "указатель на код", "no database names a repository")
	assert.NotContains(t, plain, "doc")
	assert.Contains(t, analystTools(t, true).dbIntrospectDescription(), "описание из базы знаний (doc)")
	assert.Contains(t, kbFixture(t).dbIntrospectDescription(), "указатель на код", "pg names its repositories")
}
