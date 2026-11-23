package app

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/pocketgo/proto"
)

func composerPickFiles(context.Context) ([]string, error) { return nil, nil }

func (x *testApp) openComposerSession(t *testing.T, id int64) {
	t.Helper()
	x.a.openSession(id)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == id && o.Loaded })
}

// assertComposerRow guards the single row, the slim desktop face and the
// finger-sized iOS targets, including the chip that yields width first.
func (x *testApp) assertComposerRow(t *testing.T, touch bool, labels ...string) {
	t.Helper()
	var center float32
	for i, label := range labels {
		n, ok := x.tt.Access(label)
		if !ok {
			t.Fatalf("no composer button %q", label)
		}
		cy := n.Bounds.Y + n.Bounds.H/2
		if i == 0 {
			center = cy
		} else if math.Abs(float64(cy-center)) > 2 {
			t.Errorf("%s center %v is not on composer row %v", label, cy, center)
		}
		if touch && (n.Bounds.H < 44 || (label != "Model" && n.Bounds.W < 44)) {
			t.Errorf("%s bounds %+v are not touch sized", label, n.Bounds)
		}
		if !touch && label == "Send" && (n.Bounds.H <= 0 || n.Bounds.H >= 40) {
			t.Errorf("desktop Send height %v is not slim", n.Bounds.H)
		}
	}
}

func (x *testApp) assertContextBelowComposer(t *testing.T) {
	t.Helper()
	meter, ok := x.tt.AccessByRole(ui.RoleMeter, "Context")
	if !ok {
		t.Fatal("no context meter")
	}
	field, ok := x.tt.Access("Message")
	if !ok || meter.Bounds.Y <= field.Bounds.Y+field.Bounds.H {
		t.Errorf("context %+v is not below Message %+v (found %v)", meter.Bounds, field.Bounds, ok)
	}
	send, ok := x.tt.AccessByRole(ui.RoleButton, "Send")
	if !ok || meter.Bounds.Y <= send.Bounds.Y+send.Bounds.H {
		t.Errorf("context %+v is not below the buttons %+v (found %v)", meter.Bounds, send.Bounds, ok)
	}
	if meter.Bounds.H != 4 || meter.Bounds.W != 56 {
		t.Errorf("context bar bounds %+v, want 56 by 4", meter.Bounds)
	}
}

// The desktop's composer has one slim row of buttons: Attach files as an
// icon, the model chip, which the keyboard reaches and whose tooltip holds
// its whole label, and a compact Send (ADR-0066).
func TestTheComposerKeepsItsButtonsOnOneRow(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.a.opts.PickFiles = composerPickFiles
	x.connect(t)
	x.openComposerSession(t, 3)
	x.assertComposerRow(t, false, "Attach files", "Model", "Send")
	// HasText includes accessible labels, even when no glyphs draw them.
	// The icon-only face fits 44 DIPs; the old labelled button cannot.
	attach, _ := x.tt.AccessByRole(ui.RoleButton, "Attach files")
	if attach.Bounds.W > 44 {
		t.Errorf("Attach files is not icon sized: %+v", attach.Bounds)
	}
	chip, _ := x.tt.Access("Model")
	if !chip.Focusable || !strings.HasPrefix(chip.Description, "Change the model: ") {
		t.Errorf("model chip is not focusable with a full tooltip: %+v", chip)
	}
}

