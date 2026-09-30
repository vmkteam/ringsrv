package rpc

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
)

// ToolHelp hands out the cheat sheets as a tool result. The sheets are MCP
// resources too, but a resource is application-controlled: Claude Desktop lists
// it in the attachment menu and gives the model no way to read one. A tool
// result is the one channel that lands in the context in every client.
const ToolHelp = "help"

// ErrCodeSheetUnknown is help's TargetUnknown: no such sheet for this caller. A
// sheet of a target the role does not grant answers the same way, and the list
// in the error names only what the caller may read.
const ErrCodeSheetUnknown = "SheetUnknown"

// The two sheets that are not about a target: one per family of tools.
const (
	sheetCode = "code"
	sheetDB   = "db"
)

// HelpArgs is the argument of help: names the caller already knows from the
// other tools — targets of api_call, or code / db. A list, because the log shows
// sheets fetched one at a time, seconds apart, in runs of two to four.
type HelpArgs struct {
	Names []string `json:"names,omitempty" jsonschema_description:"Targets from api_call, or code / db; up to 6. Empty: the list."`
}

// HelpSheet is one row of the list.
type HelpSheet struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// HelpResult is either the sheets asked for or the list of what there is,
// never both.
type HelpResult struct {
	Env     string      `json:"env"`
	Results []HelpItem  `json:"results,omitempty"`
	Sheets  []HelpSheet `json:"sheets,omitempty"`
	// Budget is where the caller stands. help is exempt from it, and so the
	// one call that still answers when it is spent.
	Budget *Budget `json:"budget,omitempty"`
}

// HelpItem is one sheet, or why there is none under that name. A name nobody
// has a sheet for is that item's error: the other five still answer.
type HelpItem struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Text  string `json:"text,omitempty"`
	// Error names the refusal, with the list of readable sheets attached the
	// way a single call used to answer.
	Error *ToolError `json:"error,omitempty"`
}

var helpInputSchema = mcp.SchemaFor(HelpArgs{})

// helpDescription is static on purpose: the names it would list are already
// in the api_call description, and every byte here is paid on every request.
func (s ToolsService) helpDescription() string {
	return strings.ToUpper(s.env) + " · шпаргалки: пути, примеры и грабли таргетов api_call или инструментов (code, db). names — список имён за один вызов; без names — список шпаргалок. Прочитай перед первым вызовом к таргету."
}

// sheetURI maps a name the caller knows to the resource behind it. A write
// profile shares the sheet of its read twin: youtrack-rw reads
// targets/youtrack.md.
func sheetURI(name string) string {
	switch name {
	case sheetCode, sheetDB:
		return "tools/" + name + ".md"
	default:
		return "targets/" + strings.TrimSuffix(name, "-rw") + ".md"
	}
}

