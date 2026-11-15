package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const scheme = "shop://kb/"

// catalogTOML has one database of each dialect: the SQL checks need to know
// which one an example is written in.
const catalogTOML = `
Env = "prod"

[Databases.events-ch]
Description = "prod · ClickHouse events"
Driver      = "clickhouse"
Addr        = "ch.example.com:9000"
Database    = "events"
User        = "ringsrv_ro"
Password    = "secret"

[Databases.orders-pg]
Description = "prod · PostgreSQL orders"
Driver      = "postgres"
Addr        = "pg.example.com:5432"
Database    = "orders"
User        = "ringsrv_ro"
Password    = "secret"
Redact      = "off"

[Roles.analyst]
Description = "data"
Groups      = ["analysts"]
Tools       = ["db_query", "db_introspect"]
Targets     = ["events-ch", "orders-pg"]
`

// validPack is a pack every check passes. The cases below break it one file
// at a time.
func validPack() map[string]string {
	return map[string]string{
		"instructions.md": "---\nmode: replace\n---\nТы — аналитик магазина. Сначала help().\n",
		"playbook/metrics.md": "---\ndescription: Определения метрик магазина\ndriver: clickhouse\n---\n" +
			"# Метрики\n\nПросмотры — по shop://kb/schema/events/page_views.md, запросы — в shop://kb/queries/.\n\n" +
			"```sql\nSELECT count() FROM page_views WHERE day = today()\n```\n\n" +
			"```sql-rejected\nSELECT 1; SELECT 2\n```\n\n" +
			"Фрагменты не запросы:\n\n```sql-expr\nWHERE day >= today() - 7\n```\n\n```sql-expr\nsum(amount)\n```\n",
		"playbook/glossary.md": "# Словарь\n\nGMV — сумма заказов.\n",
		"schema/events/page_views.md": "---\ntable: page_views\nsource: events\npurpose: Просмотры страниц, одна строка на просмотр\ngrain: просмотр\n" +
			"columns:\n  - name: id\n    description: Идентификатор просмотра\n  - name: kind\n    description: Тип\n    enum: [1, 2, view]\n" +
			"notes:\n  - FINAL обязателен\n---\n\n```sql\nSELECT kind, count() FROM page_views FINAL GROUP BY kind\n```\n",
		"schema/orders/orders.md": "---\ntable: public.orders\nsource: orders\npurpose: Заказы\n---\n",
		"queries/weekly.md":       "---\ndescription: Заказы по неделям\ndriver: postgres\n---\n```sql\nSELECT 1\n```\n",
		"data/regions.csv":        "id,name\n1,North\n",
		"prompts/report.md":       "---\nname: report\ndescription: Недельный отчёт\narguments:\n  - name: week\n    description: Неделя\n---\nСобери отчёт за {{week}}, по {{.week}} целиком.\n",
		".version":                "abc123\n",
	}
}

func writePack(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	return dir
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	cat, err := target.Parse([]byte(catalogTOML))
	require.NoError(t, err)
	return Deps{Catalog: cat, ReservedTopics: []string{"db", "code", "files", "events-ch"}, ReservedPrompts: []string{"investigate"}}
}

func testOptions(dir string) Options {
	return Options{Dir: dir, URIScheme: scheme, Sources: map[string]string{"events": "events-ch", "orders": "orders-pg"}}
}

func loadPack(t *testing.T, files map[string]string) *Pack {
	t.Helper()
	p, err := Load(testOptions(writePack(t, files)), testDeps(t))
	require.NoError(t, err)
	return p
}

