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

// ownersPrompts are the saved prompts the fake pocketd lists: the
// repository's examples, by name, and a file with a problem.
var ownersPrompts = proto.Prompts{Prompts: []proto.Prompt{
	{Name: "broken", Problem: `its front matter has the field "model": a prompt's are description and argument-hint`},
	{Name: "review-pr", Description: "Review a pull request and fix what you find", ArgumentHint: "<pull request number or URL>",
		Text: "Review pull request $ARGUMENTS. Read the whole diff and the issue it closes,\nrun the tests, and fix every problem you find in the same branch."},
	{Name: "ship-it", Description: "Rebase on main, run make verify, and open the pull request",
		Text: "Ship it: rebase this branch on origin/main, run `make verify` until it passes,\nthen open the pull request with `gh pr create`."},
	{Name: "status", Description: "Where each of your threads stands",
		Text: "Tell me where each of your open threads stands: its last report, its pull\nrequests and their CI, and what waits for me. One line a thread."},
}}

// promptSession opens a session on the fake pocketd, which lists the
// owner's prompts, with the desktop's window.
func promptSession(t *testing.T) *testApp {
	t.Helper()
	x := newTestApp(t)
	x.srv.SetPrompts(ownersPrompts)
	x.srv.AddSession(proto.Session{ID: 1, Title: "Fix the flaky CI in nsl", Cwd: "/home/team/projects/nsl",
		Model: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, State: proto.SessionIdle, UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "The **arm64** job fails about once a day.", At: at(-10 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryAssistant, Text: "Pinning the runner to ubuntu-24.04 fixed it in #31.", At: at(-9 * time.Minute)})
	x.connect(t)
	x.a.openSession(1)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded })
	return x
}

// draftOf is session id's message draft.
func (x *testApp) draftOf(id int64) string { return *x.a.composer.draft(id) }

// pickerShows says the / picker shows over the composer.
func (x *testApp) pickerShows() bool {
	_, ok := x.tt.AccessByRole(ui.RoleList, "Saved prompts")
	return ok
}

// / at the start of a message opens the picker, filtered as the name is
// typed; Enter puts the prompt in the composer with what followed its name
// as $ARGUMENTS, and sends nothing (ADR-0051).
func TestTheSlashPickerPutsAPromptInTheComposerSnapshot(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/")
	if !x.pickerShows() {
		t.Fatalf("no picker after /: %q", x.tt.Texts())
	}
	x.hasAll(t, "/review-pr", "<pull request number or URL>", "Review a pull request and fix what you find", "/ship-it", "/status",
		"/broken", "↑ ↓ choose · Enter or Tab puts it in your message · Esc closes")
	x.snapshot(t, "composer-prompts")
	x.tt.Type("rev 42")
	x.settle()
	if x.tt.HasText("/ship-it") || !x.tt.HasText("/review-pr") {
		t.Fatalf("the picker is not filtered by the name typed: %q", x.tt.Texts())
	}
	x.snapshot(t, "composer-prompts-filtered")
	x.tt.Key(0, ui.KeyEnter)
	x.settle()
	want := "Review pull request 42. Read the whole diff and the issue it closes,\nrun the tests, and fix every problem you find in the same branch."
	if got := x.draftOf(1); got != want {
		t.Fatalf("the composer holds %q, want %q", got, want)
	}
	if x.pickerShows() {
		t.Error("the picker shows after the prompt went in")
	}
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))); n != 0 {
		t.Fatalf("%d messages were sent", n)
	}
	// The owner edits it and sends it: then it goes.
	x.tt.Type(" Then tell me.")
	x.settle()
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))) == 1 })
	if !strings.Contains(string(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))[0].Body), "same branch. Then tell me.") {
		t.Fatalf("sent %s", x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))[0].Body)
	}
}

// Escape closes the picker and leaves the text as typed; it stays closed
// while the message starts with the call, so Enter sends it as typed, and
// the next / opens it again (ADR-0051).
func TestEscapeClosesThePickerAndKeepsTheText(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/ship")
	x.tt.Key(0, ui.KeyEscape)
	x.settle()
	if x.pickerShows() || x.draftOf(1) != "/ship" {
		t.Fatalf("after Escape: picker %v, draft %q", x.pickerShows(), x.draftOf(1))
	}
	x.tt.Type(" now")
	x.settle()
	if x.pickerShows() {
		t.Fatal("the picker came back as the owner typed on")
	}
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))) == 1 })
	x.until(t, func() bool { return x.draftOf(1) == "" })
	x.tt.Type("/")
	x.settle()
	if !x.pickerShows() {
		t.Fatal("the next / opens no picker")
	}
}

// Down moves the highlight and Tab puts that prompt in; a click on a row
// puts its prompt in; with nothing to put in, Enter sends nothing
// (ADR-0051).
func TestThePickersKeysAndClicks(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/")
	x.tt.Key(0, ui.KeyDown)
	x.settle()
	x.tt.Key(0, ui.KeyTab)
	x.settle()
	if got := x.draftOf(1); !strings.HasPrefix(got, "Ship it: rebase") {
		t.Fatalf("Tab put in %q", got)
	}
	x.a.composer.drafts[1] = new(string)
	x.settle()
	x.tt.Type("/st")
	x.settle()
	x.click(t, "/status")
	if got := x.draftOf(1); !strings.HasPrefix(got, "Tell me where each of your open threads stands") {
		t.Fatalf("the click put in %q", got)
	}
	*x.a.composer.drafts[1] = ""
	x.settle()
	x.tt.Type("/nothing-matches")
	x.settle()
	x.hasAll(t, "No saved prompt matches /nothing-matches.")
	x.tt.Key(0, ui.KeyEnter)
	x.settle()
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))); n != 0 || x.draftOf(1) != "/nothing-matches" {
		t.Fatalf("Enter with no match: %d sent, draft %q", n, x.draftOf(1))
	}
}

