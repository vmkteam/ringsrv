package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/kb"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exampleKnowledge is the shipped example pack against the shipped example
// catalogue: what cfg/local.toml.dist names when its section is uncommented.
func exampleKnowledge(t *testing.T) (Config, *target.Catalog) {
	t.Helper()
	cat, err := target.Load(filepath.Join("..", "..", "cfg", "targets.toml.dist"))
	require.NoError(t, err)
	var cfg Config
	cfg.Knowledge.Dir = filepath.Join("..", "..", "docs", "kb-example")
	cfg.Knowledge.URIScheme = "kb://shop/"
	cfg.Knowledge.Sources = map[string]string{"orders": "orders-pg", "events": "events-ch"}
	return cfg, cat
}

func TestLoadKnowledge_Example(t *testing.T) {
	t.Parallel()
	cfg, cat := exampleKnowledge(t)

	pack, err := LoadKnowledge(cfg, cat, nil)
	require.NoError(t, err)
	require.NotNil(t, pack)
	ins, ok := pack.Instructions()
	require.True(t, ok)
	assert.Equal(t, kb.ModeReplace, ins.Mode)
	_, ok = pack.Annotation("orders-pg", "public.orders")
	assert.True(t, ok)
	_, ok = pack.Annotation("events-ch", "page_views")
	assert.True(t, ok)
	assert.Len(t, pack.Library().Prompts(), 1)

	summary := KnowledgeSummary(pack)
	assert.Contains(t, summary, "annotations")
	assert.Contains(t, summary, 2)
}

func TestLoadKnowledge_NoneConfigured(t *testing.T) {
	t.Parallel()
	pack, err := LoadKnowledge(Config{}, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, pack)
}

// The names a pack may not take are the binary's: its prompts, its sheets and
// the targets of the catalogue.
func TestLoadKnowledge_ReservedNames(t *testing.T) {
	t.Parallel()
	cfg, cat := exampleKnowledge(t)
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS(cfg.Knowledge.Dir)))
	cfg.Knowledge.Dir = dir

	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompts", "investigate.md"),
		[]byte("---\nname: investigate\ndescription: shadow\n---\nbody\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "grafana.md"), []byte("# shadow\n"), 0o600))

	_, err := LoadKnowledge(cfg, cat, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `prompt "investigate" is the name of a built-in prompt`)
	assert.Contains(t, err.Error(), "grafana: the name is taken")
}
