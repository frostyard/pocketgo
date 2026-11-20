package app

import (
	"os"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"
	"github.com/frostyard/frostui/frostuitest"

	"github.com/frostyard/pocketgo/desktop/internal/config"
)

// TestALiveTerminalSnapshot runs a shell on a real pocketd, the one at
// POCKETGO_LIVE_SERVER with the device token POCKETGO_LIVE_TOKEN: it starts
// a terminal from the Terminal place, types a command and reads its output
// back, then ends it. Unset, it skips: unit tests reach no network.
func TestALiveTerminalSnapshot(t *testing.T) {
	server, token := os.Getenv("POCKETGO_LIVE_SERVER"), os.Getenv("POCKETGO_LIVE_TOKEN")
	if server == "" || token == "" {
		t.Skip("POCKETGO_LIVE_SERVER and POCKETGO_LIVE_TOKEN are not set")
	}
	emulator(t)
	frostui.SetIcons(frostui.NewIcons(nil))
	t.Cleanup(func() { frostui.SetIcons(nil) })
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loop := frostuitest.NewLoop()
	a, err := New(Options{Version: "test", Post: loop.Post, Store: store, Creds: config.Credentials{Ring: &memRing{secrets: map[string][]byte{}}, NotFound: errNoSecret, Dir: store.Dir()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x := &testApp{a: a, loop: loop, tt: ui.NewTesterWith(a.View, 1280, 820, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})}
	if err := a.st.Connect(server, token); err != nil {
		t.Fatal(err)
	}
	x.untilLoaded(t)
	x.click(t, "Terminal")
	ok, why := a.st.CanStartTerminal()
	if !ok {
		t.Skipf("no terminal starts: %s", why)
	}
	a.newTerminal()
	x.until(t, func() bool { return a.terms.sel != "" && a.terms.open[a.terms.sel] != nil })
	id := a.terms.sel
	ot := a.terms.open[id]
	x.until(t, func() bool { return strings.TrimSpace(ot.term.Text()) != "" })
	x.tt.Type("echo pocketgo-$((40+2))")
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return strings.Contains(ot.term.Text(), "pocketgo-42") })
	if !a.terms.focused {
		t.Error("the terminal does not have the keys")
	}
	x.snapshot(t, "terminal-live")
	a.endTerminal(id)
	x.until(t, func() bool { _, listed := a.st.Data.Terminal(id); return !listed && a.st.InFlight() == 0 })
}
