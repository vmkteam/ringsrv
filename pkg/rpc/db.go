package rpc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/ratelimit"
	"github.com/vmkteam/mcpkit/redact"
)

// The database tools: one read-only SQL statement and the schema it needs,
// against a base named in the catalogue.
const (
	ToolDBQuery      = "db_query"
	ToolDBIntrospect = "db_introspect"
)

// callerPrefix is how the server names itself to a database: application_name is
// "ringsrv:<sub>", or "ringsrv" for a dev instance without a principal.
const callerPrefix = "ringsrv"

// dbSheetHint points at the one document for both drivers. It sits on db_query
// alone: two tools, one sheet, and the budget is paid per request.
const dbSheetHint = "Диалекты и грабли: help(db)"

// DBQueryArgs are the inputs to db_query. One target per call: a batch runs on
// one connection pool with one deadline.
type DBQueryArgs struct {
	Target string `json:"target" jsonschema:"required" jsonschema_description:"Database from the list above."`
	// Queries is short because most of what the log shows is drill-down: the
	// next statement is written after reading the previous answer.
	Queries  []DBQuery `json:"queries" jsonschema:"required" jsonschema_description:"Independent read statements, up to 5. One query is a list of one."`
	MaxBytes int       `json:"max_bytes,omitempty" jsonschema_description:"Budget in bytes for the whole batch; max 262144."`
	Intent   string    `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// DBQuery is one statement of a db_query batch.
type DBQuery struct {
	SQL     string `json:"sql" jsonschema:"required" jsonschema_description:"One read statement (SELECT/WITH/EXPLAIN/SHOW) in the target's dialect."`
	MaxRows int    `json:"max_rows,omitempty" jsonschema_description:"Rows cap; default and ceiling per target."`
	JQ      string `json:"jq,omitempty" jsonschema_description:"Filter over {columns,rows,…}; a dot = raw."`
}

// DBIntrospectArgs are the inputs to db_introspect.
type DBIntrospectArgs struct {
	Target string `json:"target" jsonschema:"required" jsonschema_description:"Database from the db_query list."`
	// An empty list is the table list of the database — the two questions a
	// session actually asks, in one shape.
	Tables []string `json:"tables,omitempty" jsonschema_description:"Names or schema.name, up to 6. Empty: the table list."`
	Intent string   `json:"intent,omitempty" jsonschema_description:"Why, <=120 chars; audited."`
}

// DBQueryBatch is the answer of db_query: one result per statement, in the order
// sent. What is true of the whole call is named once here.
type DBQueryBatch struct {
	Env     string        `json:"env"`
	Driver  string        `json:"driver"`
	TraceID string        `json:"trace_id,omitempty"`
	Results []DBQueryItem `json:"results"`
}

// DBQueryItem is one statement's answer. The rows travel under data because jq
// may have reshaped them; the counts stay outside, so a filtered answer still
// says how much there was.
type DBQueryItem struct {
	Index        int      `json:"index"`
	Truncated    bool     `json:"truncated"`
	RowsReturned int      `json:"rows_returned"`
	ElapsedMS    int64    `json:"elapsed_ms"`
	Data         any      `json:"data,omitempty"`
	Redacted     []string `json:"redacted,omitempty"`
	// Error ends the batch, unlike api_call: a list of statements is usually one
	// line of reasoning, and running the rest after the premise failed costs the
	// database work for an answer nobody will use.
	Error *ToolError `json:"error,omitempty"`
	// Skipped marks a statement the batch never ran, so the model knows it still
	// has to ask.
	Skipped bool `json:"skipped,omitempty"`
}

// DBTablesResult is db_introspect without a table. It names the base whatever it
// found: an empty list alone cannot be told apart from a missing grant or a
// catalogue pointing at the wrong database. Visible and Note are filled only
// when the list came back empty.
type DBTablesResult struct {
	Env      string    `json:"env"`
	Driver   string    `json:"driver"`
	Database string    `json:"database"`
	Schemas  []string  `json:"schemas,omitempty"`
	Tables   []DBTable `json:"tables"`
	Visible  []string  `json:"visible,omitempty"`
	Note     string    `json:"note,omitempty"`
}

// DBTable is one row of the table list.
type DBTable struct {
	Schema       string `json:"schema,omitempty"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	RowsEstimate int64  `json:"rows_estimate"`
	Comment      string `json:"comment,omitempty"`
}

