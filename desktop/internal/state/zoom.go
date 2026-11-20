package state

import (
	"fmt"
	"math"
)

// ZoomLevels are the window's zooms, in the steps a browser takes, as
// frostyard/pocket has them: Zoom scales text, icons and spacing together.
var ZoomLevels = []float32{0.5, 0.67, 0.75, 0.8, 0.9, 1, 1.1, 1.25, 1.5, 1.75, 2, 2.5, 3}

// zoomIndex is the index of the level nearest to z. A z beyond the
// levels is the level at that end: compared in float32, the distances to
// a z far beyond them all round alike (Codex review of #39).
func zoomIndex(z float32) int {
	if z <= ZoomLevels[0] {
		return 0
	}
	if z >= ZoomLevels[len(ZoomLevels)-1] {
		return len(ZoomLevels) - 1
	}
	best := 0
	for i, l := range ZoomLevels {
		if math.Abs(float64(l-z)) < math.Abs(float64(ZoomLevels[best]-z)) {
			best = i
		}
	}
	return best
}

// Zoom is the window's zoom for the one saved: the level nearest to it,
// and 100% when none is saved. A value edited into the settings file that
// is no zoom (zero, negative, not a number, infinite) is 100% too.
func Zoom(saved float32) float32 {
	if !(saved > 0) || math.IsInf(float64(saved), 0) {
		return 1
	}
	return ZoomLevels[zoomIndex(saved)]
}

// ZoomSaved is what the settings keep for zoom z: the level nearest to
// it, and none (0) for 100%, so the file holds only a zoom chosen.
func ZoomSaved(z float32) float32 {
	if z = Zoom(z); z == 1 {
		return 0
	}
	return z
}

// ZoomBy is what the settings keep after zooming steps levels in from the
// zoom saved (out when steps is negative), staying within the levels.
func ZoomBy(saved float32, steps int) float32 {
	i := min(max(zoomIndex(Zoom(saved))+steps, 0), len(ZoomLevels)-1)
	return ZoomSaved(ZoomLevels[i])
}

// ZoomLabel writes zoom z as Settings shows it: "125%".
func ZoomLabel(z float32) string { return fmt.Sprintf("%d%%", int(math.Round(float64(z)*100))) }

// ZoomLabels are the levels as Settings offers them, smallest first.
func ZoomLabels() []string {
	out := make([]string, len(ZoomLevels))
	for i, z := range ZoomLevels {
		out[i] = ZoomLabel(z)
	}
	return out
}

// ZoomOfLabel is the level Settings shows as label, and whether there is
// one.
func ZoomOfLabel(label string) (float32, bool) {
	for _, z := range ZoomLevels {
		if ZoomLabel(z) == label {
			return z, true
		}
	}
	return 0, false
}

// ZoomFit is the zoom the window shows for zoom z: z, or the largest
// level below it at which a window width DIPs wide at 100% still shows
// need DIPs, so the session list and the session always fit side by side
// (the owner's choice, Codex review of #39). It never goes below 100%, nor
// below z when z is below 100%; a width not known yet leaves z as it is.
func ZoomFit(z, width, need float32) float32 {
	z = Zoom(z)
	if !(width > 0) {
		return z
	}
	floor := min(z, 1)
	i := zoomIndex(z)
	for i > 0 && ZoomLevels[i] > floor && width/ZoomLevels[i] < need {
		i--
	}
	return ZoomLevels[i]
}
