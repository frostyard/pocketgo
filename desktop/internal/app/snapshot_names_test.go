package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every test's app runs on a clock that never moves, so a view that ages
// its sessions (NEW, dimmed) draws the same whatever the date: no test
// writes an Options without Now, and testApp.options sets it (Codex review
// of #40, round 1).
func TestEveryTestAppRunsOnTheFakeClock(t *testing.T) {
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
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, isIdent := lit.Type.(*ast.Ident); !isIdent || id.Name != "Options" {
				return true
			}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Now" {
						return true
					}
				}
			}
			t.Errorf("%s: an app's Options without Now runs on the wall clock; use testApp.options", fset.Position(lit.Pos()))
			return true
		})
	}
}

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
