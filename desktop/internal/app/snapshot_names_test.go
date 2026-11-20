package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// make snapshot runs the view tests whose names match Snapshot: every test
// that writes a snapshot has it in its name, so none is left out (Codex
// review of #25, round 3).
func TestEveryTestThatSnapshotsIsNamedSo(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "snapshot" && !strings.Contains(fn.Name.Name, "Snapshot") {
					t.Errorf("%s writes a snapshot, and make snapshot runs only tests named Snapshot", fn.Name.Name)
				}
				return true
			})
		}
	}
}
