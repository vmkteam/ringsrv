package codegraph

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Node types GoFileSymbols produces; the engine uses the same words.
const (
	NodeFunction = "function"
	NodeMethod   = "method"
)

// GoFileSymbols reads the functions and methods of a Go file with the standard
// parser instead of asking the engine. The engine's file listing turned out to
// be the wrong source for exactly the question blast_radius asks: its
// signatures carry no receiver, and two methods of one name in one file come
// back as one symbol, so a frame inside the second resolved to nothing.
// go/parser gives the exact span of every declaration, the receiver type, and
// does not need an index — a few milliseconds for a file that is already on
// disk in the worktree.
//
// Only functions and methods are symbols, as before: a package-level constant
// or var is reported as a change no symbol owns, which is how the file-level
// rows keep their meaning.
func GoFileSymbols(dir, path string) ([]Node, error) {
	src, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		return nil, fmt.Errorf("codegraph: read %s: %w", path, err)
	}
	return GoSymbolsFromSource(path, src)
}

// GoSymbolsFromSource is GoFileSymbols over bytes already in hand — the old
// version of a file read straight from the mirror, for which no worktree
// exists and none is needed.
func GoSymbolsFromSource(path string, src []byte) ([]Node, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("codegraph: parse %s: %w", path, err)
	}

	var nodes []Node
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		n := Node{
			Name: fd.Name.Name, Type: NodeFunction, Path: path,
			StartLine: fset.Position(fd.Pos()).Line,
			EndLine:   fset.Position(fd.End()).Line,
		}
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			n.Type = NodeMethod
			n.Receiver = receiverName(fd.Recv.List[0].Type)
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].StartLine < nodes[j].StartLine })
	return nodes, nil
}

// receiverName reads the type name out of a receiver expression: Repo, *Repo,
// List[T] and *List[K, V] all name the type without its decoration.
func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.ParenExpr:
		return receiverName(t.X)
	}
	return ""
}

// isGoFile says whether GoFileSymbols applies.
func isGoFile(path string) bool { return strings.HasSuffix(path, ".go") }
