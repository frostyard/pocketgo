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
