package app

import (
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"
)

// The confirmations show one at a time, in the order asked, on a compact
// window and on a desktop's alike: one decided on a compact window leaves
// frostui's dialog too, so a window that widens again, as an iPad's
// leaving Split View, shows the next one, never the one decided
// (ADR-0043).
func TestAConfirmationDecidedCompactLeavesTheDesktopsQueue(t *testing.T) {
	var d confirms
	var ran []string
	d.Ask(frostui.Confirm{Title: "Merge the first?", Action: "Merge", OnConfirm: func() { ran = append(ran, "first merged") }})
	d.Ask(frostui.Confirm{Title: "Merge the second?", Action: "Merge", OnConfirm: func() { ran = append(ran, "second merged") },
		OnCancel: func() { ran = append(ran, "second cancelled") }})
	compact := true
	tt := ui.NewTester(func(c *ui.Context) { d.View(c, compact) }, phoneW, phoneH)
	if !tt.HasText("Merge the first?") || tt.HasText("Merge the second?") {
		t.Fatalf("the compact window shows %q", tt.Texts())
	}
	if n, ok := tt.AccessByRole(ui.RoleButton, "Cancel"); !ok || n.Bounds.X+n.Bounds.W > phoneW {
		t.Fatalf("the compact confirmation's Cancel: %+v, %v", n.Bounds, ok)
	}
	if err := tt.Click("Merge"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()

	compact = false
	tt.SetSize(1280, 820)
	if tt.HasText("Merge the first?") || !tt.HasText("Merge the second?") {
		t.Fatalf("the desktop's window shows %q", tt.Texts())
	}
	if err := tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if d.Showing() || tt.HasText("Merge the second?") {
		t.Errorf("a confirmation stays: %q", tt.Texts())
	}
	if want := []string{"first merged", "second cancelled"}; !slices.Equal(ran, want) {
		t.Errorf("ran %q, want %q", ran, want)
	}
}
