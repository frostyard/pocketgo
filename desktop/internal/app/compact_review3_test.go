package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
)

// The smallest iPhone the app runs on, the SE's, portrait, in DIPs, with
// its status bar and its keyboard.
const (
	seW, seH                = 375, 667
	seTop, seKeyboardHeight = 20, 260
)

// The sign-in card scrolls on a short phone whose keyboard is up: its top
// shows, and Connect can be scrolled to (Codex review of #85, round 3).
func TestThePhonesSignInScrollsAboveTheKeyboard(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	mobile.SetLook(mobile.Cupertino)
	t.Cleanup(func() { mobile.SetLook(mobile.Automatic) })
	opts := x.options()
	opts.Platform = "ios"
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(a.View, seW, seH, ui.TesterOptions{})
	x.keyboardUp(t, seTop, seKeyboardHeight)
	if !a.compact {
		t.Fatal("the SE's window is not compact")
	}
	if title, ok := x.tt.Find("Connect to pocketd"); !ok || title.Y < seTop {
		t.Fatalf("the sign-in's title is cut off under the status bar: %+v, %v", title, ok)
	}
	for range 20 {
		if b, ok := x.tt.AccessByRole(ui.RoleButton, "Connect"); ok && b.Bounds.Y+b.Bounds.H <= seH-seKeyboardHeight {
			return
		}
		x.tt.Scroll(seW/2, seTop+40, 0, 60)
		x.settle()
	}
	b, _ := x.tt.AccessByRole(ui.RoleButton, "Connect")
	t.Errorf("Connect cannot be scrolled above the keyboard: %+v", b.Bounds)
}

// A session the start opens again, before the first frame measured the
// phone's window, does not take the keyboard's focus either (Codex review
// of #85, round 3).
func TestAPhoneRestoresItsSessionWithTheKeyboardDown(t *testing.T) {
	x := newTestApp(t)
	x.seedPhone()
	if err := x.creds.SetToken(x.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	opts := x.options()
	opts.Platform = "ios"
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	a.settings.Server, a.settings.LastSession = x.srv.URL, 11
	a.Start()
	x.loop.RunUntil(t, func() bool { o := a.st.Data.Open(); return o != nil && o.Loaded && a.place == placeSession })
	x.tt = ui.NewTesterWith(a.View, phoneW, phoneH, ui.TesterOptions{})
	x.settle()
	if !a.compact || !x.tt.HasText("Back to Inbox") {
		t.Fatalf("the phone does not show the session restored: compact %v, %q", a.compact, x.tt.Texts())
	}
	if x.tt.Focused("Message") {
		t.Error("the restored session's composer took the focus")
	}
}

// A keyboard reaches the Sessions tab and opens a session from it, with
// no pointer (Codex review of #85, round 3).
func TestAPhonesSessionsTakeTheKeyboard(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tt.Key(ui.Cmd, ui.Key3)
	x.settle()
	if x.a.place != placeSessions {
		t.Fatalf("the Sessions tab's key shows place %v", x.a.place)
	}
	// Tab goes through the top bar's New Session and the filter to the
	// pinned session's row.
	for range 6 {
		if x.tt.Focused("Desk dashboard: weekly metrics") {
			break
		}
		x.tt.Key(0, ui.KeyTab)
		x.settle()
	}
	x.tt.Key(0, ui.KeyEnter)
	x.settle()
	if x.a.place != placeSession || x.a.st.Data.OpenID() != 2 {
		t.Errorf("Tab and Enter open place %v, session %d; want the pinned session 2", x.a.place, x.a.st.Data.OpenID())
	}
	x.tt.Key(ui.Cmd, ui.Key4)
	x.settle()
	if x.a.place != placeMore {
		t.Errorf("More's key shows place %v", x.a.place)
	}
}

// Every sheet opens on a flag of the window's own: mobile.Sheet keeps its
// state on the window's root by the flag's address, so a flag of a value
// made again at each opening would keep each one there for good (Codex
// review of #85, round 3).
func TestEverySheetOpensOnAFlagOfTheWindow(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sheets := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Sheet" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "mobile" {
				return true
			}
			sheets++
			if len(call.Args) < 2 || !windowFlag(call.Args[1]) {
				t.Errorf("%s: a sheet opens on a flag that is not the window's (a.phone or a.shell)", fset.Position(call.Pos()))
			}
			return true
		})
	}
	if sheets == 0 {
		t.Fatal("found no sheet")
	}
}

// windowFlag reports whether e is &a.phone.… or &a.shell.….
func windowFlag(e ast.Expr) bool {
	u, ok := e.(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return false
	}
	sel, ok := u.X.(*ast.SelectorExpr)
	for ok {
		inner, isSel := sel.X.(*ast.SelectorExpr)
		if !isSel {
			break
		}
		if id, isID := inner.X.(*ast.Ident); isID && id.Name == "a" {
			return inner.Sel.Name == "phone" || inner.Sel.Name == "shell"
		}
		sel = inner
	}
	return false
}

// The Activity's newest line shows as the sheet opens, however many lines
// it holds (Codex review of #85, round 3).
func TestThePhonesActivityShowsItsNewestLine(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	for i := range 80 {
		x.a.console.Add(frostui.LevelInfo, "test", "line "+strconv.Itoa(i+1))
	}
	x.tap(t, "More")
	x.tap(t, "Activity")
	last, ok := x.tt.Find("line 80")
	if !ok || last.Y+last.H > phoneH {
		t.Errorf("the newest line is below the screen: %+v, %v", last, ok)
	}
}
