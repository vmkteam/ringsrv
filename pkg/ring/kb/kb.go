// Package kb is the knowledge pack of an instance: a directory of markdown and
// other files served beside the tools — rules of the domain, notes on tables,
// prompts and the instance's own instructions. The built-in cheat sheets teach
// a dialect; the pack says what the data means, which neither the schema nor
// the code records.
//
// The pack belongs to the instance, not to a role: an audience that needs other
// knowledge gets another instance. It is loaded once, at boot, and checked
// whole — a broken link or an SQL example the tools would refuse fails the
// start rather than somebody's question. Serving it is pkg/rpc's business; this
// package indexes and checks.
package kb

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/doc"
)

const (
	// DefaultFoldOver is how many entries a directory may have before the
	// listing of help() shows it as one row.
	DefaultFoldOver = 15
	// InstructionsFile is the voice of the instance, at the root of the pack.
	InstructionsFile = "instructions.md"
	// SchemaDir holds the notes on tables, schema/<source>/<table>.md.
	SchemaDir = "schema"
	// PromptsDir holds the prompts, as in the built-in tree.
	PromptsDir = doc.DefaultPromptsDir
	// VersionFile is written by the image build: the revision of the pack.
	VersionFile = ".version"
	// MaxInstructionsBytes caps the instructions: they go into every
	// initialize, read by the model before its first call.
	MaxInstructionsBytes = 8 << 10
)

// Mode says how the instructions of the pack meet the built-in ones.
type Mode string

const (
	// ModeReplace puts the pack's text in place of the built-in body; the core
	// of the service — the contour, lists, help — stays.
	ModeReplace Mode = "replace"
	// ModeAppend adds the pack's text after the built-in instructions.
	ModeAppend Mode = "append"
)

// Options is what the instance config says about the pack.
type Options struct {
	// Dir is the root of the pack.
	Dir string
	// URIScheme prefixes the URIs of the pack's resources; the links inside
	// the texts are written with it.
	URIScheme string
	// FoldOver is the size above which a directory is one row of the listing.
	FoldOver int
	// Sources maps the source a table note names onto a database target of the
	// catalogue.
	Sources map[string]string
}

// Deps is what the pack is checked against.
type Deps struct {
	// Catalog holds the database targets the notes and SQL examples are about.
	Catalog *target.Catalog
	// ReservedTopics are names help already answers to: a topic may not take
	// the name of a built-in sheet or of a target.
	ReservedTopics []string
	// ReservedPrompts are the names of the built-in prompts.
	ReservedPrompts []string
}

// Instructions is the voice of the instance.
type Instructions struct {
	Mode Mode
	Body string
}

// Topic is one file help can hand out.
type Topic struct {
	// Name is the path from the root of the pack, without .md for markdown.
	Name        string
	URI         string
	Description string
	MimeType    string
	// Source is set on the files under schema/<source>/: such a topic is shown
	// to a role granted the database behind it, and to no other.
	Source string
}

// Pack is a loaded and checked knowledge pack.
type Pack struct {
	opts         Options
	lib          *doc.Library
	topics       []Topic
	byName       map[string]int
	dirs         map[string]bool
	instructions *Instructions
	tables       map[string][]*Table // by database target, lower case
	version      string
}

// Load reads the pack and checks it whole. Every finding is an error, joined
// into one: a pack wrong in three places should cost one fix, not three boots.
func Load(opts Options, d Deps) (*Pack, error) {
	if opts.Dir == "" {
		return nil, errors.New("knowledge pack: Dir is empty")
	}
	if opts.URIScheme == "" {
		return nil, errors.New("knowledge pack: URIScheme is required")
	}
	if opts.FoldOver <= 0 {
		opts.FoldOver = DefaultFoldOver
	}
	st, err := os.Stat(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("knowledge pack: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("knowledge pack: %s is not a directory", opts.Dir)
	}

	root := os.DirFS(opts.Dir)
	lib, err := doc.Load(hideFS{FS: root, hide: InstructionsFile}, ".", doc.Options{URIScheme: opts.URIScheme, PromptsDir: PromptsDir})
	if err != nil {
		return nil, fmt.Errorf("knowledge pack: %w", err)
	}
	p := &Pack{opts: opts, lib: lib, byName: map[string]int{}, dirs: map[string]bool{}, tables: map[string][]*Table{}}
	if v, err := fs.ReadFile(root, VersionFile); err == nil {
		p.version = strings.TrimSpace(string(v))
	}

	c := newChecker(p, root, d)
	if err := c.run(); err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", opts.Dir, err)
	}
	return p, nil
}

// Library is the pack as resources and prompts.
func (p *Pack) Library() *doc.Library { return p.lib }

