package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// pendingPrompt puts session 1's field in the state Tab leaves it in
// until the next frame: the call still shows, and text waits to go in.
func (x *testApp) pendingPrompt(text string) {
	x.a.composer.edit = &fieldEdit{key: any(int64(1)), PromptEdit: state.PromptEdit{Text: text, Caret: len([]rune(text))}}
}

// The keys and the paste that come before the frame that puts a prompt in
// act on the prompt's text, never on the call the field still shows:
// Backspace edits it, a paste follows it, and Enter sends it, and never
// takes a prompt a second time (Codex review of #108, round 3).
func TestKeysBeforeThePromptShowsActOnIt(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/sh")
	x.pendingPrompt("Ship it.")
	x.tt.Key(0, ui.KeyBackspace)
	x.settle()
	if got := x.draftOf(1); got != "Ship it" {
		t.Fatalf("after Backspace the draft is %q", got)
	}

	*x.a.composer.draft(1) = ""
	x.settle()
	x.typeInto(t, "Message", "/sh")
	x.pendingPrompt("Ship it.")
	x.tt.SetClipboard(" Then tell me.")
	x.tt.Command("paste")
	x.settle()
	if got := x.draftOf(1); got != "Ship it. Then tell me." {
		t.Fatalf("after a paste the draft is %q", got)
	}

	*x.a.composer.draft(1) = ""
	x.settle()
	x.typeInto(t, "Message", "/sh")
	x.pendingPrompt("Ship it.")
	x.tt.Key(0, ui.KeyEnter)
	messages := proto.Path(proto.PathMessages, 1)
	x.until(t, func() bool { return len(x.srv.RequestsTo(http.MethodPost, messages)) == 1 })
	var sent proto.SendMessage
	if err := json.Unmarshal(x.srv.RequestsTo(http.MethodPost, messages)[0].Body, &sent); err != nil || sent.Text != "Ship it." {
		t.Fatalf("sent %+v, %v", sent, err)
	}
}

// A tap on a row of the phone's sheet as it slides away after a choice
// puts nothing more in (Codex review of #108, round 3).
func TestThePhonesSheetTakesNoTapAsItSlidesAway(t *testing.T) {
	x := newTestApp(t)
	x.srv.SetPrompts(ownersPrompts)
	x.phoneApp(t, false, nil)
	x.a.openSession(11)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	x.tap(t, "Insert a saved prompt")
	x.until(t, func() bool { return x.a.phone.prompts && x.a.st.InFlight() == 0 })
	if err := x.tt.Click("/ship-it"); err != nil {
		t.Fatal(err)
	}
	if x.a.phone.prompts {
		t.Fatal("the sheet stays open after a choice")
	}
	if _, ok := x.tt.Find("/status"); !ok {
		t.Skip("the sheet was gone before a second tap")
	}
	if err := x.tt.Click("/status"); err != nil {
		t.Fatal(err)
	}
	x.settle()
	if got := x.draftOf(11); !strings.HasPrefix(got, "Ship it:") || strings.Contains(got, "Tell me where") {
		t.Fatalf("the draft %q", got)
	}
}

// Escape closes the picker from a row the keyboard went to, as from the
// field, and the field gets the keys back with its text as it was (Codex
// review of #108, round 3).
func TestEscapeOnAPromptsRowClosesThePicker(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/st")
	for range 10 {
		if x.tt.Focused("/status") {
			break
		}
		x.tt.Key(ui.Shift, ui.KeyTab)
		x.settle()
	}
	if !x.tt.Focused("/status") {
		t.Fatalf("Shift+Tab never reached a prompt's row: %q", x.tt.Texts())
	}
	x.tt.Key(0, ui.KeyEscape)
	x.settle()
	if x.pickerShows() || x.draftOf(1) != "/st" || !x.tt.Focused("Message") {
		t.Fatalf("picker %v, draft %q, field focused %v", x.pickerShows(), x.draftOf(1), x.tt.Focused("Message"))
	}
}
