package app

import (
	"os"
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
)

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
