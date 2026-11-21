package app

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// shell is a shell pocketd runs, started in cwd. It is here, built on every
// platform, for the iPad's test as for the Terminal's.
func shell(id, cwd string) proto.Terminal {
	return proto.Terminal{ID: id, Title: filepath.Base(cwd), Cwd: cwd, StartedAt: pocketdtest.Now}
}

// terminalPlugin is MyGo's terminal plugin, which iOS builds leave out
// (ADR-0040).
const terminalPlugin = "github.com/egoist/mygo/plugins/terminal"

// The iOS build leaves MyGo's terminal plugin out, tests and the live test
// included: no file of this package built for iOS imports it, or uses a
// name that only the files iOS leaves out declare, so the package and its
// tests compile for iOS (Codex review of #74, round 1).
func TestTheIOSBuildLeavesTheEmulatorOut(t *testing.T) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "ios", "arm64", true
	ctx.BuildTags = []string{"live"}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	type source struct {
		name string
		file *ast.File
		ios  bool
	}
	fset := token.NewFileSet()
	var files []source
	for _, name := range names {
		ios, err := ctx.MatchFile(".", name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, source{name: name, file: f, ios: ios})
	}
	// receiver is the type a method is declared on, "" for a function.
	receiver := func(fn *ast.FuncDecl) string {
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return ""
		}
		typ := fn.Recv.List[0].Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		if id, ok := typ.(*ast.Ident); ok {
			return id.Name
		}
		return "?"
	}
	// iosTypes are the types the iOS build declares: a method on one of
	// them declared only where iOS leaves it out is missing there.
	iosTypes := map[string]bool{}
	for _, f := range files {
		for _, d := range f.file.Decls {
			if g, ok := d.(*ast.GenDecl); ok && f.ios && g.Tok == token.TYPE {
				for _, s := range g.Specs {
					iosTypes[s.(*ast.TypeSpec).Name.Name] = true
				}
			}
		}
	}
	// declared are the package's names, methods its methods on the types
	// iOS declares, each by whether iOS builds the file declaring them.
	declared := map[bool]map[string]bool{true: {}, false: {}}
	methods := map[bool]map[string]bool{true: {}, false: {}}
	for _, f := range files {
		for _, d := range f.file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				switch r := receiver(d); {
				case r == "":
					declared[f.ios][d.Name.Name] = true
				case iosTypes[r]:
					methods[f.ios][d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						declared[f.ios][s.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range s.Names {
							declared[f.ios][n.Name] = true
						}
					}
				}
			}
		}
	}
	for _, f := range files {
		if !f.ios {
			continue
		}
		for _, imp := range f.file.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == terminalPlugin {
				t.Errorf("%s is built for iOS and imports %s", f.name, terminalPlugin)
			}
		}
		seen := map[string]bool{}
		missing := func(id *ast.Ident, names map[bool]map[string]bool) {
			if names[false][id.Name] && !names[true][id.Name] && !seen[id.Name] {
				seen[id.Name] = true
				t.Errorf("%s is built for iOS and uses %s, which only the files iOS leaves out declare", f.name, id.Name)
			}
		}
		var visit func(n ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				// x.name is a method or a field: only a method can be
				// missing on iOS.
				missing(n.Sel, methods)
				ast.Inspect(n.X, visit)
				return false
			case *ast.Field:
				// A field's or a parameter's own name is no use.
				ast.Inspect(n.Type, visit)
				return false
			case *ast.KeyValueExpr:
				// A literal's field name is no use either.
				if _, field := n.Key.(*ast.Ident); !field {
					ast.Inspect(n.Key, visit)
				}
				ast.Inspect(n.Value, visit)
				return false
			case *ast.Ident:
				missing(n, declared)
			}
			return true
		}
		ast.Inspect(f.file, visit)
	}
	if len(files) == 0 || !methods[false]["termOptions"] {
		t.Fatalf("found no file that iOS leaves out among %v", names)
	}
}