// The context mark sits under the composer, not in the header, and shows
// nothing before an answer measured the context (ADR-0066).
func TestTheContextMarkSitsUnderTheComposer(t *testing.T) {
	x := newTestApp(t)
	x.seedUsage()
	x.srv.AddSession(proto.Session{ID: 42, Title: "Compacted", Model: deepseekRef},
		proto.Entry{ID: 1, Kind: proto.EntryAssistant, Usage: &proto.Usage{Input: 78_200}},
		proto.Entry{ID: 2, Kind: proto.EntryCompaction, Text: "Earlier work"})
	for i, route := range []string{"worker", "cross-review"} {
		x.srv.AddSession(proto.Session{ID: int64(43 + i), Title: route, Route: route, Model: deepseekRef},
			proto.Entry{ID: 1, Kind: proto.EntryAssistant, Usage: &proto.Usage{Input: 78_200}})
	}
	x.connect(t)
	x.openComposerSession(t, 21)
	x.assertContextBelowComposer(t)
	count := 0
	for _, n := range x.tt.AccessAll() {
		if n.Role == ui.RoleMeter && n.Name == "Context" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("context meters = %d, want only the mark below the composer", count)
	}
	x.openComposerSession(t, 11)
	if _, ok := x.tt.AccessByRole(ui.RoleMeter, "Context"); ok || x.tt.HasText("context ") {
		t.Error("unmeasured context draws a mark")
	}
	x.openComposerSession(t, 42)
	caption, ok := x.tt.Access("context compacted")
	field, fieldOK := x.tt.Access("Message")
	if !ok || !fieldOK || caption.Bounds.Y <= field.Bounds.Y+field.Bounds.H {
		t.Errorf("compacted caption %+v (found %v) is not below Message %+v (found %v)", caption, ok, field, fieldOK)
	}
	for _, id := range []int64{43, 44} {
		x.openComposerSession(t, id)
		meter, found := x.tt.AccessByRole(ui.RoleMeter, "Context")
		if !found || !x.tt.HasText("takes no message") {
			t.Errorf("read-only session %d has no context mark or caption: %+v", id, meter)
		}
		if _, found := x.tt.Access("Message"); found {
			t.Errorf("read-only session %d has a message field", id)
		}
	}
}

// The iPad's full window keeps the desktop's layout, and a finger can
// press each of the composer's slim controls (state.MinTarget, ADR-0066).
func TestTheIPadsComposerIsTouchSized(t *testing.T) {
	x := newTestApp(t)
	x.iPadApp(t, nil)
	x.a.opts.PickFiles = composerPickFiles
	x.seedUsage()
	x.connect(t)
	x.openComposerSession(t, 21)
	x.assertComposerRow(t, true, "Attach files", "Model", "Send")
}

// A phone's note under the title no longer says the context: the mark
// under the composer does (ADR-0066).
func TestThePhonesNoteNoLongerSaysTheContext(t *testing.T) {
	x := newTestApp(t)
	x.seedUsage()
	x.phoneApp(t, false, nil)
	x.openComposerSession(t, 21)
	x.assertContextBelowComposer(t)
	x.hasAll(t, "61%")
	found := false
	for _, text := range x.tt.Texts() {
		if strings.HasPrefix(text, "projects/pocketgo · deepseek-v4") {
			found = true
			if strings.Contains(text, "context ") {
				t.Errorf("the phone still draws a context note: %q", text)
			}
		}
	}
	if !found {
		t.Fatal("no phone session note")
	}
}

// The slim composer on the desktop, idle and while a run works, on a phone
// and on an iPad (ADR-0066).
func TestTheSlimComposerSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	t.Run("desktop", func(t *testing.T) {
		x := newTestApp(t)
		x.seedUsage()
		x.a.opts.PickFiles = composerPickFiles
		x.connect(t)
		x.openComposerSession(t, 21)
		x.assertComposerRow(t, false, "Attach files", "Model", "Send")
		x.assertContextBelowComposer(t)
		x.snapshot(t, "composer-slim")
		x.srv.SetLive(proto.Live{SessionID: 21, Busy: true})
		x.a.st.Open(21)
		x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Live.Busy })
		x.assertComposerRow(t, false, "Attach files", "Model", "Abort", "Send")
		x.snapshot(t, "composer-slim-busy")
	})
	t.Run("ipad", func(t *testing.T) {
		x := newTestApp(t)
		x.iPadApp(t, nil)
		x.a.opts.PickFiles = composerPickFiles
		x.seedUsage()
		x.connect(t)
		x.openComposerSession(t, 21)
		x.assertComposerRow(t, true, "Attach files", "Model", "Send")
		x.assertContextBelowComposer(t)
		x.snapshot(t, "ipad-composer-slim")
	})
	// Safe areas enable touch process-wide, so the phone draws last.
	x := newTestApp(t)
	x.seedUsage()
	x.phoneApp(t, true, nil)
	x.openComposerSession(t, 21)
	x.assertComposerRow(t, true, "Attach files", "Model", "Send")
	x.assertContextBelowComposer(t)
	x.snapshot(t, "phone-composer-slim")
}