// DBSchemaBatch is db_introspect with tables: one schema per name, in the order
// sent.
type DBSchemaBatch struct {
	Env     string         `json:"env"`
	Driver  string         `json:"driver"`
	Results []DBSchemaItem `json:"results"`
}

// DBSchemaItem is one table's schema, or why there is none. A name that does not
// resolve is that item's error: a typo in the third table should not cost the
// two that answered.
type DBSchemaItem struct {
	Index  int             `json:"index"`
	Table  string          `json:"table"`
	Schema *DBSchemaResult `json:"schema,omitempty"`
	Error  *ToolError      `json:"error,omitempty"`
}

// DBSchemaResult is the schema of one table.
type DBSchemaResult struct {
	Table        string       `json:"table"`
	Comment      string       `json:"comment,omitempty"`
	Columns      []DBColumn   `json:"columns"`
	PrimaryKey   []string     `json:"primary_key,omitempty"`
	Relations    []DBRelation `json:"relations,omitempty"`
	Checks       []string     `json:"checks,omitempty"`
	RowsEstimate int64        `json:"rows_estimate"`
	Engine       string       `json:"engine,omitempty"`
	SortingKey   string       `json:"sorting_key,omitempty"`
	PartitionKey string       `json:"partition_key,omitempty"`
	// Code points at the repository whose db layer describes these tables: the
	// enums live there, not in the database.
	Code *DBCode `json:"code,omitempty"`
}

// DBColumn is one column of a table.
type DBColumn struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Nullable bool     `json:"nullable"`
	Comment  string   `json:"comment,omitempty"`
	Enum     []string `json:"enum,omitempty"`
}

// DBRelation is a foreign key.
type DBRelation struct {
	Column     string `json:"column"`
	References string `json:"references"`
}

// DBCode is where in the code the table's models and enums are. A schema can
// belong to more than one service, so the pointer is a list and the hint is the
// search that covers all of them.
type DBCode struct {
	Repos []DBCodeRepo `json:"repos"`
	Hint  string       `json:"hint"`
}

// DBCodeRepo is one repository and the db layer inside it.
type DBCodeRepo struct {
	Repo  string `json:"repo"`
	Layer string `json:"layer"`
}

var (
	dbQueryInputSchema      = mcp.SchemaFor(DBQueryArgs{})
	dbIntrospectInputSchema = mcp.SchemaFor(DBIntrospectArgs{})
)

// dbToolsVisible is the one condition that both lists the tools and lets a call
// through: the role grants the tool, names at least one database, and the
// instance has the manager to serve it.
func (s ToolsService) dbToolsVisible(acc target.Access, tool string) bool {
	return s.db != nil && acc.HasTool(tool) && len(acc.Databases) > 0
}

// dbQueryDescription lists the databases the caller may query, each with the two
// rules its catalogue entry carries.
func (s ToolsService) dbQueryDescription(acc target.Access) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · readonly SQL к базе из списка; диалект — по базе. queries — до %d запросов к одной базе подряд; после первой ошибки остальные skipped. Один запрос — список из одного.\n\nБазы:\n",
		strings.ToUpper(s.env), maxDBQueryBatch)
	for _, name := range acc.Databases {
		d, ok := s.targets.Database(name)
		if !ok {
			continue
		}
		// A base whose role sees no tables is marked where the model picks a
		// target, and nowhere else: every request pays for this list.
		fmt.Fprintf(&b, "  • %s — %s%s\n", name, s.shortDescription(d.Description), s.emptyMark(name))
	}
	b.WriteString("\nСначала db_introspect(target[, tables]): таблицы, колонки, ключи, где в коде enum'ы. rows — массив массивов в порядке columns; см. truncated. " + dbSheetHint + "\n")
	return b.String()
}

