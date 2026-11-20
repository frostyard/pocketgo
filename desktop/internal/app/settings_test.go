package app

import (
	"os"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/config"
)

// zoomTester draws x's app and records the zoom each frame draws at.
func zoomTester(x *testApp, shown *float32) {
	x.tt = ui.NewTesterWith(func(c *ui.Context) { x.a.View(c); *shown = c.Zoom() }, 1280, 820,
		ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
}

// The zoom keys zoom the window and save the zoom; Settings shows it and
// sets it; the commands stop at the ends; and the zoom saved holds from
// the next start, the sign-in screen included (the owner's usability
// list, item 4).
func TestZoomZoomsTheWindowAndIsSavedSnapshot(t *testing.T) {
	x := newTestApp(t)
	var shown float32
	zoomTester(x, &shown)
	x.connect(t)
	x.untilLoaded(t)
	if shown != 1 {
		t.Fatalf("the window shows at %v with no zoom saved, want 1", shown)
	}
	if ok, _ := x.a.table.Enabled("zoom-reset"); ok {
		t.Error("Actual Size is offered at 100%")
	}
	x.loop.Post(func() { x.a.table.Run("zoom-in"); x.a.table.Run("zoom-in") })
	x.until(t, func() bool { return x.a.zoom() == 1.25 })
	if shown != 1.25 {
		t.Fatalf("the window shows at %v, want 1.25", shown)
	}
	if saved, err := x.store.Load(); err != nil || saved.Zoom != 1.25 {
		t.Fatalf("saved zoom %v (%v), want 1.25", saved.Zoom, err)
	}
	x.hasAll(t, "Inbox")
	x.snapshot(t, "zoom-125-inbox")

	x.loop.Post(func() { x.a.table.Run("settings") })
	x.until(t, func() bool { return x.a.prefs.layout.Open })
	x.hasAll(t, "Settings", "Appearance", "Zoom", "125%")
	x.snapshot(t, "zoom-125-settings")
	x.click(t, "125%")
	x.click(t, "150%")
	x.until(t, func() bool { return x.a.zoom() == 1.5 })
	if saved, err := x.store.Load(); err != nil || saved.Zoom != 1.5 {
		t.Fatalf("Settings saved zoom %v (%v), want 1.5", saved.Zoom, err)
	}

	x.loop.Post(func() { x.a.prefs.layout.Open = false; x.a.table.Run("zoom-reset") })
	x.until(t, func() bool { return x.a.zoom() == 1 })
	if shown != 1 {
		t.Fatalf("Actual Size shows at %v", shown)
	}
	if saved, err := x.store.Load(); err != nil || saved.Zoom != 0 {
		t.Fatalf("100%% saved as %v (%v), want none", saved.Zoom, err)
	}
	x.loop.Post(func() { x.a.setZoom(3) })
	x.until(t, func() bool { return x.a.zoom() == 3 })
	if ok, _ := x.a.table.Enabled("zoom-in"); ok {
		t.Error("Zoom In is offered at the largest zoom")
	}
	x.loop.Post(func() { x.a.setZoom(0.5) })
	x.until(t, func() bool { return x.a.zoom() == 0.5 })
	if ok, _ := x.a.table.Enabled("zoom-out"); ok {
		t.Error("Zoom Out is offered at the smallest zoom")
	}

	// The next start shows at the zoom saved, before it signs in.
	if err := x.store.Save(config.Settings{Zoom: 1.75}); err != nil {
		t.Fatal(err)
	}
	x.a = x.newApp(t)
	zoomTester(x, &shown)
	x.settle()
	if shown != 1.75 {
		t.Errorf("the sign-in screen shows at %v, want the zoom saved, 1.75", shown)
	}
}

// A zoom too large for the window shows at the largest level that keeps
// the session list beside the session, and Settings fits the window and
// says so; the zoom keys work before signing in, and in Settings, once a
// press (Codex review of #39).
func TestZoomFitsAndItsKeysWorkEverywhereSnapshot(t *testing.T) {
	x := newTestApp(t)
	var shown float32
	zoomTester(x, &shown)
	x.connect(t)
	x.untilLoaded(t)
	x.loop.Post(func() { x.a.setZoom(2) })
	x.until(t, func() bool { return x.a.zoom() == 2 })
	if shown != 1.75 {
		t.Fatalf("200%% in a 1280-wide window shows at %v, want 1.75", shown)
	}
	x.hasAll(t, "No sessions yet")
	x.loop.Post(func() { x.a.table.Run("settings") })
	x.until(t, func() bool { return x.a.prefs.layout.Open })
	x.hasAll(t, "200%", "Shown at 175%")
	x.snapshot(t, "zoom-200-capped-settings")
	// The Zoom selector and the close button lie inside the window.
	for _, label := range []string{"200%", "Close"} {
		r, ok := x.tt.Find(label)
		if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > 1280 || r.Y+r.H > 820 {
			t.Errorf("Settings' %q lies at %+v (found %v), outside the 1280 by 820 window", label, r, ok)
		}
	}

	// In Settings, a zoom key zooms once.
	x.loop.Post(func() { x.a.setZoom(1) })
	x.until(t, func() bool { return x.a.zoom() == 1 })
	x.tt.Key(ui.Cmd, ui.KeyEqual)
	x.settle()
	if z := x.a.zoom(); z != 1.1 {
		t.Fatalf("Ctrl = in Settings zooms to %v, want 1.1", z)
	}
	x.loop.Post(func() { x.a.prefs.layout.Open = false })
	x.settle()
	if z := x.a.zoom(); z != 1.1 {
		t.Fatalf("after Settings closed the zoom is %v, want 1.1: the key ran twice", z)
	}

	// Before signing in, Ctrl 0 undoes a zoom saved too large.
	if err := x.store.Save(config.Settings{Zoom: 3}); err != nil {
		t.Fatal(err)
	}
	x.a = x.newApp(t)
	zoomTester(x, &shown)
	x.settle()
	x.tt.Key(ui.Cmd, ui.Key0)
	x.settle()
	if z := x.a.zoom(); z != 1 || shown != 1 {
		t.Fatalf("Ctrl 0 on the sign-in screen leaves the zoom at %v (shown %v), want 1", z, shown)
	}
}
