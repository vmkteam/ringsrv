package kb

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/adrg/frontmatter"
)

// Fence languages of the SQL examples. A recipe the model is invited to copy
// has to pass the gate that runs it; a counter-example is fenced as rejected
// and has to stay refused. Any other language is not looked at: a fragment — a
// metric expression, a WHERE clause — is fenced as sql-expr or the like, since
// the gate would refuse it for its first word, which says nothing about the
// recipe it belongs to.
const (
	fenceSQL         = "sql"
	fenceSQLRejected = "sql-rejected"
)

var (
	fenceRe = regexp.MustCompile("(?s)```([A-Za-z0-9_-]*)[^\\n]*\\n(.*?)```")
	// placeholderRe finds {{name}} and {{.name}} in a prompt, the two forms
	// mcpkit/doc renders. There is no third: the loader rewrites every bare
	// {{word}}, so {{end}} would be an argument too and a template action
	// cannot be written at all.
	placeholderRe = regexp.MustCompile(`\{\{-?\s*\.?([A-Za-z_][A-Za-z0-9_]*)\s*-?\}\}`)
)

// textFile is a file the links and SQL examples are looked for in.
type textFile struct {
	rel    string
	data   []byte
	md     bool
	driver string
	// note is set on a table note: its examples run against that target.
	note *Table
}

// checker runs the checks of one load and collects every finding.
type checker struct {
	p     *Pack
	root  fs.FS
	d     Deps
	errs  []error
	files map[string]bool
	dirs  map[string]bool
	texts []textFile
}

func newChecker(p *Pack, root fs.FS, d Deps) *checker {
	return &checker{p: p, root: root, d: d, files: map[string]bool{}, dirs: map[string]bool{}}
}

func (c *checker) failf(rel, format string, args ...any) {
	if rel == "" {
		c.errs = append(c.errs, fmt.Errorf(format, args...))
		return
	}
	c.errs = append(c.errs, fmt.Errorf("%s: %s", rel, fmt.Sprintf(format, args...)))
}

func (c *checker) run() error {
	c.checkSources()
	if err := c.walk(); err != nil {
		return err
	}
	if err := c.p.index(); err != nil {
		c.errs = append(c.errs, err)
	}
	if len(c.p.topics) == 0 {
		c.failf("", "no topics: the pack holds nothing help could hand out")
	}
	for _, t := range c.p.topics {
		if slices.Contains(c.d.ReservedTopics, t.Name) {
			c.failf(t.Name, "the name is taken by a built-in cheat sheet or a target")
		}
		// The purpose of a note is its description: the first paragraph of the
		// body, which is what the loader would take instead, is rarely that.
		if note := c.noteOf(t.Name); note != nil && note.Purpose != "" {
			c.p.topics[c.p.byName[t.Name]].Description = note.Purpose
		}
	}
	for _, f := range c.texts {
		c.checkLinks(f)
		if f.md {
			c.checkSQL(f)
		}
	}
	return errors.Join(c.errs...)
}

// checkSources: every mapping names a database the catalogue has.
func (c *checker) checkSources() {
	names := make([]string, 0, len(c.p.opts.Sources))
	for src := range c.p.opts.Sources {
		names = append(names, src)
	}
	sort.Strings(names)
	for _, src := range names {
		tgt := c.p.opts.Sources[src]
		if c.d.Catalog == nil {
			c.failf("", "Sources.%s: no catalogue to check %q against", src, tgt)
			continue
		}
		if _, ok := c.d.Catalog.Database(tgt); !ok {
			c.failf("", "Sources.%s = %q is not a [Databases.*] target of the catalogue", src, tgt)
		}
	}
}

// walk reads every file once: the instructions, the prompts and the notes are
// checked as they come, the rest is kept for the links and the SQL.
func (c *checker) walk() error {
	return fs.WalkDir(c.root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != "." {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != "." {
				c.dirs[p] = true
			}
			return nil
		}
		c.files[p] = true
		data, err := fs.ReadFile(c.root, p)
		if err != nil {
			return err
		}
		c.file(p, data)
		return nil
	})
}

