package app

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// visible says whether the element is shown whole inside a window h DIPs
// high: a box clipped away has no height.
func visible(r ui.Rect, h float32) bool { return r.H > 0 && r.Y >= 0 && r.Y+r.H <= h }

// In the smallest window, 760x480, the New Session form's fields scroll
// and its buttons stay at the bottom: with a repository and a new
// worktree chosen, Cancel and Start Session can be clicked, and the last
// field can be scrolled to (the owner's choice, 2026-11-20).
func TestNewSessionFitsTheSmallestWindowSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.repoProfiles()
	x.connect(t)
	x.tt.SetSize(760, 480)
	x.click(t, "New Session")
	f := x.a.newSession
	f.profile = "Duncan Idaho · agent"
	x.settle()
	f.folder.Repository = "frostyard/chairlift"
	x.settle()
	f.folder.Worktree = true
	f.title = "Fix CI"
	x.settle()
	for _, button := range []string{"Cancel", "Start Session"} {
		if b, ok := x.tt.AccessByRole(ui.RoleButton, button); !ok || !visible(b.Bounds, 480) {
			t.Fatalf("%s at %+v (found %v) in a 480-high window", button, b.Bounds, ok)
		}
	}
	x.snapshot(t, "new-session-small")
	thinking, ok := x.tt.AccessByRole(ui.RolePopUpButton, "Thinking")
	if ok && visible(thinking.Bounds, 480) {
		t.Fatal("the last field shows without a scroll: this window is not too short for the form")
	}
	fields, ok := x.tt.Access("New session's fields")
	if !ok {
		t.Fatal("the fields do not scroll")
	}
	for range 10 {
		x.tt.Scroll(fields.Bounds.X+fields.Bounds.W/2, fields.Bounds.Y+fields.Bounds.H/2, 0, 120)
		x.settle()
	}
	thinking, ok = x.tt.AccessByRole(ui.RolePopUpButton, "Thinking")
	if !ok || !visible(thinking.Bounds, 480) {
		t.Fatalf("the last field at %+v (found %v) after a scroll", thinking.Bounds, ok)
	}
	x.snapshot(t, "new-session-small-scrolled")
	x.click(t, "Start Session")
	x.until(t, func() bool {
		return x.a.place == placeSession && x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded
	})
	s, _ := x.a.st.Data.Session(x.a.st.Data.OpenID())
	if s.Profile != "duncan" || s.Title != "Fix CI" {
		t.Fatalf("opened %+v", s)
	}
	// Cancel too, in a new form at that size.
	x.click(t, "New Session")
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Cancel"); !ok || !visible(b.Bounds, 480) {
		t.Fatalf("Cancel at %+v (found %v)", b.Bounds, ok)
	}
	x.click(t, "Cancel")
	if x.a.newSession.open {
		t.Fatal("Cancel did not close the form")
	}
}