// readSheet returns the text of a sheet by the caller's name for it, if the
// binary ships one. The body and nothing else: a listing asks about a dozen
// sheets and shows the text of none, so it uses sheetDescription instead.
func (s ToolsService) readSheet(name string) (string, bool) {
	if s.docs == nil {
		return "", false
	}
	data, _, err := s.docs.Read(sheetURI(name))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// sheetDescription is the description of one sheet, and whether the binary ships
// it at all. Entry is the half of Read that costs nothing: the library indexed
// the descriptions at startup, so a listing opens no files.
func (s ToolsService) sheetDescription(name string) (string, bool) {
	if s.docs == nil {
		return "", false
	}
	entry, ok := s.docs.Entry(sheetURI(name))
	return entry.Description, ok
}

// sheetText is what a refusal carries: the sheet of the target the call was
// about, empty when there is none.
func (s ToolsService) sheetText(name string) string {
	text, _ := s.readSheet(name)
	return text
}

func hasCodeTool(acc target.Access) bool {
	return slices.ContainsFunc([]string{ToolCodeRead, ToolCodeSearch, ToolCodeHistory, ToolCodeRefs, ToolBlastRadius, ToolWhy}, acc.HasTool)
}

// canRead is the one rule the list and the lookup share: a sheet is readable
// exactly when the thing it describes is callable.
func (s ToolsService) canRead(acc target.Access, name string) bool {
	switch name {
	case sheetCode:
		return hasCodeTool(acc) && len(acc.Repos) > 0
	case sheetDB:
		return (acc.HasTool(ToolDBQuery) || acc.HasTool(ToolDBIntrospect)) && len(acc.Databases) > 0
	default:
		return acc.HasTarget(name)
	}
}

// sheetNames lists what this caller may read, sorted; a write twin is not a
// second row for the same sheet.
func (s ToolsService) sheetNames(acc target.Access) []HelpSheet {
	var out []HelpSheet
	for _, name := range acc.Targets {
		if strings.HasSuffix(name, "-rw") {
			continue
		}
		if desc, ok := s.sheetDescription(name); ok {
			out = append(out, HelpSheet{Name: name, Description: desc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for _, name := range []string{sheetCode, sheetDB} {
		if !s.canRead(acc, name) {
			continue
		}
		if desc, ok := s.sheetDescription(name); ok {
			out = append(out, HelpSheet{Name: name, Description: desc})
		}
	}
	return out
}

// callHelp answers with the sheets asked for, or the list. Audited like every
// tool: which sheets a caller asks for says what they were about to do.
func (s ToolsService) callHelp(ctx context.Context, arguments map[string]any) mcp.ToolCallResult {
	rec, done := s.beginAudit(ctx, ToolHelp)
	defer done()

	acc, terr := s.grant(ctx, rec)
	if terr != nil {
		return errResult(*terr)
	}

	var args HelpArgs
	if decErr := mcp.DecodeArgs(arguments, &args); decErr != nil {
		observe(ToolHelp, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + decErr.Error(), Env: s.env})
	}

	observeBatch(ToolHelp, len(args.Names))
	sheets := s.sheetNames(acc)
	if len(args.Names) == 0 {
		observe(ToolHelp, "-", outcomeOK)
		rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
		return okResultJSON(HelpResult{Env: s.env, Sheets: sheets, Budget: budgetOf(ctx)}, s.env)
	}
	if err := checkBatch(len(args.Names), maxHelpBatch, "names"); err != nil {
		observe(ToolHelp, "-", outcomeBadArgs)
		rec.DenyReason = ErrCodeBadArgs
		return errResult(ToolError{Code: ErrCodeBadArgs, Message: "invalid arguments: " + err.Error(), Env: s.env})
	}
	rec.BatchSize, rec.BatchIndex = len(args.Names), batchCallIndex

	// Served from the binary, so there is nothing to overlap: the list saves
	// round trips through the model, not I/O.
	asked := mcp.Map(args.Names, strings.TrimSpace)
	out := HelpResult{Env: s.env, Results: make([]HelpItem, len(asked)), Budget: budgetOf(ctx)}
	for i, name := range asked {
		out.Results[i] = HelpItem{Index: i, Name: name}

		text, ok := s.readSheet(name)
		if !ok || !s.canRead(acc, name) {
			observe(ToolHelp, "-", outcomeBadArgs)
			out.Results[i].Error = &ToolError{
				Code:    ErrCodeSheetUnknown,
				Message: fmt.Sprintf("no cheat sheet %q for your role", name),
				Sheets:  sheetList(sheets),
			}
			continue
		}
		// Reported under the name the caller wrote, not the file's: youtrack-rw
		// asked, youtrack-rw answered.
		observe(ToolHelp, "-", outcomeOK)
		out.Results[i].Text = text + s.liveFacts(acc, name)
	}
	rec.Path = strings.Join(asked, ",")
	rec.Decision, rec.DenyReason = audit.DecisionAllow, ""
	res := okResultJSON(out, s.env)
	rec.BytesOut = res.Size()
	return res
}

func sheetList(sheets []HelpSheet) []string {
	return mcp.Map(sheets, func(s HelpSheet) string { return s.Name })
}

// liveFacts is what a compiled-in sheet cannot know. The sheets are one per
// driver, so their table names are illustrations — a fine way to teach a dialect
// and a poor way to learn a catalogue. This is generated per call from the
// catalogue and the last probe, and it makes an empty base visible at a glance.
func (s ToolsService) liveFacts(acc target.Access, name string) string {
	if name != sheetDB || s.db == nil || len(acc.Databases) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Базы этого инстанса\n\nШпаргалка выше — про диалект; имена таблиц в её примерах вымышленные. Реальные базы вашей роли:\n\n")
	b.WriteString("| target | driver | база | схемы | таблиц роли видно |\n|---|---|---|---|---|\n")
	for _, db := range acc.Databases {
		d, ok := s.targets.Database(db)
		if !ok {
			continue
		}
		schemas := "—"
		if len(d.Schemas) > 0 {
			schemas = strings.Join(d.Schemas, ", ")
		}
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s | %s |\n", db, d.Driver, d.Database, schemas, s.tableCount(db))
	}
	b.WriteString("\n`0` — роли не видно ни одной таблицы: база пуста либо нет грантов. `db_introspect(target)` без таблицы " +
		"назовёт базу и то, что роли доступно, в полях `database`, `visible` и `note`. `?` — последняя проба не смогла спросить.\n")
	return b.String()
}

// tableCount is the last probe's count as a cell: a number, or "?" when the
// probe never got an answer. An unproven target says so rather than printing a
// stale number as if it were current.
func (s ToolsService) tableCount(name string) string {
	st, ok := s.db.State(name)
	switch {
	case !ok:
		return "?"
	case !st.Proven:
		// Checked before the count, not after: an unproven target always has
		// none, and "не проверен" is the answer, not "?".
		return "не проверен: " + st.Reason
	case st.Tables < 0:
		return "?"
	default:
		return strconv.Itoa(st.Tables)
	}
}

// describeHelp is the tools/list entry, offered whenever there is a tool to read
// about.
func (s ToolsService) describeHelp() mcp.Tool {
	return mcp.Tool{
		Name:        ToolHelp,
		Description: s.helpDescription(),
		InputSchema: helpInputSchema,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Cheat sheets",
			ReadOnlyHint:    new(true),
			DestructiveHint: new(false),
			// Text compiled into the binary; nothing outside it is read.
			OpenWorldHint: new(false),
		},
	}
}