func (c *checker) file(rel string, data []byte) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return
	}
	md := isMarkdown(rel)
	switch {
	case rel == InstructionsFile:
		c.instructions(data)
		return
	case strings.HasPrefix(rel, PromptsDir+"/"):
		if md {
			c.prompt(rel, data)
		}
		return
	}
	f := textFile{rel: rel, data: data, md: md}
	if md {
		var meta struct {
			Driver string `yaml:"driver"`
			Table  string `yaml:"table"`
			Source string `yaml:"source"`
		}
		if _, err := frontmatter.Parse(bytes.NewReader(data), &meta); err != nil {
			c.failf(rel, "frontmatter: %v", err)
			return
		}
		f.driver = meta.Driver
		if strings.HasPrefix(rel, SchemaDir+"/") && (meta.Table != "" || meta.Source != "") {
			f.note = c.table(rel, data)
		}
	}
	c.texts = append(c.texts, f)
}

func (c *checker) instructions(data []byte) {
	var meta struct {
		Mode string `yaml:"mode"`
	}
	body, err := frontmatter.Parse(bytes.NewReader(data), &meta)
	if err != nil {
		c.failf(InstructionsFile, "frontmatter: %v", err)
		return
	}
	mode := Mode(meta.Mode)
	if mode != ModeReplace && mode != ModeAppend {
		c.failf(InstructionsFile, "mode must be %s or %s, got %q — whether the built-in instructions stay is a choice, not a default", ModeReplace, ModeAppend, meta.Mode)
	}
	text := strings.TrimSpace(string(body))
	switch {
	case text == "":
		c.failf(InstructionsFile, "the body is empty")
	case len(text) > MaxInstructionsBytes:
		c.failf(InstructionsFile, "%d bytes, the cap is %d: the text goes into every initialize", len(text), MaxInstructionsBytes)
	}
	c.p.instructions = &Instructions{Mode: mode, Body: text}
}

func (c *checker) prompt(rel string, data []byte) {
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Arguments   []struct {
			Name string `yaml:"name"`
		} `yaml:"arguments"`
	}
	body, err := frontmatter.Parse(bytes.NewReader(data), &meta)
	if err != nil {
		// mcpkit/doc has refused the file already, with the same words.
		return
	}
	if slices.Contains(c.d.ReservedPrompts, meta.Name) {
		c.failf(rel, "prompt %q is the name of a built-in prompt", meta.Name)
	}
	if strings.TrimSpace(meta.Description) == "" {
		c.failf(rel, "prompt %q has no description: prompts/list is how a person picks one", meta.Name)
	}
	declared := make([]string, 0, len(meta.Arguments))
	for _, a := range meta.Arguments {
		declared = append(declared, a.Name)
	}
	seen := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(string(body), -1) {
		name := m[1]
		if seen[name] || slices.Contains(declared, name) {
			continue
		}
		seen[name] = true
		c.failf(rel, "placeholder {{%s}} is not declared in arguments — it would render empty", name)
	}
}

// table reads and checks one note on a table.
func (c *checker) table(rel string, data []byte) *Table {
	var t Table
	if _, err := frontmatter.Parse(bytes.NewReader(data), &t); err != nil {
		c.failf(rel, "frontmatter: %v", err)
		return nil
	}
	if t.Table == "" || t.Source == "" {
		c.failf(rel, "a table note needs both table and source")
		return nil
	}
	if dir, ok := schemaSource(rel); ok && dir != t.Source {
		c.failf(rel, "the file lies under %s/%s/ but names source %q", SchemaDir, dir, t.Source)
	}
	if !dbq.ValidTable(t.Table) {
		c.failf(rel, "table %q is not an identifier with an optional schema", t.Table)
	}
	tgt, ok := c.p.opts.Sources[t.Source]
	if !ok {
		c.failf(rel, "source %q is not in [Knowledge.Sources]", t.Source)
		return nil
	}
	key := strings.ToLower(tgt)
	for _, other := range c.p.tables[key] {
		if strings.EqualFold(other.Table, t.Table) {
			c.failf(rel, "table %s of %s is described twice, also in %s", t.Table, tgt, other.Topic)
			return nil
		}
	}
	t.Target = tgt
	t.Topic = strings.TrimSuffix(rel, pathExt(rel))
	c.p.tables[key] = append(c.p.tables[key], &t)
	return &t
}

