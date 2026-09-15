package codegraph

import (
	"context"
	"sort"
)

// Node returns one symbol with its boundaries and body.
func (c *Client) Node(ctx context.Context, dir, symbol, path string) (*Node, error) {
	args, err := symbolArgs(symbol, path)
	if err != nil {
		return nil, err
	}
	var n Node
	if err := c.call(ctx, dir, "get_ast_node", args, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// symbolArgs builds the arguments both symbol queries take. path is how a
// caller answers the engine's "which of these two did you mean".
func symbolArgs(symbol, path string) (map[string]any, error) {
	if err := ValidateSymbol(symbol); err != nil {
		return nil, err
	}
	args := map[string]any{"symbol_name": symbol}
	if path != "" {
		args["file_path"] = path
	}
	return args, nil
}

// References answers "who calls this symbol". The engine marks by-name matches
// as inferred instead of hiding them, and that mark travels on: in Go two
// methods of different types share a name often enough to matter.
func (c *Client) References(ctx context.Context, dir, symbol, path string, limit int) ([]Reference, error) {
	args, err := symbolArgs(symbol, path)
	if err != nil {
		return nil, err
	}

	var res struct {
		References []Reference `json:"references"`
		Total      int         `json:"total_references"`
	}
	if err := c.call(ctx, dir, "find_references", args, &res); err != nil {
		return nil, err
	}
	if limit > 0 && len(res.References) > limit {
		res.References = res.References[:limit]
	}
	return res.References, nil
}

// FileSymbols returns the symbols of one file, sorted by where they start —
// what a stack frame is resolved against.
//
// include_dead is not optional: without it the engine reports only symbols
// something calls, and a release that broke an entry point would come back as a
// file with no symbols in it.
func (c *Client) FileSymbols(ctx context.Context, dir, path string) ([]Node, error) {
	// Go is read with go/parser: exact spans, receivers, and both String methods
	// of a file instead of one. The engine answers for everything else, and for
	// a Go file it cannot parse.
	if isGoFile(path) {
		if nodes, err := GoFileSymbols(dir, path); err == nil {
			return nodes, nil
		}
	}
	// The two lists carry the same fields under different names, which is why
	// the file is read from either spelling.
	type entry struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		Path      string `json:"file_path"`
		AltPath   string `json:"file"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Signature string `json:"signature"`
	}
	var res struct {
		Active   []entry `json:"active_exports"`
		DeadCode struct {
			Results []entry `json:"results"`
		} `json:"dead_code"`
	}

	// dead_min_lines is zero on purpose: the engine's default of five hides
	// every shorter uncalled symbol under "inactive_summary" without line
	// numbers, and a frame in one of them then resolves to nothing.
	args := map[string]any{"path": path, "include_dead": true, "dead_min_lines": 0}
	if err := c.call(ctx, dir, "module_overview", args, &res); err != nil {
		return nil, err
	}

	all := append(res.Active, res.DeadCode.Results...) //nolint:gocritic // two lists of the same thing
	nodes := make([]Node, 0, len(all))
	for _, e := range all {
		p := e.Path
		if p == "" {
			p = e.AltPath
		}
		nodes = append(nodes, Node{
			Name: e.Name, Type: e.Type, Path: p,
			StartLine: e.StartLine, EndLine: e.EndLine, Signature: e.Signature,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].StartLine < nodes[j].StartLine })
	return nodes, nil
}

// SymbolAt returns the symbol containing the line, or nil when the line falls
// between symbols. Nil is an answer: claiming the previous function owns a line
// it does not is how a blast radius acquires a symbol that was never in the
// frame.
//
// nodes must be sorted by StartLine, as FileSymbols returns them. The search is
// binary because a diff asks this once per changed line.
func SymbolAt(nodes []Node, line int) *Node {
	// The last symbol that starts at or before the line; anything later cannot
	// contain it.
	i := sort.Search(len(nodes), func(i int) bool { return nodes[i].StartLine > line }) - 1
	for ; i >= 0; i-- {
		if nodes[i].Type == "module" {
			continue // the file-wide node contains every line and answers nothing
		}
		if line <= nodes[i].EndLine {
			return &nodes[i]
		}
		// Symbols do not nest here, so once one ends before the line the earlier
		// ones do too — except the module node, which is why the loop steps back.
		if nodes[i].StartLine < line && nodes[i].EndLine < line {
			break
		}
	}
	return nil
}