// URIScheme is the scheme of the pack's resources.
func (p *Pack) URIScheme() string { return p.opts.URIScheme }

// Version is the revision of the pack, empty when the build wrote none.
func (p *Pack) Version() string { return p.version }

// Instructions returns the voice of the instance, if the pack has one.
func (p *Pack) Instructions() (Instructions, bool) {
	if p.instructions == nil {
		return Instructions{}, false
	}
	return *p.instructions, true
}

// Topics returns every topic, sorted by name. The slice is shared.
func (p *Pack) Topics() []Topic { return p.topics }

// Sources maps a source of the table notes onto its database target.
func (p *Pack) Sources() map[string]string { return p.opts.Sources }

// Normalize turns what a caller wrote into a topic name. The texts link with
// URIs and the model copies them as they are, so the scheme, a leading slash,
// a fragment and the .md are all accepted and dropped. A name ending in "/"
// stays a directory.
func (p *Pack) Normalize(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, p.opts.URIScheme)
	name = strings.TrimLeft(name, "/")
	if i := strings.IndexByte(name, '#'); i >= 0 {
		name = name[:i]
	}
	if strings.HasSuffix(name, "/") {
		return name
	}
	if _, ok := p.byName[name]; !ok && strings.EqualFold(pathExt(name), ".md") {
		return name[:len(name)-len(".md")]
	}
	return name
}

// Lookup finds a topic by any of the names Normalize accepts.
func (p *Pack) Lookup(name string) (Topic, bool) {
	i, ok := p.byName[p.Normalize(name)]
	if !ok {
		return Topic{}, false
	}
	return p.topics[i], true
}

// IsDir reports whether a name, normalised, is a directory of the pack.
func (p *Pack) IsDir(name string) bool {
	n := p.Normalize(name)
	return strings.HasSuffix(n, "/") && p.dirs[strings.TrimSuffix(n, "/")]
}

// Dir lists the topics under a directory, at any depth, sorted by name.
func (p *Pack) Dir(name string) []Topic {
	prefix := p.Normalize(name)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var out []Topic
	for _, t := range p.topics {
		if strings.HasPrefix(t.Name, prefix) {
			out = append(out, t)
		}
	}
	return out
}

// Read returns the bytes and the MIME type of a topic.
func (p *Pack) Read(t Topic) ([]byte, string, error) {
	return p.lib.Read(t.URI)
}

// Section is the first segment of a topic's path — what the metrics count by:
// a label per file would have no bound.
func Section(name string) string {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return "."
}

// index builds the topics from the library: every resource outside the prompts
// subtree, named by its path.
func (p *Pack) index() error {
	var errs []error
	for _, r := range p.lib.Resources() {
		rel := strings.TrimPrefix(r.URI, p.opts.URIScheme)
		if strings.HasPrefix(rel, PromptsDir+"/") {
			continue
		}
		name := rel
		if strings.EqualFold(pathExt(rel), ".md") {
			name = rel[:len(rel)-len(".md")]
		}
		if _, dup := p.byName[name]; dup {
			errs = append(errs, fmt.Errorf("%s: topic %q is named twice — a file with and without .md", rel, name))
			continue
		}
		t := Topic{Name: name, URI: r.URI, Description: r.Description, MimeType: r.MimeType}
		if src, ok := schemaSource(rel); ok {
			t.Source = src
		}
		p.byName[name] = len(p.topics)
		p.topics = append(p.topics, t)
		for dir := parentDir(name); dir != ""; dir = parentDir(dir) {
			p.dirs[dir] = true
		}
	}
	sort.Slice(p.topics, func(i, j int) bool { return p.topics[i].Name < p.topics[j].Name })
	for i, t := range p.topics {
		p.byName[t.Name] = i
	}
	return errors.Join(errs...)
}

// schemaSource is the source of a file under schema/<source>/.
func schemaSource(rel string) (string, bool) {
	rest, ok := strings.CutPrefix(rel, SchemaDir+"/")
	if !ok {
		return "", false
	}
	src, _, ok := strings.Cut(rest, "/")
	return src, ok && src != ""
}

func parentDir(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return ""
}

func pathExt(p string) string {
	base := p[strings.LastIndexByte(p, '/')+1:]
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		return base[i:]
	}
	return ""
}

// hideFS is a tree with one file at its root left out: the instructions go into
// initialize, not into resources/list.
type hideFS struct {
	fs.FS
	hide string
}

func (h hideFS) Open(name string) (fs.File, error) {
	if name == h.hide {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return h.FS.Open(name)
}

func (h hideFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(h.FS, name)
	if err != nil || name != "." {
		return entries, err
	}
	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool { return e.Name() == h.hide }), nil
}