func topicNames(ts []Topic) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func TestLoad_Valid(t *testing.T) {
	t.Parallel()
	p := loadPack(t, validPack())

	assert.Equal(t, []string{
		"data/regions.csv", "playbook/glossary", "playbook/metrics",
		"queries/weekly", "schema/events/page_views", "schema/orders/orders",
	}, topicNames(p.Topics()), "instructions, prompts and the version file are not topics")
	assert.Equal(t, "abc123", p.Version())

	ins, ok := p.Instructions()
	require.True(t, ok)
	assert.Equal(t, ModeReplace, ins.Mode)
	assert.Equal(t, "Ты — аналитик магазина. Сначала help().", ins.Body)

	for _, r := range p.Library().Resources() {
		assert.NotEqual(t, scheme+InstructionsFile, r.URI, "the instructions go into initialize, not into resources/list")
	}
	require.Len(t, p.Library().Prompts(), 1)
	assert.Equal(t, "report", p.Library().Prompts()[0].Name)

	views, ok := p.Lookup("schema/events/page_views")
	require.True(t, ok)
	assert.Equal(t, "events", views.Source)
	assert.Equal(t, "Просмотры страниц, одна строка на просмотр", views.Description, "a note is described by its purpose")
	metrics, ok := p.Lookup("playbook/metrics")
	require.True(t, ok)
	assert.Empty(t, metrics.Source)
	assert.Equal(t, "Определения метрик магазина", metrics.Description)
}

// The texts link with URIs and the model copies them as written.
func TestPack_Names(t *testing.T) {
	t.Parallel()
	p := loadPack(t, validPack())

	for _, name := range []string{
		"playbook/metrics", "playbook/metrics.md", scheme + "playbook/metrics.md",
		"/playbook/metrics", " playbook/metrics.md#definitions ",
	} {
		got, ok := p.Lookup(name)
		require.True(t, ok, name)
		assert.Equal(t, "playbook/metrics", got.Name, name)
	}
	csv, ok := p.Lookup("data/regions.csv")
	require.True(t, ok, "a file that is not markdown keeps its extension")
	assert.Equal(t, "text/csv", csv.MimeType)
	_, ok = p.Lookup("playbook/nosuch")
	assert.False(t, ok)

	assert.True(t, p.IsDir("playbook/"))
	assert.True(t, p.IsDir(scheme+"schema/"))
	assert.False(t, p.IsDir("playbook"), "a directory is asked for with a trailing slash")
	assert.False(t, p.IsDir("nosuch/"))
	assert.Equal(t, []string{"schema/events/page_views", "schema/orders/orders"}, topicNames(p.Dir("schema/")))

	data, mime, err := p.Read(csv)
	require.NoError(t, err)
	assert.Equal(t, "text/csv", mime)
	assert.Contains(t, string(data), "North")
	assert.Equal(t, "playbook", Section("playbook/metrics"))
	assert.Equal(t, ".", Section("readme"))
}

func TestPack_Annotations(t *testing.T) {
	t.Parallel()
	p := loadPack(t, validPack())

	views, ok := p.Annotation("events-ch", "page_views")
	require.True(t, ok)
	assert.Equal(t, "events-ch", views.Target)
	assert.Equal(t, "schema/events/page_views", views.Topic)
	assert.Equal(t, "просмотр", views.Grain)
	kind, ok := views.Column("KIND")
	require.True(t, ok, "columns match without regard to case")
	assert.Equal(t, Values{"1", "2", "view"}, kind.Enum, "numbers in an enum are read as text")
	assert.Equal(t, []string{"id"}, views.Unmatched([]string{"kind", "ts"}))

	_, ok = p.Annotation("EVENTS-CH", "events.page_views")
	assert.True(t, ok, "a qualified name finds a bare note when only one fits")
	_, ok = p.Annotation("orders-pg", "orders")
	assert.True(t, ok, "a bare name finds a qualified note when only one fits")
	_, ok = p.Annotation("orders-pg", "public.orders")
	assert.True(t, ok)
	_, ok = p.Annotation("orders-pg", "billing.orders")
	assert.False(t, ok, "two qualified names are compared whole: the note is on public.orders, not on every orders")
	_, ok = p.Annotation("orders-pg", "PUBLIC.Orders")
	assert.True(t, ok, "whole, but without regard to case")
	_, ok = p.Annotation("events-ch", "orders")
	assert.False(t, ok, "notes are per target")
	assert.Len(t, p.Tables("orders-pg"), 1)
}

