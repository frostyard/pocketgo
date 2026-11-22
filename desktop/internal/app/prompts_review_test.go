package app

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/proto"
)

// Holding Enter puts the prompt in once and sends nothing: its repeats
// are the picker's until the key is let go, and the next press sends
// (Codex review of #108, round 1).
func TestHoldingEnterNeverSendsTheInsertedPrompt(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/rev 42")
	x.tt.KeyDown(ui.KeyEnter)
	x.settle()
	for range 4 {
		x.tt.KeyRepeat(0, ui.KeyEnter)
		x.settle()
	}
	messages := proto.Path(proto.PathMessages, 1)
	if n := len(x.srv.RequestsTo(http.MethodPost, messages)); n != 0 {
		t.Fatalf("holding Enter sent %d messages", n)
	}
	if got := x.draftOf(1); !strings.HasPrefix(got, "Review pull request 42.") || strings.Contains(got, "\n\n") {
		t.Fatalf("the draft %q", got)
	}
	x.tt.KeyUp(ui.KeyEnter)
	x.settle()
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return len(x.srv.RequestsTo(http.MethodPost, messages)) == 1 })
}

// A prompt goes in as a step of undo of its own: one Undo brings back the
// call as typed, with its arguments (Codex review of #108, round 1).
func TestOneUndoTakesAPromptBackOut(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/rev 42")
	x.tt.Key(0, ui.KeyEnter)
	x.settle()
	if got := x.draftOf(1); !strings.HasPrefix(got, "Review pull request 42.") {
		t.Fatalf("the draft %q", got)
	}
	x.tt.Key(ui.Cmd, ui.KeyZ)
	x.settle()
	if got := x.draftOf(1); got != "/rev 42" {
		t.Fatalf("after one Undo the draft is %q", got)
	}
	// A click on a row is one step too.
	*x.a.composer.draft(1) = ""
	x.settle()
	x.typeInto(t, "Message", "/st")
	x.click(t, "/status")
	x.tt.Key(ui.Cmd, ui.KeyZ)
	x.settle()
	if got := x.draftOf(1); got != "/st" {
		t.Fatalf("after one Undo of a click the draft is %q", got)
	}
}

// A picker left open in a session the owner went away from asks pocketd
// again as the session shows, so a prompt added meanwhile shows (Codex
// review of #108, round 1).
func TestAPickerShownAgainShowsAPromptAddedMeanwhile(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/")
	if !x.pickerShows() {
		t.Fatal("no picker")
	}
	x.a.goInbox()
	x.settle()
	x.srv.SetPrompts(proto.Prompts{Prompts: append(slices.Clone(ownersPrompts.Prompts), proto.Prompt{Name: "triage", Description: "Sort the new issues", Text: "Triage."})})
	before := len(x.srv.RequestsTo(http.MethodGet, proto.PathPrompts))
	x.a.openSession(1)
	x.settle()
	x.until(t, func() bool {
		return len(x.srv.RequestsTo(http.MethodGet, proto.PathPrompts)) > before && x.a.st.InFlight() == 0
	})
	x.hasAll(t, "/triage", "Sort the new issues")
}

// The phone's sheet puts a prompt in with its caret after it, as the
// picker does: the field keeps the keys while the sheet is up, as a
// finger on the button takes none, so what the owner types next goes
// after the prompt (Codex review of #108, round 1).
func TestThePhonesSheetPutsTheCaretAfterThePrompt(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.srv.SetPrompts(ownersPrompts)
	x.phoneApp(t, true, nil)
	x.a.openSession(11)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	x.typeInto(t, "Message", "/")
	x.tt.Key(0, ui.KeyEscape)
	x.settle()
	x.tap(t, "Insert a saved prompt")
	x.until(t, func() bool { return x.a.phone.prompts && x.a.st.InFlight() == 0 })
	x.tap(t, "/ship-it")
	if got := x.draftOf(11); !strings.HasPrefix(got, "Ship it:") || x.a.phone.prompts {
		t.Fatalf("draft %q, sheet %v", got, x.a.phone.prompts)
	}
	if !x.tt.Focused("Message") {
		t.Fatal("the field lost the keys to the sheet")
	}
	x.tt.Type(" Then tell me.")
	x.settle()
	if got := x.draftOf(11); !strings.HasSuffix(got, "`gh pr create`. Then tell me.") {
		t.Fatalf("typed after the prompt: %q", got)
	}
}

// The phone's sheet closes when its composer no longer writes a message,
// as when a question comes: a tap then never changes a draft that is out
// of sight (Codex review of #108, round 1).
func TestThePhonesSheetClosesWhenAQuestionComes(t *testing.T) {
	x := newTestApp(t)
	x.srv.SetPrompts(ownersPrompts)
	x.phoneApp(t, false, nil)
	x.a.openSession(11)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	*x.a.composer.draft(11) = "Keep these words."
	x.settle()
	x.tap(t, "Insert a saved prompt")
	x.until(t, func() bool { return x.a.phone.prompts && x.a.st.InFlight() == 0 })
	q := proto.Question{ID: "Q9", SessionID: 11, CallID: "q9", Question: "Pin the runner?", State: proto.QuestionWaiting,
		CreatedAt: at(-time.Minute), Deadline: at(29 * time.Minute)}
	x.putQuestion(t, q)
	x.settle()
	if x.a.phone.prompts {
		t.Fatal("the sheet stays open over an answer")
	}
	if got := x.draftOf(11); got != "Keep these words." {
		t.Fatalf("the message's draft %q", got)
	}
}
