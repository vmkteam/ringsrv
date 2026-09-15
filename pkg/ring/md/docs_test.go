package md_test

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
	"github.com/vmkteam/ringsrv/pkg/ring/target/targettest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/doc"
)

// A cheat sheet with a path the catalogue does not allow is worse than none: the
// model believes it and spends its attempts on a PathNotAllowed. The paths are
// read out of the shipped markdown and run through the profile they describe.
func TestTargetCheatSheetsMatchCatalog(t *testing.T) {
	t.Parallel()

	for _, c := range targettest.Catalogs(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			cat, err := target.Load(c.Path)
			require.NoError(t, err, c.Source)

			entries, err := fs.ReadDir(md.FS, path.Join(md.Root, "targets"))
			require.NoError(t, err)

			for _, e := range entries {
				profileName := strings.TrimSuffix(e.Name(), ".md")
				p, ok := cat.Profile(profileName)
				if !ok {
					continue // this catalogue does not carry that target
				}

				body, err := fs.ReadFile(md.FS, path.Join(md.Root, "targets", e.Name()))
				require.NoError(t, err)

				paths := extractPaths(string(body))
				require.NotEmpty(t, paths, "%s documents no paths at all", e.Name())

				for _, raw := range paths {
					assert.True(t, p.AllowsPath(raw), "%s: %q is documented but the catalogue rejects it", e.Name(), raw)
					// The parameters on a documented path have to pass too: a
					// sheet teaching a QueryParamNotAllowed is the same mistake.
					assert.NoError(t, p.CheckQuery(raw), "%s: %q is documented but its parameters are refused", e.Name(), raw)
				}
			}
		})
	}
}

// A sheet that lists query parameters has to list the catalogue's: a refusal
// carries both, so a sheet naming a parameter the catalogue refuses contradicts
// the payload it travels in. Checked both ways.
func TestCheatSheetsListQueryParams(t *testing.T) {
	t.Parallel()
	cat, err := target.Load(path.Join("..", "..", "..", "cfg", "targets.toml.dist"))
	require.NoError(t, err)

	for name, p := range cat.Profiles {
		if len(p.AllowQueryParams) == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := fs.ReadFile(md.FS, path.Join(md.Root, "targets", name+".md"))
			require.NoError(t, err)
			sheet := string(body)

			listed := map[string]bool{}
			for _, entry := range p.AllowQueryParams {
				param := entry
				if i := strings.IndexAny(entry, "<>"); i >= 0 {
					param = entry[:i]
				}
				param = strings.TrimSpace(param)
				listed[param] = true
				assert.Contains(t, sheet, "`"+param+"`", "the catalogue takes %q, the sheet does not mention it", param)
			}

			sentence := paramSentence.FindString(sheet)
			require.NotEmpty(t, sentence, "the sheet has a sentence starting with «Параметры запроса»")
			for _, m := range paramName.FindAllStringSubmatch(sentence, -1) {
				assert.True(t, listed[m[1]], "the sheet lists %q, the catalogue does not take it", m[1])
			}
		})
	}
}

// paramSentence is the first sentence of the paragraph that enumerates the
// parameters: up to the first `;`, `:` or `.`, which is where the sheet
// moves on to what is refused and why.
var paramSentence = regexp.MustCompile(`Параметры запроса[^;:.]*`)

// paramName is a backticked token shaped like a parameter name, camelCase
// included (`customFields`). Bounds (`15s`) and error codes
// (`QueryParamNotAllowed`) do not match on purpose: they start with a digit
// or a capital.
var paramName = regexp.MustCompile("`([a-z_$][A-Za-z0-9_\\[\\]]*)`")

// Every prompt renders with its arguments and leaves no placeholder behind: a
// leftover {{service}} in the text is an instruction the model will follow
// literally.
func TestPromptsRender(t *testing.T) {
	t.Parallel()
	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)

	require.NotEmpty(t, lib.Prompts())
	for _, p := range lib.Prompts() {
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			args := map[string]string{}
			for _, a := range p.Arguments {
				args[a.Name] = "apisrv"
			}
			text, err := renderPrompt(lib, p.Name, args)
			require.NoError(t, err)

			assert.NotContains(t, text, "{{")

			// A scenario has to name the tools it drives: a walkthrough that
			// says "look at the logs" without naming a tool is a wish.
			named := false
			for _, tool := range target.KnownTools {
				if strings.Contains(text, tool) {
					named = true
					break
				}
			}
			assert.True(t, named, "a scenario has to say which tools it drives")
		})
	}
}