func (c *checker) noteOf(topic string) *Table {
	for _, notes := range c.p.tables {
		for _, t := range notes {
			if t.Topic == topic {
				return t
			}
		}
	}
	return nil
}

// linkTrail is what a sentence puts after a link without meaning it.
const linkTrail = ".,;:!?*"

func (c *checker) checkLinks(f textFile) {
	re := regexp.MustCompile(regexp.QuoteMeta(c.p.opts.URIScheme) + "[^\\s)\\]<>{}\"'`|]*")
	seen := map[string]bool{}
	for _, link := range re.FindAllString(string(f.data), -1) {
		rel := strings.TrimPrefix(link, c.p.opts.URIScheme)
		if i := strings.IndexByte(rel, '#'); i >= 0 {
			rel = rel[:i]
		}
		rel = strings.TrimRight(rel, linkTrail)
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		if strings.HasSuffix(rel, "/") {
			if !c.dirs[strings.TrimSuffix(rel, "/")] {
				c.failf(f.rel, "link %s%s names a directory the pack does not have", c.p.opts.URIScheme, rel)
			}
			continue
		}
		if !c.files[rel] {
			c.failf(f.rel, "link %s%s leads nowhere", c.p.opts.URIScheme, rel)
		}
	}
}

func (c *checker) checkSQL(f textFile) {
	n := 0
	for _, m := range fenceRe.FindAllStringSubmatch(string(f.data), -1) {
		lang := strings.ToLower(m[1])
		if lang != fenceSQL && lang != fenceSQLRejected {
			continue
		}
		n++
		driver, err := c.driverFor(f)
		if err != nil {
			c.failf(f.rel, "%v", err)
			return
		}
		err = dbq.CheckSQL(driver, m[2])
		switch {
		case lang == fenceSQL && err != nil:
			c.failf(f.rel, "```sql example %d is refused by the read-only gate (%s): %v — fence it as ```%s if it is meant to be refused", n, driver, err, fenceSQLRejected)
		case lang == fenceSQLRejected && err == nil:
			c.failf(f.rel, "```%s example %d passes the read-only gate (%s) — fence it as ```sql", fenceSQLRejected, n, driver)
		}
	}
}

// driverFor is the dialect a file's examples are written in: its own driver:,
// the database of its note, or the one dialect every mapped database shares.
func (c *checker) driverFor(f textFile) (string, error) {
	if f.driver != "" {
		if f.driver != target.DriverPostgres && f.driver != target.DriverClickHouse {
			return "", fmt.Errorf("driver must be %s or %s, got %q", target.DriverPostgres, target.DriverClickHouse, f.driver)
		}
		return f.driver, nil
	}
	if f.note != nil {
		if d := c.driverOfTarget(f.note.Target); d != "" {
			return d, nil
		}
	}
	if src, ok := schemaSource(f.rel); ok {
		if d := c.driverOfTarget(c.p.opts.Sources[src]); d != "" {
			return d, nil
		}
	}
	drivers := c.knownDrivers()
	if len(drivers) == 1 {
		return drivers[0], nil
	}
	return "", fmt.Errorf("SQL example, but the dialect is ambiguous (%s) — set driver: in the frontmatter", strings.Join(drivers, ", "))
}

func (c *checker) driverOfTarget(name string) string {
	if c.d.Catalog == nil || name == "" {
		return ""
	}
	if d, ok := c.d.Catalog.Database(name); ok {
		return d.Driver
	}
	return ""
}

// knownDrivers are the dialects of the mapped databases, or of every database
// of the catalogue when nothing is mapped.
func (c *checker) knownDrivers() []string {
	set := map[string]bool{}
	for _, tgt := range c.p.opts.Sources {
		if d := c.driverOfTarget(tgt); d != "" {
			set[d] = true
		}
	}
	if len(set) == 0 && c.d.Catalog != nil {
		for _, d := range c.d.Catalog.Databases {
			set[d.Driver] = true
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func isMarkdown(rel string) bool {
	e := strings.ToLower(pathExt(rel))
	return e == ".md" || e == ".markdown"
}