// iPadApp is the test app's window as on an iPad (ADR-0040), drawn at iPad
// portrait size, its emulator's library counted as it loads.
func (x *testApp) iPadApp(t *testing.T, loads *int) {
	t.Helper()
	opts := x.options()
	opts.Platform = "ios"
	opts.Quit = func() { t.Error("the iPad's window quit") }
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	a.loadLib = func() error { *loads++; return nil }
	a.goLoad = func(f func()) { f() }
	x.a = a
	x.tt = ui.NewTesterWith(a.View, 820, 1180, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
}

// The iPad has no Terminal until MyGo's terminal plugin supports iOS: its
// sidebar lists the Inbox and the work board without it, the window never
// goes to the place, and the emulator's library never loads. Its menu has
// no Quit, since iOS ends apps itself (state.HasTerminal,
// state.QuitsItself).
func TestTheIPadWindowHasNoTerminalSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.srv.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	loads := 0
	x.iPadApp(t, &loads)
	x.connect(t)
	x.until(t, func() bool { status, _ := x.a.st.Data.TerminalsStatus(); return status == state.ListLoaded })
	x.hasAll(t, "Inbox", "Work Board")
	if x.tt.HasText("Terminal") {
		t.Errorf("the iPad's window shows the Terminal: %q", x.tt.Texts())
	}
	if places := x.a.sidebarData().Sections[0].Items; slices.Contains(places, state.PlaceKey(sidebarTerminal)) {
		t.Errorf("the iPad's places are %v", places)
	}
	x.snapshot(t, "ipad-inbox")

	x.a.goPlace(sidebarTerminal)
	x.a.attach("s1")
	x.settle()
	if x.a.place == placeTerminal {
		t.Error("the iPad's window went to the Terminal place")
	}
	if loads != 0 || x.a.terms.lib != libNone {
		t.Errorf("the iPad loaded the emulator's library: %d loads, state %v", loads, x.a.terms.lib)
	}

	if x.a.opts.Quit != nil {
		t.Error("the iPad's window keeps the program's Quit")
	}
	if cmd, found := x.a.table.Command("quit"); found && cmd.Enabled != nil {
		if on, _ := cmd.Enabled(); on {
			t.Error("the iPad's menu offers Quit")
		}
	}
}

// The desktops keep the Terminal place and Quit.
func TestTheDesktopKeepsTheTerminalAndQuit(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		x := newTestApp(t)
		opts := x.options()
		opts.Platform = goos
		opts.Quit = func() {}
		a, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		if !slices.Contains(a.places(), sidebarTerminal) {
			t.Errorf("%s: the places are %v", goos, a.places())
		}
		if a.opts.Quit == nil {
			t.Errorf("%s: no Quit", goos)
		}
	}
}

// suspendOn signs in a window on goos, shows session 3 with its panes'
// layout changed, suspends it as the system does, and gives the settings
// saved then.
func suspendOn(t *testing.T, goos string) (*testApp, config.Settings) {
	t.Helper()
	x := newTestApp(t)
	x.seed()
	loads := 0
	x.iPadApp(t, &loads)
	x.a.opts.Platform = goos
	x.connect(t)
	x.a.openSession(3)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded })
	x.a.shell.Panes.Layout.Widths = map[string]float32{"sidebar": 333}
	x.a.Suspend()
	saved, err := x.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return x, saved
}

// iOS may end the app in the background without closing its window, so
// the iPad saves what Close saves as it goes there: the session it shows,
// seen as it is, the panes' layout and the rest of the settings. Unlike
// Close it stops nothing, so the app goes on as it comes back (ADR-0041).
func TestTheIPadKeepsItsWindowWhenSuspended(t *testing.T) {
	x, saved := suspendOn(t, "ios")
	if saved.LastSession != 3 {
		t.Errorf("the session kept is %d, want 3", saved.LastSession)
	}
	if saved.Widths["sidebar"] != 333 {
		t.Errorf("the widths kept are %v", saved.Widths)
	}
	if _, seen := saved.Seen[x.a.st.Conn.Server][3]; !seen {
		t.Errorf("the session shown has no seen mark: %v", saved.Seen)
	}
	if x.a.st.Conn.State != state.Connected || x.a.place != placeSession || x.a.st.Data.OpenID() != 3 {
		t.Errorf("the suspend stopped the window: %v, place %v, session %d", x.a.st.Conn.State, x.a.place, x.a.st.Data.OpenID())
	}
}

// A desktop is suspended as the computer sleeps, and goes on with its
// window open: it saves as the window closes, as before (ADR-0041).
func TestTheDesktopSavesNothingWhenSuspended(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		_, saved := suspendOn(t, goos)
		if saved.LastSession != 0 || len(saved.Widths) != 0 {
			t.Errorf("%s saved the session %d and the widths %v as it was suspended", goos, saved.LastSession, saved.Widths)
		}
	}
}
