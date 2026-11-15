package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/kb"
	"github.com/vmkteam/ringsrv/pkg/ring/md"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
	"github.com/vmkteam/ringsrv/pkg/rpc"

	"github.com/vmkteam/mcpkit/doc"
	"github.com/vmkteam/mcpkit/mcp"
)

// LoadKnowledge reads and checks the pack the config names, against the
// catalogue and the tree that ships in the binary: a topic may not shadow a
// built-in sheet or a target, a prompt may not shadow a built-in prompt. A nil
// pack and no error mean the config names none.
//
// It is exported for -check-config, which has to run exactly the checks a
// start runs: a pack that passes the check and fails the boot is the failure
// the check exists to prevent.
func LoadKnowledge(cfg Config, cat *target.Catalog, builtin *doc.Library) (*kb.Pack, error) {
	k := cfg.Knowledge
	if k.Dir == "" {
		return nil, nil
	}
	if builtin == nil {
		lib, err := doc.Load(md.FS, md.Root, doc.Options{URIScheme: md.URIScheme})
		if err != nil {
			return nil, fmt.Errorf("load docs: %w", err)
		}
		builtin = lib
	}
	pack, err := kb.Load(kb.Options{Dir: k.Dir, URIScheme: k.URIScheme, FoldOver: k.FoldOver, Sources: k.Sources}, kb.Deps{
		Catalog:         cat,
		ReservedTopics:  rpc.ReservedSheetNames(cat),
		ReservedPrompts: mcp.Map(builtin.Prompts(), func(p mcp.PromptEntry) string { return p.Name }),
	})
	if err != nil {
		return nil, fmt.Errorf("load knowledge: %w", err)
	}
	return pack, nil
}

// loadKnowledge loads the pack at boot. A broken pack is a refused start, as a
// broken catalogue is: the alternative is finding out from an answer.
func (a *App) loadKnowledge(ctx context.Context) error {
	pack, err := LoadKnowledge(a.cfg, a.targets, a.docs)
	if err != nil || pack == nil {
		return err
	}
	a.kb = pack
	kb.RecordLoaded(pack)
	a.Print(ctx, "loaded knowledge", KnowledgeSummary(pack)...)
	return nil
}

// checkAnnotations compares the notes on tables with the tables, once the
// databases are proven: a column the note describes and the table lacks is a
// note a migration outlived. It runs in the background and refuses nothing —
// the base may be slow or down at boot, and the same comparison is in every
// db_introspect answer anyway. What it adds is a number to alert on.
func (a *App) checkAnnotations(ctx context.Context) {
	if a.kb == nil || a.db == nil {
		return
	}
	seen := map[string]bool{}
	for _, tgt := range a.kb.Sources() {
		if seen[tgt] {
			continue
		}
		seen[tgt] = true
		// An unproven base answers every table with the same refusal; one line
		// says it, twelve say it twelve times.
		if st, ok := a.db.State(tgt); !ok || !st.Proven {
			a.Print(ctx, "table notes not checked: database not proven", "target", tgt, "notes", len(a.kb.Tables(tgt)))
			continue
		}
		total := 0
		for _, note := range a.kb.Tables(tgt) {
			schema, err := a.db.Introspect(ctx, tgt, note.Table)
			if err != nil {
				a.Print(ctx, "table note not checked", "target", tgt, "table", note.Table, "err", err.Error())
				continue
			}
			names := make([]string, len(schema.Columns))
			for i, c := range schema.Columns {
				names[i] = c.Name
			}
			if missing := note.Unmatched(names); len(missing) > 0 {
				total += len(missing)
				a.Print(ctx, "table note describes columns the table lacks",
					"target", tgt, "table", note.Table, "columns", strings.Join(missing, ","), "note", note.Topic)
			}
		}
		kb.RecordUnmatched(tgt, total)
	}
}

// KnowledgeSummary is the boot log line of a pack, shared with -check-config.
func KnowledgeSummary(pack *kb.Pack) []any {
	mode := "none"
	if ins, ok := pack.Instructions(); ok {
		mode = string(ins.Mode)
	}
	notes, seen := 0, map[string]bool{}
	for _, tgt := range pack.Sources() {
		if !seen[tgt] {
			seen[tgt] = true
			notes += len(pack.Tables(tgt))
		}
	}
	return []any{
		"scheme", pack.URIScheme(),
		"version", pack.Version(),
		"topics", len(pack.Topics()),
		"prompts", len(pack.Library().Prompts()),
		"annotations", notes,
		"instructions", mode,
	}
}