// Two notes of one bare name in two schemas: a bare question has no single
// answer, and a guess would describe the wrong table.
func TestPack_AnnotationAmbiguous(t *testing.T) {
	t.Parallel()
	files := validPack()
	files["schema/orders/billing_orders.md"] = "---\ntable: billing.orders\nsource: orders\n---\n"
	p := loadPack(t, files)

	_, ok := p.Annotation("orders-pg", "orders")
	assert.False(t, ok)
	got, ok := p.Annotation("orders-pg", "billing.orders")
	require.True(t, ok)
	assert.Equal(t, "billing.orders", got.Table)
}

// A bare note beside a qualified one of the same name: the qualified note
// belongs to its own schema, so a table of any other schema has one candidate
// left — the bare note.
func TestPack_AnnotationBareBesideQualified(t *testing.T) {
	t.Parallel()
	files := validPack()
	files["schema/orders/orders.md"] = "---\ntable: orders\nsource: orders\n---\n"
	files["schema/orders/billing_orders.md"] = "---\ntable: billing.orders\nsource: orders\n---\n"
	p := loadPack(t, files)

	got, ok := p.Annotation("orders-pg", "public.orders")
	require.True(t, ok)
	assert.Equal(t, "orders", got.Table, "billing.orders is not a candidate for public.orders")
	got, ok = p.Annotation("orders-pg", "billing.orders")
	require.True(t, ok)
	assert.Equal(t, "billing.orders", got.Table)
	got, ok = p.Annotation("orders-pg", "orders")
	require.True(t, ok)
	assert.Equal(t, "orders", got.Table, "a bare question meets the bare note whole")
}

