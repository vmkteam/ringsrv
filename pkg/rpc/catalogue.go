package rpc

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/kb"

	"github.com/vmkteam/mcpkit"
	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/mcp"
)

// catalogue is the resources and prompts of the instance: the tree that ships
// in the binary and, when the instance has one, its knowledge pack. Each keeps
// its own scheme — the texts of a pack link with theirs — and a request is
// routed by it. The lists are computed once: both trees are fixed at boot.
//
// It implements the mcpkit sources, so resources/* and prompts/* stay the
// library's services; the merge is the only thing written here.
type catalogue struct {
	parts     []cataloguePart
	resources []mcp.ResourceEntry
	prompts   []mcp.PromptEntry
}

type cataloguePart struct {
	lib    *doc.Library
	scheme string
}

// newCatalogue merges the built-in tree and the pack. A prompt name in both is
// refused when the pack loads (kb.Deps.ReservedPrompts); here the built-in one
// would win.
func newCatalogue(builtin *doc.Library, builtinScheme string, pack *kb.Pack) *catalogue {
	c := &catalogue{}
	if builtin != nil {
		c.parts = append(c.parts, cataloguePart{lib: builtin, scheme: builtinScheme})
	}
	if pack != nil {
		c.parts = append(c.parts, cataloguePart{lib: pack.Library(), scheme: pack.URIScheme()})
	}
	seen := map[string]bool{}
	for _, p := range c.parts {
		c.resources = append(c.resources, p.lib.Resources()...)
		for _, pr := range p.lib.Prompts() {
			if !seen[pr.Name] {
				seen[pr.Name] = true
				c.prompts = append(c.prompts, pr)
			}
		}
	}
	sort.Slice(c.prompts, func(i, j int) bool { return c.prompts[i].Name < c.prompts[j].Name })
	return c
}

// Resources lists both trees, the built-in one first.
func (c *catalogue) Resources() []mcp.ResourceEntry { return c.resources }

// Read routes by scheme; a bare path is looked up in each tree in turn.
func (c *catalogue) Read(uri string) ([]byte, string, error) {
	p, ok := c.partOf(uri)
	if !ok {
		return nil, "", fmt.Errorf("unknown resource %q: %w", uri, mcp.ErrInvalidParams)
	}
	return p.lib.Read(uri)
}

// NormalizeURI names a resource the way resources/list did.
func (c *catalogue) NormalizeURI(uri string) string {
	if p, ok := c.partOf(uri); ok {
		return p.lib.NormalizeURI(uri)
	}
	return strings.TrimSpace(uri)
}

// Prompts lists both trees' prompts by name.
func (c *catalogue) Prompts() []mcp.PromptEntry { return c.prompts }

// Render finds the prompt in whichever tree has it.
func (c *catalogue) Render(name string, args map[string]string) (description, text string, err error) {
	for _, p := range c.parts {
		if _, err := p.lib.Prompt(name); err == nil {
			return p.lib.Render(name, args)
		}
	}
	return "", "", fmt.Errorf("unknown prompt %q: %w", name, mcp.ErrInvalidParams)
}

func (c *catalogue) partOf(uri string) (cataloguePart, bool) {
	uri = strings.TrimSpace(uri)
	for _, p := range c.parts {
		if strings.HasPrefix(uri, p.scheme) {
			return p, true
		}
	}
	for _, p := range c.parts {
		if _, ok := p.lib.Entry(uri); ok {
			return p, true
		}
	}
	return cataloguePart{}, false
}

// countKnowledgeReads counts the pack's topics read through resources/read, the
// way help counts its own: which sections the models actually read is the
// question the counter answers, whichever door they came through.
func countKnowledgeReads(pack *kb.Pack) mcpkit.ReadHook {
	return func(_ context.Context, uri string, _ int, err error) {
		if pack == nil || uri == "" || err != nil || !strings.HasPrefix(uri, pack.URIScheme()) {
			return
		}
		if t, ok := pack.Lookup(uri); ok {
			kb.RecordRead(t)
		}
	}
}