// The picker asks pocketd for the prompts each time it opens, so a prompt
// the owner added to the folder shows without a restart (ADR-0051).
func TestThePickerShowsAPromptAddedSince(t *testing.T) {
	x := promptSession(t)
	before := len(x.srv.RequestsTo(http.MethodGet, proto.PathPrompts))
	x.srv.SetPrompts(proto.Prompts{Prompts: append(slices.Clone(ownersPrompts.Prompts), proto.Prompt{Name: "triage", Description: "Sort the new issues", Text: "Triage the new issues."})})
	x.typeInto(t, "Message", "/")
	x.until(t, func() bool {
		return len(x.srv.RequestsTo(http.MethodGet, proto.PathPrompts)) > before && x.a.st.InFlight() == 0
	})
	x.hasAll(t, "/triage", "Sort the new issues")
}

// The palette's Insert Prompt opens the picker over the shown session's
// composer, its draft kept below a / of its own; with no session shown it
// is off (ADR-0051).
func TestThePalettesInsertPromptOpensThePicker(t *testing.T) {
	x := promptSession(t)
	*x.a.composer.draft(1) = "The CI is red."
	x.settle()
	cmd, ok := x.a.table.Command("insert-prompt")
	if !ok || cmd.Title != "Insert Prompt…" {
		t.Fatalf("no command: %+v", cmd)
	}
	if on, why := cmd.Enabled(); !on {
		t.Fatalf("off with a session shown: %s", why)
	}
	cmd.Run()
	x.settle()
	if !x.pickerShows() || x.draftOf(1) != "/\nThe CI is red." {
		t.Fatalf("picker %v, draft %q", x.pickerShows(), x.draftOf(1))
	}
	x.tt.Type("sh")
	x.settle()
	x.tt.Key(0, ui.KeyEnter)
	x.settle()
	if got := x.draftOf(1); !strings.HasPrefix(got, "Ship it:") || !strings.HasSuffix(got, "\nThe CI is red.") {
		t.Fatalf("the draft %q", got)
	}
	x.a.goInbox()
	x.settle()
	if on, _ := cmd.Enabled(); on {
		t.Error("on with no session shown")
	}
}

// On the iPhone, / opens the same picker over the composer, within the
// screen, and the button beside Attach opens the saved prompts as a
// sheet: a tap puts one in the composer, with the call's arguments, and
// closes the sheet, and nothing is sent (ADR-0051).
func TestThePhonesSavedPromptsSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.srv.SetPrompts(ownersPrompts)
	x.phoneApp(t, true, nil)
	x.a.openSession(11)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	x.typeInto(t, "Message", "/")
	if !x.pickerShows() {
		t.Fatalf("no picker after /: %q", x.tt.Texts())
	}
	x.within(t, ui.RoleList, "Saved prompts")
	x.within(t, ui.RoleButton, "Insert a saved prompt")
	x.within(t, ui.RoleButton, "Send")
	x.snapshot(t, "phone-prompt-picker")
	x.tt.Key(0, ui.KeyEscape)
	x.settle()
	x.tt.Type(" 31")
	x.settle()
	x.tap(t, "Insert a saved prompt")
	x.until(t, func() bool { return x.a.phone.prompts && x.a.st.InFlight() == 0 })
	x.hasAll(t, "Saved Prompts", "/review-pr", "<pull request number or URL> · Review a pull request and fix what you find", "/ship-it", "Need Fixing", "/broken")
	x.snapshot(t, "phone-prompt-sheet")
	x.tap(t, "/review-pr")
	if x.a.phone.prompts {
		t.Error("the sheet stays open after a tap")
	}
	if got := x.draftOf(11); !strings.HasPrefix(got, "Review pull request 31. Read the whole diff") {
		t.Fatalf("the composer holds %q", got)
	}
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 11))); n != 0 {
		t.Fatalf("%d messages were sent", n)
	}
}

// While the session's question waits, the composer answers it: / opens no
// picker there, as an answer takes no file either (ADR-0051).
func TestAnAnswerTakesNoPrompt(t *testing.T) {
	x := newTestApp(t)
	x.srv.SetPrompts(ownersPrompts)
	x.seedTeam()
	x.seedQuestion(questionOf("Q1", "Which runner?"))
	x.connect(t)
	x.openQuestionSession(t)
	x.until(t, func() bool { _, ok := x.a.st.Data.Question(1); return ok })
	x.typeInto(t, "Answer", "/")
	if x.pickerShows() {
		t.Fatal("an answer opened the picker")
	}
	if cmd, _ := x.a.table.Command("insert-prompt"); func() bool { on, _ := cmd.Enabled(); return on }() {
		t.Error("Insert Prompt is on while the composer answers")
	}
}