// emptyMark is what is appended to a base the last probe found empty for its
// role. Nothing at all in the ordinary case.
func (s ToolsService) emptyMark(name string) string {
	st, ok := s.db.State(name)
	if !ok || !st.Proven || st.Tables != 0 {
		return ""
	}
	return " [сейчас 0 таблиц]"
}

func (s ToolsService) dbIntrospectDescription() string {
	return fmt.Sprintf("%s · схема из базы: без tables — список таблиц; с tables (до %d имён за вызов) — колонки, ключи, enum'ы, указатель на код. target — из db_query.",
		strings.ToUpper(s.env), maxIntrospectBatch)
}

// dbTarget resolves the role and the target for a database tool, writing the
// refusal into the record the caller will log. The target's redaction rules
// reach the record before the role check: a refused call is logged too, and its
// SQL is no less the model's.
func (s ToolsService) dbTarget(ctx context.Context, tool, name string, rec *auditRecord) (*target.Database, *ToolError) {
	acc, terr := s.grant(ctx, rec)
	if terr != nil {
		return nil, terr
	}
	if !s.dbToolsVisible(acc, tool) {
		observe(tool, "-", outcomeForbidden)
		rec.DenyReason = ErrCodeForbiddenRole
		return nil, &ToolError{Code: ErrCodeForbiddenRole, Message: "your role grants no databases for " + tool, Env: s.env}
	}
	if name == "" {
		observe(tool, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return nil, &ToolError{Code: ErrCodeBadArgs, Message: "target is required", Env: s.env, Targets: acc.Databases}
	}
	d, ok := s.targets.Database(name)
	if !ok {
		// No target, no rules of its own: the record gets every rule, so a
		// literal in the SQL to a mistyped target is no less redacted.
		rec.Redact = redact.ModeOn
		observe(tool, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeTargetUnknown
		return nil, &ToolError{Code: ErrCodeTargetUnknown, Message: fmt.Sprintf("unknown database %q", name), Env: s.env, Targets: acc.Databases}
	}
	rec.Redact, rec.RedactRules = d.Redact, d.RedactRules
	if !acc.HasDatabase(name) {
		observe(tool, name, outcomeForbidden)
		rec.DenyReason = ErrCodeForbiddenRole
		return nil, &ToolError{Code: ErrCodeForbiddenRole, Message: fmt.Sprintf("database %q exists but your role does not grant it", name), Env: s.env, Targets: acc.Databases}
	}
	return d, nil
}

// callDBQuery runs a batch of statements against one database, one after
// another: the pool behind a target holds two connections, so parallel
// statements would queue anyway, and a batch that stops at the first failure has
// to know there was one. What the list buys is round trips through the model.
func (s ToolsService) callDBQuery(ctx context.Context, arguments map[string]any) mcp.ToolCallResult {
	rec, done := s.beginAudit(ctx, ToolDBQuery)
	defer done()
	rec.BatchIndex = batchCallIndex

	var args DBQueryArgs
	if decErr := mcp.DecodeArgs(arguments, &args); decErr != nil {
		observe(ToolDBQuery, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + decErr.Error(), Env: s.env})
	}
	rec.Target, rec.Intent = args.Target, args.Intent
	// Set before the target is resolved: batch_size = 0 on a refusal would read
	// as "a tool without a list" and drop those records out of batch queries.
	rec.BatchSize = len(args.Queries)
	observeBatch(ToolDBQuery, len(args.Queries))
	d, terr := s.dbTarget(ctx, ToolDBQuery, args.Target, rec)
	if terr != nil {
		// A refused batch still names the statements it carried: it is the only
		// place the SQL of a refusal is written.
		s.auditRefusedQueries(ctx, args, rec, terr.Code)
		return errResult(*terr)
	}
	if err := checkBatch(len(args.Queries), maxDBQueryBatch, "queries"); err != nil {
		observe(ToolDBQuery, args.Target, outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	out := DBQueryBatch{Env: s.env, Driver: d.Driver, TraceID: rec.TraceID, Results: make([]DBQueryItem, len(args.Queries))}
	for i, q := range args.Queries {
		out.Results[i] = s.oneDBQuery(ctx, args, q, i, rec)
		if out.Results[i].Error != nil {
			// The tail is not run: continuing one line of reasoning after the
			// premise failed costs the database work nobody will use.
			for j := i + 1; j < len(args.Queries); j++ {
				out.Results[j] = DBQueryItem{Index: j, Skipped: true}
			}
			break
		}
	}

	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	res := okResultJSON(out, s.env)
	rec.BytesOut = res.Size()
	return res
}

// auditRefusedQueries writes one record per statement of a batch that was
// refused before any of them ran.
func (s ToolsService) auditRefusedQueries(ctx context.Context, args DBQueryArgs, call *auditRecord, reason string) {
	for i, q := range args.Queries {
		rec, _, done := s.beginItemAudit(ctx, call, i)
		rec.SQL, rec.JQ = q.SQL, q.JQ
		rec.DenyReason = reason
		done()
	}
}

// oneDBQuery runs one statement of the batch and writes its audit record with
// the SQL whole and redacted.
func (s ToolsService) oneDBQuery(ctx context.Context, args DBQueryArgs, q DBQuery, index int, parent *auditRecord) DBQueryItem {
	rec, started, done := s.beginItemAudit(ctx, parent, index)
	defer done()
	rec.SQL, rec.JQ = q.SQL, q.JQ

	caller := callerPrefix
	if rec.Subject != "" {
		caller += ":" + rec.Subject
	}
	answer, ferr := s.db.Query(ctx, args.Target, dbq.Request{
		SQL: q.SQL, MaxRows: q.MaxRows, JQ: q.JQ, MaxBytes: args.MaxBytes, Caller: caller,
	})
	// Each statement is billed on its own: a batch that ran five did five
	// statements' worth of work, whatever the wall clock says.
	ratelimit.Charge(ctx, time.Since(started))
	if ferr != nil {
		terr := s.dbError(ctx, ToolDBQuery, args.Target, ferr, rec)
		observeDBQuery(args.Target, time.Since(started), outcomeOf(terr.Code))
		return DBQueryItem{Index: index, Error: terr}
	}
	observeDBQuery(args.Target, time.Since(started), outcomeOK)

	observe(ToolDBQuery, args.Target, outcomeOK)
	observeRedactions(args.Target, answer.Redacted)
	rec.Masked = answer.Redacted.Names()
	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	rec.RowsReturned, rec.Truncated, rec.BytesOut = answer.RowsReturned, answer.Truncated, answer.BytesTotal

	item := DBQueryItem{
		Index: index, Truncated: answer.Truncated, RowsReturned: answer.RowsReturned,
		ElapsedMS: answer.Elapsed.Milliseconds(), Data: answer.Data, Redacted: answer.Redacted.Names(),
	}
	if answer.AsText {
		item.Data = answer.Text
	}
	return item
}

// callDBIntrospect answers the schemas. An empty list is the question "what is
// in this database" and answers with the table list; a list of names answers
// with their schemas, one item each. Sequential like db_query, and for the same
// reason: the pool holds two connections.
func (s ToolsService) callDBIntrospect(ctx context.Context, arguments map[string]any) mcp.ToolCallResult {
	rec, done := s.beginAudit(ctx, ToolDBIntrospect)
	defer done()
	rec.BatchIndex = batchCallIndex

	var args DBIntrospectArgs
	if decErr := mcp.DecodeArgs(arguments, &args); decErr != nil {
		observe(ToolDBIntrospect, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + decErr.Error(), Env: s.env})
	}
	rec.Target, rec.Path, rec.Intent = args.Target, strings.Join(args.Tables, ","), args.Intent
	observeBatch(ToolDBIntrospect, len(args.Tables))
	d, terr := s.dbTarget(ctx, ToolDBIntrospect, args.Target, rec)
	if terr != nil {
		return errResult(*terr)
	}
	if err := checkBatchMax(len(args.Tables), maxIntrospectBatch, "tables"); err != nil {
		observe(ToolDBIntrospect, args.Target, outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	rec.BatchSize = len(args.Tables)

	if len(args.Tables) == 0 {
		return s.dbTableList(ctx, d, args.Target, rec)
	}

	out := DBSchemaBatch{Env: s.env, Driver: d.Driver, Results: make([]DBSchemaItem, len(args.Tables))}
	for i, table := range args.Tables {
		out.Results[i] = s.oneDBSchema(ctx, d, args, table, i, rec)
	}

	observe(ToolDBIntrospect, args.Target, outcomeOK)
	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	res := okResultJSON(out, s.env)
	rec.BytesOut = res.Size()
	return res
}

// dbTableList answers db_introspect without names: what this role can see in the
// database at all.
func (s ToolsService) dbTableList(ctx context.Context, d *target.Database, name string, rec *auditRecord) mcp.ToolCallResult {
	tables, ferr := s.db.Tables(ctx, name)
	if ferr != nil {
		// The contour travels on every top-level answer, error or not; inside a
		// batch it is named once, on the batch.
		e := *s.dbError(ctx, ToolDBIntrospect, name, ferr, rec)
		e.Env = s.env
		return errResult(e)
	}
	out := s.newTablesResult(d, tables)
	if len(tables) == 0 {
		s.explainEmpty(ctx, name, &out)
	}
	observe(ToolDBIntrospect, name, outcomeOK)
	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	res := okResultJSON(out, s.env)
	rec.BytesOut = res.Size()
	return res
}

// oneDBSchema introspects one table of the batch and writes its audit record.
func (s ToolsService) oneDBSchema(ctx context.Context, d *target.Database, args DBIntrospectArgs, table string, index int, parent *auditRecord) DBSchemaItem {
	rec, started, done := s.beginItemAudit(ctx, parent, index)
	defer done()
	rec.Path = table

	schema, ferr := s.db.Introspect(ctx, args.Target, table)
	ratelimit.Charge(ctx, time.Since(started))
	if ferr != nil {
		return DBSchemaItem{Index: index, Table: table, Error: s.dbError(ctx, ToolDBIntrospect, args.Target, ferr, rec)}
	}
	observe(ToolDBIntrospect, args.Target, outcomeOK)
	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	out := s.newSchemaResult(d, schema)
	return DBSchemaItem{Index: index, Table: table, Schema: &out}
}

func (s ToolsService) newTablesResult(d *target.Database, tables []dbq.Table) DBTablesResult {
	// Schemas is what the catalogue wrote down — the default included — and
	// empty for a driver without schemas.
	out := DBTablesResult{
		Env: s.env, Driver: d.Driver, Database: d.Database,
		Schemas: d.Schemas, Tables: make([]DBTable, len(tables)),
	}
	for i, t := range tables {
		out.Tables[i] = DBTable{Schema: t.Schema, Name: t.Name, Kind: t.Kind, RowsEstimate: t.RowsEstimate, Comment: t.Comment}
	}
	return out
}

// explainEmpty asks the server what this role can see and turns the answer into
// the sentence an empty list is missing. Three different causes print the same
// {"tables": []}: a base that holds nothing, a role granted nothing in it, and a
// catalogue entry naming a base the role cannot reach.
func (s ToolsService) explainEmpty(ctx context.Context, name string, out *DBTablesResult) {
	d, ferr := s.db.DiagnoseEmpty(ctx, name)
	if ferr != nil {
		out.Note = "no tables here, and the server could not be asked what this role can see: " + ferr.Error()
		return
	}
	out.Visible, out.Note = d.Scope.Visible, d.Note
}

// newSchemaResult maps the domain's schema and attaches the pointer to the code:
// the layer from the catalogue and the search that finds the model.
func (s ToolsService) newSchemaResult(d *target.Database, schema *dbq.Schema) DBSchemaResult {
	out := DBSchemaResult{
		Table: schema.Table, Comment: schema.Comment,
		Columns: make([]DBColumn, len(schema.Columns)), PrimaryKey: schema.PrimaryKey,
		Checks: schema.Checks, RowsEstimate: schema.RowsEstimate,
		Engine: schema.Engine, SortingKey: schema.SortingKey, PartitionKey: schema.PartitionKey,
	}
	for i, col := range schema.Columns {
		out.Columns[i] = DBColumn{Name: col.Name, Type: col.Type, Nullable: col.Nullable, Comment: col.Comment, Enum: col.Enum}
	}
	for _, r := range schema.Relations {
		out.Relations = append(out.Relations, DBRelation{Column: r.Column, References: r.References})
	}
	// Resolved by the schema the table was found in: one base can carry the
	// schemas of several services.
	if refs := d.CodeRefs(schema.Schema); len(refs) > 0 {
		bare := schema.Table
		if _, n, ok := strings.Cut(bare, "."); ok {
			bare = n
		}
		out.Code = &DBCode{Repos: make([]DBCodeRepo, len(refs)), Hint: codeHint(bare, refs)}
		for i, r := range refs {
			out.Code.Repos[i] = DBCodeRepo{Repo: r.Repo, Layer: r.Layer}
		}
	}
	return out
}

// codeHint is the search that finds the models: one call when the layers agree,
// one per repository when they do not — a single path_glob cannot mean two
// directories.
func codeHint(table string, refs []target.CodeRef) string {
	sameLayer := true
	for _, r := range refs[1:] {
		if r.Layer != refs[0].Layer {
			sameLayer = false
			break
		}
	}
	if sameLayer {
		names := make([]string, len(refs))
		for i, r := range refs {
			names[i] = fmt.Sprintf("%q", r.Repo)
		}
		return fmt.Sprintf("enum'ы и модели — code_search(%q, repos: [%s], path_glob: %q)",
			table, strings.Join(names, ", "), refs[0].Layer+"/**")
	}
	calls := make([]string, len(refs))
	for i, r := range refs {
		calls[i] = fmt.Sprintf("code_search(%q, repos: [%q], path_glob: %q)", table, r.Repo, r.Layer+"/**")
	}
	return "enum'ы и модели — " + strings.Join(calls, "; ")
}

// dbError maps a failed call onto the envelope. The server's own text is the
// documentation; unproven says so in as many words.
func (s ToolsService) dbError(ctx context.Context, tool, name string, e *dbq.Error, rec *auditRecord) *ToolError {
	out := ToolError{Message: e.Error(), Keys: e.Keys}
	switch e.Kind {
	case dbq.FailBadArgs:
		out.Code = ErrCodeBadArgs
	case dbq.FailTimeout:
		out.Code = ErrCodeTimeout
		s.Error(ctx, "database timeout", labelTarget, name, "err", rec.LogText(e.Error()))
	case dbq.FailJQ:
		out.Code = ErrCodeJQFailed
	case dbq.FailUnproven:
		out.Code = ErrCodeUpstream
		s.Error(ctx, "database target unproven", labelTarget, name, "err", rec.LogText(e.Error()))
	default:
		// A server that could not be reached, refused the password or answered
		// with an error: the one line the on-call has besides the audit record.
		out.Code = ErrCodeUpstream
		s.Error(ctx, "database failed", labelTarget, name, "err", rec.LogText(e.Error()))
	}
	rec.DenyReason = out.Code
	observe(tool, name, outcomeOf(out.Code))
	return &out
}
