package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every rows.Next loop of the store checks rows.Err, so a read that fails
// is never taken for a short or empty result (#129).
func TestEveryRowsLoopChecksItsError(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			var body *ast.BlockStmt
			var name string
			switch fn := node.(type) {
			case *ast.FuncDecl:
				body, name = fn.Body, fn.Name.Name
			case *ast.FuncLit:
				body, name = fn.Body, "function literal"
			default:
				return true
			}
			if body == nil {
				return false
			}
			next, checked := false, false
			ast.Inspect(body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "rows" {
					return true
				}
				switch sel.Sel.Name {
				case "Next":
					next = true
				case "Err":
					checked = true
				}
				return true
			})
			if next && !checked {
				t.Errorf("%s: %s calls rows.Next() without rows.Err()", fset.Position(node.Pos()), name)
			}
			// The body's inspection includes its nested function literals.
			return false
		})
	}
}
