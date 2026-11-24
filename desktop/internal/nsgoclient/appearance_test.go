package nsgoclient

import (
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// Frostyard's grounds, from the kit's spec: the window's root paints them.
const (
	frostyardDarkGround  = 0x06111d
	frostyardLightGround = 0xf5f9fc
)

// A theme saved by an earlier build, under the same key and names, still
// shows at the start, and the window's root paints Frostyard's ground for it.
func TestSavedThemeApplies(t *testing.T) {
	for _, tc := range []struct {
		saved  string
		want   core.Appearance
		ground core.Color
	}{
		{"Dark", core.AppearanceDark, core.RGB(frostyardDarkGround)},
		{"Light", core.AppearanceLight, core.RGB(frostyardLightGround)},
		{"System", core.AppearanceSystem, 0},
		{"", core.AppearanceSystem, 0},
	} {
		t.Run("saved "+tc.saved, func(t *testing.T) {
			var opts []nsgotest.Option
			if tc.saved != "" {
				opts = append(opts, nsgotest.Storage(map[string]string{"appearance": tc.saved}))
			}
			app := nsgotest.Start(t, func(cx *core.Scope) core.Node { return newClient(cx, pocketdtest.New().Transport()) }, opts...)
			if app.Appearance != tc.want {
				t.Fatalf("the app asked for appearance %v, want %v", app.Appearance, tc.want)
			}
			if got := app.Find("id:frame").Style.Background; tc.ground != 0 && got != tc.ground {
				t.Fatalf("the window's ground is %#x, want %#x", uint32(got), uint32(tc.ground))
			}
		})
	}
}

// The Theme setting shows a choice at once, repaints the ground, and keeps
// the choice under the names the earlier select saved.
func TestThemeChoiceIsKept(t *testing.T) {
	app := signedInApp(t, pocketdtest.New(), 1230, 800)
	app.Press("label:Settings")
	app.Find("label:Appearance")
	for _, tc := range []struct {
		name   string
		want   core.Appearance
		ground core.Color
	}{
		{"Dark", core.AppearanceDark, core.RGB(frostyardDarkGround)},
		{"System", core.AppearanceSystem, 0},
		{"Light", core.AppearanceLight, core.RGB(frostyardLightGround)},
	} {
		app.Press("text:" + tc.name)
		if app.Appearance != tc.want {
			t.Fatalf("after %s the app asked for appearance %v, want %v", tc.name, app.Appearance, tc.want)
		}
		if got, _ := app.Storage().GetItem("appearance"); got != tc.name {
			t.Fatalf("after %s the kept theme is %q", tc.name, got)
		}
		if got := app.Find("id:frame").Style.Background; tc.ground != 0 && got != tc.ground {
			t.Fatalf("after %s the window's ground is %#x, want %#x", tc.name, uint32(got), uint32(tc.ground))
		}
	}
}

// The Theme control stays inside a narrow window's settings.
func TestThemeChoiceFitsACompactWindow(t *testing.T) {
	app := signedInApp(t, pocketdtest.New(), 390, 730)
	app.Press("label:Navigation")
	app.Press("label:Settings")
	choice := app.WaitFor("label:Appearance", 0).Bounds()
	if choice.Origin.X < 0 || choice.Origin.X+choice.Size.Width > 390 {
		t.Fatalf("the Theme control spans %v to %v in a window 390 wide", choice.Origin.X, choice.Origin.X+choice.Size.Width)
	}
}

// The frame fills the window at every zoom; the view inside it lays the
// workspace out at the window's size divided by the zoom, then scales it.
func TestTheFrameKeepsTheZoom(t *testing.T) {
	for _, tc := range []struct {
		saved         string
		width, height core.Points
	}{
		{"", 1230, 800},
		{"1.25", 984, 640},
	} {
		t.Run("zoom "+tc.saved, func(t *testing.T) {
			var opts []nsgotest.Option
			if tc.saved != "" {
				opts = append(opts, nsgotest.Storage(map[string]string{"zoom": tc.saved}))
			}
			app := signedInApp(t, pocketdtest.New(), 1230, 800, opts...)
			if frame := app.Find("id:frame").Frame; frame.Origin != (core.Point{}) || frame.Size.Width != 1230 || frame.Size.Height != 800 {
				t.Fatalf("the frame is %+v, want the window, 1230 by 800", frame)
			}
			if zoom := app.Find("id:zoom").Frame.Size; zoom.Width != tc.width || zoom.Height != tc.height {
				t.Fatalf("the zoomed view is laid out %v by %v, want %v by %v", zoom.Width, zoom.Height, tc.width, tc.height)
			}
		})
	}
}