// Every target in the shipped catalogue has a cheat sheet: without one the
// target is unusable from Claude Desktop, where skills are not read.
func TestEveryReadTargetHasCheatSheet(t *testing.T) {
	t.Parallel()
	cat, err := target.Load(path.Join("..", "..", "..", "cfg", "targets.toml.dist"))
	require.NoError(t, err)

	for name, p := range cat.Profiles {
		if p.Write {
			continue // write profiles share the sheet of their read twin
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := fs.ReadFile(md.FS, path.Join(md.Root, "targets", name+".md"))
			assert.NoError(t, err, "target %q has no cheat sheet", name)
		})
	}
}

// placeholders stand in for the parts a cheat sheet writes as <id>: the path
// has to be checked as it would look in a real call.
var placeholders = strings.NewReplacer(
	"<org>", "acme",
	"<id>", "42",
	"<sha>", "abc1234",
	"<uid>", "abc123def",
	"<name>", "job",
	"<job>", "apisrv",
	"<project>", "apisrv",
	"<path>", "internal%2Frpc%2Forder.go",
	"<dir>", "internal",
	"<ID>", "ABC-123",
	// An article is a different kind of id in the same tracker: the A between
	// the project key and the number is what tells the two APIs apart.
	"<AID>", "ABC-A-1",
	"<iid>", "7",
	"<pipeline>", "11689",
	"<job_id>", "76500",
	"<branch>", "devel",
)

// pathLine matches a line that is a request path: inside a fenced block, or in
// the first column of a table.
var pathLine = regexp.MustCompile(`(?m)^\s*(?:\| )?` + "`?" + `(/[^\s|` + "`" + `]*)`)

func extractPaths(md string) []string {
	var out []string
	for _, m := range pathLine.FindAllStringSubmatch(md, -1) {
		p := placeholders.Replace(m[1])
		if strings.Contains(p, "<") {
			continue // an unknown placeholder: not a checkable path
		}
		out = append(out, p)
	}
	return out
}

// The code tools are only usable from Claude Desktop if there is somewhere to
// read about them. A tool shipped without a line in the sheet is
// a tool nobody will call correctly.
func TestCodeToolsHaveACheatSheet(t *testing.T) {
	t.Parallel()
	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)

	body, _, err := lib.Read("ringsrv://tools/code.md")
	require.NoError(t, err, "the code cheat sheet ships with the binary")
	sheet := string(body)

	for _, tool := range []string{"code_read", "code_search", "code_history", "code_refs", "blast_radius", "why"} {
		assert.Contains(t, sheet, tool, "the sheet has to cover %s", tool)
	}
	// The refusals are the part a caller meets first and understands least.
	for _, refusal := range []string{"AmbiguousSymbol", "implementors", "EngineUnavailable", "unmatched_frames"} {
		assert.Contains(t, sheet, refusal, "the sheet has to explain %s", refusal)
	}
}

// sqlBlock is a fenced SQL example with the driver on the info string:
// ```sql postgres … ``` or ```sql clickhouse … ```.
var sqlBlock = regexp.MustCompile("(?s)```sql([^\n]*)\n(.*?)```")

// Every SQL example in the database cheat sheet has to pass the same text check
// the tool applies: an example the tool would refuse teaches the model a call
// that fails. A block without a driver is a mistake in the sheet, not a skip.
func TestDBCheatSheetExamples(t *testing.T) {
	t.Parallel()
	lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
	require.NoError(t, err)

	body, _, err := lib.Read("ringsrv://tools/db.md")
	require.NoError(t, err, "the database cheat sheet ships with the binary")
	sheet := string(body)

	for _, tool := range []string{"db_query", "db_introspect", "FINAL", "READ ONLY", "code_search"} {
		assert.Contains(t, sheet, tool)
	}

	blocks := sqlBlock.FindAllStringSubmatch(sheet, -1)
	require.NotEmpty(t, blocks, "the sheet has SQL examples")
	for _, m := range blocks {
		driver, sql := strings.TrimSpace(m[1]), m[2]
		t.Run(driver+": "+strings.SplitN(strings.TrimSpace(sql), "\n", 2)[0], func(t *testing.T) {
			t.Parallel()
			require.Contains(t, []string{target.DriverPostgres, target.DriverClickHouse}, driver,
				"every SQL block names its driver on the info string")
			assert.NoError(t, dbq.CheckSQL(driver, sql))
		})
	}
}

// renderPrompt is Library.Render with the description dropped: what this test
// checks is the text the model would be handed.
func renderPrompt(lib *doc.Library, name string, args map[string]string) (string, error) {
	_, text, err := lib.Render(name, args)
	return text, err
}
