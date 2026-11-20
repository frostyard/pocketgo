//go:build live

package app

import (
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"
	"github.com/frostyard/frostui/frostuitest"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/icons"
	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
)

// TestALiveTerminal runs a shell on a real pocketd, the one at
// POCKETGO_LIVE_SERVER with the device token POCKETGO_LIVE_TOKEN: it starts
// a terminal from the Terminal place, types a command into that shell and
// reads its output back, then ends that shell, whatever else runs there.
// It builds only with the live tag (go test -tags live -run
// TestALiveTerminal ./internal/app), so no plain test run reaches a
// network; unset, the variables skip it.
func TestALiveTerminal(t *testing.T) {
	server, token := os.Getenv("POCKETGO_LIVE_SERVER"), os.Getenv("POCKETGO_LIVE_TOKEN")
	if server == "" || token == "" {
		t.Skip("POCKETGO_LIVE_SERVER and POCKETGO_LIVE_TOKEN are not set")
	}
	emulator(t)
	frostui.SetIcons(frostui.NewIcons(icons.Embedded()))
	t.Cleanup(func() { frostui.SetIcons(nil) })
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loop := frostuitest.NewLoop()
	a, err := New(Options{Version: "test", Post: loop.Post, Store: store, Creds: config.Credentials{Ring: &memRing{secrets: map[string][]byte{}}, NotFound: errNoSecret, Dir: store.Dir()},
		Now: func() time.Time { return pocketdtest.Now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x := &testApp{a: a, loop: loop, tt: ui.NewTesterWith(a.View, 1280, 820, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})}
	if err := a.st.Connect(server, token); err != nil {
		t.Fatal(err)
	}
	x.untilLoaded(t)
	if ok, why := a.st.CanStartTerminal(); !ok {
		t.Skipf("no terminal starts: %s", why)
	}
	x.liveTerminal(t, "echo pocketgo-$((40+2))", "pocketgo-42", nil)
}