func TestLoad_Rejects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit func(files map[string]string)
		opts func(o *Options)
		want string
	}{
		"broken frontmatter": {
			edit: func(f map[string]string) { f["playbook/glossary.md"] = "---\nname: [unclosed\n---\nbody\n" },
			want: "playbook/glossary.md: frontmatter",
		},
		"instructions without a mode": {
			edit: func(f map[string]string) { f["instructions.md"] = "Просто текст.\n" },
			want: "mode must be replace or append",
		},
		"instructions empty": {
			edit: func(f map[string]string) { f["instructions.md"] = "---\nmode: append\n---\n\n" },
			want: "the body is empty",
		},
		"instructions too long": {
			edit: func(f map[string]string) {
				f["instructions.md"] = "---\nmode: append\n---\n" + strings.Repeat("я", MaxInstructionsBytes)
			},
			want: "the cap is 8192",
		},
		"prompt with a built-in name": {
			edit: func(f map[string]string) {
				f["prompts/report.md"] = strings.Replace(validPack()["prompts/report.md"], "name: report", "name: investigate", 1)
			},
			want: `prompt "investigate" is the name of a built-in prompt`,
		},
		"prompt without a description": {
			edit: func(f map[string]string) {
				f["prompts/report.md"] = strings.Replace(validPack()["prompts/report.md"], "description: Недельный отчёт\n", "", 1)
			},
			want: "has no description",
		},
		"undeclared placeholder": {
			edit: func(f map[string]string) { f["prompts/report.md"] += "И за {{month}}.\n" },
			want: "placeholder {{month}} is not declared",
		},
		"note without a source": {
			edit: func(f map[string]string) { f["schema/orders/orders.md"] = "---\ntable: orders\n---\n" },
			want: "needs both table and source",
		},
		"note under another source": {
			edit: func(f map[string]string) { f["schema/events/orders.md"] = "---\ntable: x\nsource: orders\n---\n" },
			want: `lies under schema/events/ but names source "orders"`,
		},
		"source not mapped": {
			edit: func(f map[string]string) { f["schema/stock/items.md"] = "---\ntable: items\nsource: stock\n---\n" },
			want: `source "stock" is not in [Knowledge.Sources]`,
		},
		"mapping onto a missing target": {
			opts: func(o *Options) { o.Sources["stock"] = "stock-pg" },
			want: `Sources.stock = "stock-pg" is not a [Databases.*] target`,
		},
		"table described twice": {
			edit: func(f map[string]string) {
				f["schema/events/views_again.md"] = "---\ntable: PAGE_VIEWS\nsource: events\n---\n"
			},
			want: "is described twice",
		},
		"table that is not a name": {
			edit: func(f map[string]string) { f["schema/events/bad.md"] = "---\ntable: page-views\nsource: events\n---\n" },
			want: `table "page-views" is not an identifier`,
		},
		"link to a missing file": {
			edit: func(f map[string]string) { f["playbook/glossary.md"] += "\nСм. shop://kb/playbook/nosuch.md.\n" },
			want: "link shop://kb/playbook/nosuch.md leads nowhere",
		},
		"link to a missing directory": {
			edit: func(f map[string]string) { f["playbook/glossary.md"] += "\nСм. shop://kb/archive/.\n" },
			want: "names a directory the pack does not have",
		},
		"SQL the gate refuses": {
			edit: func(f map[string]string) {
				f["queries/weekly.md"] = "---\ndescription: x\ndriver: clickhouse\n---\n```sql\nSELECT 1 FORMAT JSON\n```\n"
			},
			want: "example 1 is refused by the read-only gate (clickhouse)",
		},
		"a fragment fenced as a recipe": {
			edit: func(f map[string]string) {
				f["queries/weekly.md"] = "---\ndescription: x\ndriver: postgres\n---\n```sql\nWHERE status = 'paid'\n```\n"
			},
			want: `example 1 is refused by the read-only gate (postgres): a query starts with`,
		},
		"a write fenced as a recipe": {
			edit: func(f map[string]string) {
				f["queries/weekly.md"] = "---\ndescription: x\ndriver: postgres\n---\n```sql\n/* сброс */\nDELETE FROM orders\n```\n"
			},
			want: "example 1 is refused by the read-only gate (postgres)",
		},
		"rejected SQL the gate passes": {
			edit: func(f map[string]string) {
				f["queries/weekly.md"] = "---\ndescription: x\ndriver: clickhouse\n---\n```sql-rejected\nSELECT 1\n```\n"
			},
			want: "passes the read-only gate",
		},
		"ambiguous dialect": {
			edit: func(f map[string]string) {
				f["playbook/glossary.md"] += "\n```sql\nSELECT 1\n```\n"
			},
			want: "the dialect is ambiguous (clickhouse, postgres)",
		},
		"unknown driver": {
			edit: func(f map[string]string) {
				f["playbook/glossary.md"] = "---\ndriver: mysql\n---\n```sql\nSELECT 1\n```\n"
			},
			want: `driver must be postgres or clickhouse, got "mysql"`,
		},
		"topic named like a sheet": {
			edit: func(f map[string]string) { f["db.md"] = "# db\n" },
			want: "db: the name is taken by a built-in cheat sheet or a target",
		},
		"topic named twice": {
			edit: func(f map[string]string) { f["playbook/metrics"] = "plain\n" },
			want: `topic "playbook/metrics" is named twice`,
		},
		"no topics": {
			edit: func(f map[string]string) {
				for name := range f {
					if name != "instructions.md" {
						delete(f, name)
					}
				}
			},
			want: "no topics",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := validPack()
			if tc.edit != nil {
				tc.edit(files)
			}
			opts := testOptions(writePack(t, files))
			if tc.opts != nil {
				tc.opts(&opts)
			}
			_, err := Load(opts, testDeps(t))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// One load names every finding: a pack wrong in three places costs one fix.
func TestLoad_JoinsFindings(t *testing.T) {
	t.Parallel()
	files := validPack()
	files["playbook/glossary.md"] += "\nshop://kb/a.md shop://kb/b.md\n"
	files["prompts/report.md"] += "{{month}}\n"
	_, err := Load(testOptions(writePack(t, files)), testDeps(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop://kb/a.md leads nowhere")
	assert.Contains(t, err.Error(), "shop://kb/b.md leads nowhere")
	assert.Contains(t, err.Error(), "{{month}}")
}

func TestLoad_Options(t *testing.T) {
	t.Parallel()
	deps := testDeps(t)

	_, err := Load(Options{URIScheme: scheme}, deps)
	require.ErrorContains(t, err, "Dir is empty")
	_, err = Load(Options{Dir: t.TempDir()}, deps)
	require.ErrorContains(t, err, "URIScheme is required")
	_, err = Load(Options{Dir: filepath.Join(t.TempDir(), "nosuch"), URIScheme: scheme}, deps)
	require.ErrorContains(t, err, "no such file")

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err = Load(Options{Dir: file, URIScheme: scheme}, deps)
	require.ErrorContains(t, err, "is not a directory")
}

// A directory of FoldOver entries is listed whole; one more and it is a row.
func TestPack_ListingFolds(t *testing.T) {
	t.Parallel()
	files := validPack()
	for i := 1; i <= DefaultFoldOver; i++ {
		files[fmt.Sprintf("archive/a%02d.md", i)] = fmt.Sprintf("# A%d\n\nЗапись %d.\n", i, i)
		files[fmt.Sprintf("big/b%02d.md", i)] = "# B\n\nЗапись.\n"
	}
	files["big/b99.md"] = "# B\n\nЕщё одна.\n"
	files["big/INDEX.md"] = "---\ndescription: Большой раздел\n---\n"
	p := loadPack(t, files)

	rows := p.Listing(nil)
	byName := map[string]Row{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	assert.Contains(t, byName, "archive/a01", "fifteen entries are listed whole")
	assert.NotContains(t, byName, "big/b01")
	assert.Equal(t, Row{Name: "big/", Description: "Большой раздел", Count: DefaultFoldOver + 2}, byName["big/"])
	assert.Contains(t, byName, "playbook/metrics")

	noIndex := loadPack(t, func() map[string]string {
		f := validPack()
		for i := 0; i <= DefaultFoldOver; i++ {
			f[fmt.Sprintf("big/b%02d.md", i)] = "# B\n"
		}
		return f
	}())
	for _, r := range noIndex.Listing(nil) {
		if r.Name == "big/" {
			assert.Equal(t, `записей: 16 — help(["big/"]) покажет их`, r.Description)
		}
	}

	hidden := p.Listing(func(t Topic) bool { return t.Source == "" })
	for _, r := range hidden {
		assert.False(t, strings.HasPrefix(r.Name, "schema/events"), "the filter runs before the fold")
	}
}

func counterValue(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	if m.GetCounter() != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}

// Not parallel: the metrics are process-wide.
func TestMetrics(t *testing.T) {
	files := validPack()
	files[".version"] = "metrics-test\n"
	p := loadPack(t, files)
	RecordLoaded(p)

	m := pm()
	assert.InDelta(t, 2.0, counterValue(t, m.topics.WithLabelValues("playbook")), 0)
	assert.InDelta(t, 2.0, counterValue(t, m.topics.WithLabelValues("schema")), 0)
	assert.InDelta(t, 1.0, counterValue(t, m.info.WithLabelValues("metrics-test")), 0)

	before := counterValue(t, m.reads.WithLabelValues("playbook"))
	topic, ok := p.Lookup("playbook/metrics")
	require.True(t, ok)
	RecordRead(topic)
	assert.InDelta(t, before+1, counterValue(t, m.reads.WithLabelValues("playbook")), 0)

	RecordUnmatched("events-ch", 3)
	assert.InDelta(t, 3.0, counterValue(t, m.unmatched.WithLabelValues("events-ch")), 0)
}
