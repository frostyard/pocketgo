package app

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// phoneKeyboard is the height of an iPhone's keyboard, as the window's
// bottom safe area takes it while it shows.
const phoneKeyboard = 336

// keyboardUp has the phone's keyboard cover the bottom of the screen, as
// a bottom safe area (Tester.SetInsets), which only a phone process may
// draw (inPhoneProcess).
func (x *testApp) keyboardUp(t *testing.T) {
	t.Helper()
	if os.Getenv(phoneProcessEnv) != t.Name() {
		t.Fatal("a keyboard outside a phone process would turn touch on for the other tests")
	}
	x.tt.SetInsets(phoneTop, 0, phoneKeyboard, 0)
	x.settle()
}

// Send stays on a phone's screen while a run works, with Attach files and
// Abort beside it, and the model's chip on a row of its own (Codex review
// of #85, round 2).
func TestAPhonesSendStaysOnScreenWhileARunWorks(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.a.opts.PickFiles = func(context.Context) ([]string, error) { return nil, nil }
	x.srv.SetLive(proto.Live{SessionID: 11, Busy: true})
	x.tap(t, "Sessions")
	x.tap(t, "T1 · Fix the flaky arm64 job")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.Live.Busy })
	for _, label := range []string{"Attach files", "Abort", "Send"} {
		x.within(t, ui.RoleButton, label)
	}
	if chip, ok := x.tt.Access("Model"); !ok || chip.Bounds.X+chip.Bounds.W > phoneW {
		t.Errorf("the model's chip runs past the screen: %+v, %v", chip.Bounds, ok)
	}
}

// New Session's buttons stay above a phone's keyboard as it comes up
// (Codex review of #85, round 2).
func TestAPhonesNewSessionStaysAboveTheKeyboard(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.phoneApp(t, true, nil)
	x.tap(t, "Sessions")
	x.tap(t, "New Session")
	x.until(t, func() bool { return x.tt.HasText("Start Session") })
	x.keyboardUp(t)
	for _, label := range []string{"Start Session", "Cancel"} {
		n, ok := x.tt.AccessByRole(ui.RoleButton, label)
		if !ok || n.Bounds.Y+n.Bounds.H > phoneH-phoneKeyboard+0.5 {
			t.Errorf("%s is under the keyboard: %+v, %v", label, n.Bounds, ok)
		}
	}
}

// An unsaved GitHub sign-in shows with Save Again on the connecting
// screen too: GitHub opens without pocketd (Codex review of #85, round 2).
func TestAPhoneSavesGitHubAgainWhilePocketdNeverAnswers(t *testing.T) {
	x := newTestApp(t)
	auth := &ghAuth{signedIn: true, unsaved: true}
	x.phoneWindow(t, auth)
	x.a.Start()
	x.until(t, x.a.gh.Opened)
	x.srv.FailOnce(http.MethodGet, proto.PathEvents, http.StatusBadGateway)
	if err := x.a.st.Connect(x.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connecting && x.a.st.Conn.Reason != "" })
	x.hasAll(t, "GitHub sign-in not saved")
	x.click(t, "Save Again")
	x.until(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return auth.saved == 1 })
}

// The console's key shows the Activity on a phone that never drew a wide
// window, which brings the key to the command table (Codex review of #85,
// round 2).
func TestThePhonesActivityKeyWorksFromTheStart(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tt.Key(ui.Cmd, ui.KeyJ)
	x.settle()
	if !x.a.shell.Console.Open || !x.tt.HasText("Close Activity") {
		t.Errorf("the console's key shows no Activity: %v, %q", x.a.shell.Console.Open, x.tt.Texts())
	}
}

// The filter's results list each session found once, and no worker it
// did not find (Codex review of #85, round 2).
func TestAPhonesFilterListsOnlyWhatItFound(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	model := proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}
	worker := proto.Session{ID: 30, Title: "T1 · helper", Cwd: "/home/team/projects/nsl--worker", Model: model, Route: "worker-default",
		StartedBy: 11, State: proto.SessionIdle, UpdatedAt: at(-90 * time.Second)}
	x.srv.Emit(proto.Event{Type: proto.EventSession, Session: &worker})
	x.until(t, func() bool { _, ok := x.a.st.Data.Session(30); return ok })
	x.tap(t, "Sessions")
	x.a.filter = "flaky"
	x.settle()
	if n := x.rowsTitled("T1 · Fix the flaky arm64 job"); n != 1 {
		t.Errorf("the session found shows %d times", n)
	}
	if n := x.rowsTitled("T1 · helper"); n != 0 {
		t.Errorf("the worker the filter did not find shows %d times", n)
	}
}

// rowsTitled counts the phone's list rows that a session titled title
// shows in.
func (x *testApp) rowsTitled(title string) int {
	n := 0
	for _, s := range x.tt.Texts() {
		if s == title {
			n++
		}
	}
	return n
}

// The withdrawn draft's card on a phone puts its thread under its title
// (Codex review of #85, round 2).
func TestAPhonesWithdrawnDraftPutsItsThreadUnderItsTitle(t *testing.T) {
	x := newTestApp(t)
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor"})
	x.srv.AddSession(proto.Session{ID: 3, Title: "Odrade", Cwd: "/home/team", Model: model, Profile: "odrade", Route: "profile",
		State: proto.SessionIdle, UpdatedAt: at(-time.Hour)})
	x.srv.AddSession(proto.Session{ID: 7, Title: "T4 · Renumber the ADR", Cwd: "/home/team/projects/pocketgo--duncan-renumber-the-adr-t4",
		Model: model, Profile: "duncan", Route: "profile", State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	x.srv.AddThread(proto.Thread{ID: 4, Title: "Renumber the ADR", Agent: "duncan", Supervisor: 3, Session: 7, State: proto.ThreadBlocked,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	asked := proto.Ask{ID: 3, Thread: 4, SessionID: 7, Question: "core#281 took ADR-0088. Which number should mine take?", State: proto.AskOpen, CreatedAt: at(-2 * time.Minute)}
	x.srv.AddAsk(asked)
	x.phoneWindow(t, nil)
	x.connect(t)
	x.typeInto(t, "Answer to A3", "Take 0089, and say so in the PR.")
	asked.State = proto.AskWithdrawn
	x.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &asked})
	x.until(t, func() bool { return len(x.a.st.WithdrawnDrafts()) == 1 })
	title, ok := x.tt.Find("A3 was withdrawn")
	if !ok {
		t.Fatalf("no withdrawn draft in %q", x.tt.Texts())
	}
	thread, ok := x.tt.Find("T4 · Renumber the ADR")
	if !ok || thread.Y < title.Y+title.H {
		t.Errorf("the thread's caption %+v is not under the title %+v", thread, title)
	}
}

// About goes as it is closed at any width: closed on a wide window, it
// does not come back as the window narrows (Codex review of #85, round 2).
func TestAboutClosedWideStaysClosedNarrow(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tap(t, "More")
	x.tap(t, "About PocketGo")
	if !x.tt.HasText("MIT") {
		t.Fatalf("no About in %q", x.tt.Texts())
	}
	x.tt.SetSize(820, 1180)
	x.settle()
	x.click(t, "Close")
	x.tt.SetSize(phoneW, phoneH)
	x.settle()
	if x.tt.HasText("MIT") {
		t.Errorf("About came back as the window narrowed: %q", x.tt.Texts())
	}
}
