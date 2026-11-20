package state

import (
	"math"
	"testing"
)

// The window's zoom steps through a browser's levels and stays within
// them; 100% is saved as none, and a value edited into the settings file
// shows as the nearest level, or 100% when it is no zoom.
func TestZoomStepsThroughItsLevels(t *testing.T) {
	if z := Zoom(0); z != 1 {
		t.Fatalf("zoom with none saved = %v, want 1", z)
	}
	saved := float32(0)
	for _, step := range []struct {
		by   int
		want float32
	}{{1, 1.1}, {1, 1.25}, {-3, 0.9}, {-20, 0.5}, {-1, 0.5}, {100, 3}, {1, 3}} {
		saved = ZoomBy(saved, step.by)
		if z := Zoom(saved); z != step.want {
			t.Fatalf("ZoomBy(%d) = %v, want %v", step.by, z, step.want)
		}
	}
	if s := ZoomSaved(1); s != 0 {
		t.Errorf("100%% saves %v, want none", s)
	}
	if s := ZoomBy(1.1, -1); s != 0 {
		t.Errorf("stepping back to 100%% saves %v, want none", s)
	}
	for saved, want := range map[float32]float32{
		1.3:                                  1.25,
		0.1:                                  0.5,
		9:                                    3,
		-2:                                   1,
		float32(math.NaN()):                  1,
		float32(math.Inf(1)):                 1,
		float32(math.Inf(-1)):                1,
		float32(math.SmallestNonzeroFloat32): 0.5,
		1e9:                                  3,
		math.MaxFloat32:                      3,
	} {
		if z := Zoom(saved); z != want {
			t.Errorf("a saved %v zooms to %v, want %v", saved, z, want)
		}
	}
	if got := ZoomLabel(0.67); got != "67%" {
		t.Errorf("ZoomLabel(0.67) = %q", got)
	}
	labels := ZoomLabels()
	if len(labels) != len(ZoomLevels) || labels[0] != "50%" || labels[len(labels)-1] != "300%" {
		t.Errorf("labels %q", labels)
	}
	for _, l := range labels {
		if z, ok := ZoomOfLabel(l); !ok || ZoomLabel(z) != l {
			t.Errorf("label %q gives %v %v", l, z, ok)
		}
	}
	if _, ok := ZoomOfLabel("123%"); ok {
		t.Error("a label that is no level gives a zoom")
	}
}

// The window never zooms past the largest level at which the session
// list and the session fit side by side, and the cap never takes it
// below 100%, nor below a zoom under 100% (the owner's choice, Codex
// review of #39).
func TestZoomFitsTheWindow(t *testing.T) {
	for _, c := range []struct {
		z, width, want float32
	}{
		{2, 1280, 1.75},
		{3, 1280, 1.75},
		{1.5, 1280, 1.5},
		{3, 1920, 2.5},
		{2, 1282, 2},
		{1.25, 600, 1},
		{1, 600, 1},
		{0.8, 600, 0.8},
		{3, 0, 3},
		{0, 1280, 1},
	} {
		if got := ZoomFit(c.z, c.width, 641); got != c.want {
			t.Errorf("ZoomFit(%v, %v) = %v, want %v", c.z, c.width, got, c.want)
		}
	}
}
