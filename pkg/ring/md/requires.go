package md

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/adrg/frontmatter"
)

// Families of tools a built-in text can lean on. A prompt names the ones it
// cannot do without in its frontmatter, requires: [api_call]; an instance that
// serves none of them does not offer the prompt — a scenario that starts with
// "look at Sentry" on an instance without a single profile is a dead end.
const (
	FamilyAPICall = "api_call"
	FamilyCode    = "code"
	FamilyDB      = "db"
	FamilyFiles   = "files"
)

// Families is the closed set a requires: may name.
var Families = []string{FamilyAPICall, FamilyCode, FamilyDB, FamilyFiles}

// PromptRequires maps every built-in prompt onto the families it requires,
// read once from the tree. The tree is checked by its test, so an error here is
// a binary built from a broken tree.
var PromptRequires = sync.OnceValues(func() (map[string][]string, error) {
	dir := path.Join(Root, "prompts")
	entries, err := fs.ReadDir(FS, dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := fs.ReadFile(FS, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var meta struct {
			Name     string   `yaml:"name"`
			Requires []string `yaml:"requires"`
		}
		if _, err := frontmatter.Parse(bytes.NewReader(data), &meta); err != nil {
			return nil, fmt.Errorf("md: %s: %w", e.Name(), err)
		}
		out[meta.Name] = meta.Requires
	}
	return out, nil
})
