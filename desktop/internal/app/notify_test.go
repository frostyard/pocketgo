package app

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// notifyAsk is an open ask of thread 3, in session 10.
func notifyAsk(id int64, question string) proto.Ask {
	return proto.Ask{ID: id, Thread: 3, SessionID: 10, Question: question, State: proto.AskOpen, CreatedAt: at(0)}
}

// startKept starts x's app as the program does, with its kept sign-in, and
// waits for its lists.
func (x *testApp) startKept(t *testing.T) {
	t.Helper()
	x.a.Start()
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connected })
	x.untilLoaded(t)
}

// The issue's "Done when" (frostyard/pocketgo#98), through the app: a new
// ask raises one notification while the window is not focused; a click on
// it opens the ask in the Inbox; the app's icon shows the Inbox's count;
// losing the connection and starting the app again notify nothing again.
func TestANewAskNotifiesOnceAndItsClickOpensIt(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddThread(proto.Thread{ID: 3, Title: "Fix the login", Agent: "duncan", State: proto.ThreadBlocked})
	x.srv.AddSession(proto.Session{ID: 10, Title: "Duncan on T3", State: proto.SessionWaiting, UpdatedAt: at(0)})
	x.srv.AddAsk(notifyAsk(1, "Which branch?"))
	if err := x.creds.SetToken(x.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	x.a.settings.Server = x.srv.URL
	x.startKept(t)
	if len(x.notified) != 0 {
		t.Fatalf("the first load notified %+v: what waits then is the badge's", x.notified)
	}
	if !slices.Equal(x.badges, []int{1}) {
		t.Fatalf("badges %v, want the Inbox's count, 1", x.badges)
	}

	x.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(notifyAsk(2, "Merge it now?"))})
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 2 })
	id := state.NoticeID(x.srv.URL, state.NoticeTarget{Card: state.AskCard(2)})
	want := Notification{ID: id, Title: "A2 asks you", Subtitle: "T3 · Fix the login", Body: "Merge it now?"}
	if len(x.notified) != 1 || x.notified[0] != want {
		t.Fatalf("notified %+v, want one: %+v", x.notified, want)
	}
	if x.badges[len(x.badges)-1] != 2 || state.InboxBadge(x.a.st.Data.Inbox()) != "2" {
		t.Errorf("badges %v, want 2 last, as the Inbox tab's", x.badges)
	}

	// While the window has the keyboard, a new ask shows nothing, and never
	// will: its notice is spent.
	x.a.SetFocused(true)
	x.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(notifyAsk(3, "Seen at once"))})
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 3 })
	x.a.SetFocused(false)
	// The connection lost and back.
	x.srv.DropStreams()
	x.a.st.RetryNow()
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connected && x.srv.Streams() == 1 })
	if len(x.notified) != 1 {
		t.Fatalf("notified %+v after focus and a reconnect, want the one", x.notified)
	}
	if st, _ := x.store.Load(); !slices.Contains(st.Notified[x.srv.URL], "ask:3") {
		t.Errorf("kept keys %q, want ask:3's", st.Notified[x.srv.URL])
	}

	// The app started again: nothing again.
	x.a.Close()
	x.a = x.newApp(t)
	x.tt = ui.NewTesterWith(x.a.View, 1280, 820, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
	x.startKept(t)
	if len(x.notified) != 1 {
		t.Fatalf("notified %+v after a restart, want the one", x.notified)
	}

	// The click, from the earlier run's notification, opens the Inbox at
	// the ask's card.
	x.a.goBoard()
	x.a.OpenNotification(id)
	if x.a.place != placeInbox || x.a.reveal != state.AskCard(2) {
		t.Fatalf("the click went to place %v, revealing %q", x.a.place, x.a.reveal)
	}
	x.settle()
	if x.a.reveal != "" {
		t.Errorf("the card %q was not revealed", x.a.reveal)
	}
	x.hasAll(t, "A2 asks you", "Merge it now?")

	// One of a question opens its session; one of another service, or not
	// PocketGo's, opens nothing.
	x.a.OpenNotification(state.NoticeID(x.srv.URL, state.NoticeTarget{Session: 10}))
	if x.a.place != placeSession || x.a.st.Data.OpenID() != 10 {
		t.Errorf("a session's notification opened place %v, session %d", x.a.place, x.a.st.Data.OpenID())
	}
	x.a.goBoard()
	x.a.OpenNotification(state.NoticeID("https://other.example", state.NoticeTarget{Session: 10}))
	x.a.OpenNotification("mygo-123")
	if x.a.place != placeBoard {
		t.Errorf("a foreign notification moved the window to %v", x.a.place)
	}

	// Signed out, the icon counts nothing.
	x.a.signOut()
	if x.badges[len(x.badges)-1] != 0 {
		t.Errorf("badges %v after signing out, want 0 last", x.badges)
	}
}

// A click on an ask's notification scrolls the Inbox to its card, below
// the others (ADR-0049).
func TestAClickScrollsTheInboxToItsCardSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddThread(proto.Thread{ID: 3, Title: "Fix the login", Agent: "duncan", State: proto.ThreadBlocked})
	long := "Which of these should the login keep? The session cookie, the device token, or both, given that the phone signs in with the token and the browser with the cookie, and that either may expire while the other does not."
	for id := int64(1); id <= 6; id++ {
		a := notifyAsk(id, long)
		a.Options = []string{"The cookie", "The token", "Both"}
		x.srv.AddAsk(a)
	}
	x.connect(t)
	last := notifyAsk(7, "Merge it now, or wait for CI?")
	last.CreatedAt = at(time.Minute)
	x.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &last})
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 7 })
	if len(x.notified) != 1 {
		t.Fatalf("notified %+v, want A7's", x.notified)
	}
	if r, ok := x.tt.Find("A7 asks you"); ok && r.Y >= 0 && r.Y+r.H <= 820 {
		t.Fatalf("A7's card shows at %+v before the click: the test shows no scroll", r)
	}
	x.a.OpenNotification(x.notified[0].ID)
	x.settle()
	r, ok := x.tt.Find("A7 asks you")
	if !ok || r.Y < 0 || r.Y+r.H > 820 {
		t.Errorf("A7's card lies at %+v (found %v) after the click, outside the window", r, ok)
	}
	x.snapshot(t, "inbox-notification-click")
}

// A click on a notification as the app starts, before its kept sign-in
// connected, opens what it is about once connected, in place of the
// session shown last (ADR-0049).
func TestAClickAtStartOpensOnceConnected(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	if err := x.creds.SetToken(x.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	x.a.settings.Server, x.a.settings.LastSession = x.srv.URL, 3
	x.a.OpenNotification(state.NoticeID(x.srv.URL, state.NoticeTarget{Session: 1}))
	x.startKept(t)
	if x.a.place != placeSession || x.a.st.Data.OpenID() != 1 {
		t.Errorf("place %v, session %d: want the notification's session 1", x.a.place, x.a.st.Data.OpenID())
	}
}

// Settings has a Notifications page, on by default: a switch for all of
// them and one for each kind, kept for the service signed in to. A kind
// switched off shows nothing; notifications off grays the kinds out and
// shows nothing at all (ADR-0049).
func TestNotificationSwitchesSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.connect(t)
	x.loop.Post(func() { x.a.table.Run("settings") })
	x.until(t, func() bool { return x.a.prefs.layout.Open })
	x.click(t, "Notifications")
	x.hasAll(t, "Notify me when", "Asks and questions", "Tool approvals", "Pull requests, plans and lessons", "Finished runs")
	for _, title := range []string{"Notifications", "Asks and questions", "Tool approvals", "Pull requests, plans and lessons", "Finished runs"} {
		if sw, ok := x.tt.AccessByRole(ui.RoleSwitch, title); !ok || !sw.Checked || sw.Disabled {
			t.Errorf("switch %q: %+v (found %v), want on", title, sw, ok)
		}
	}
	x.snapshot(t, "settings-notifications")

	flip := func(title string) {
		t.Helper()
		sw, ok := x.tt.AccessByRole(ui.RoleSwitch, title)
		if !ok {
			t.Fatalf("no switch %q in %q", title, x.tt.Texts())
		}
		x.tt.ClickAt(sw.Bounds.X+sw.Bounds.W/2, sw.Bounds.Y+sw.Bounds.H/2)
		x.settle()
	}
	flip("Asks and questions")
	if st, _ := x.store.Load(); st.Notify[x.srv.URL] != (config.Notify{AsksOff: true}) {
		t.Fatalf("saved switches %+v, want asks off", st.Notify[x.srv.URL])
	}
	x.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(notifyAsk(1, "Shows nothing"))})
	x.srv.Emit(proto.Event{Type: proto.EventApproval, Approval: &proto.Approval{ID: "a1", SessionID: 10, Tool: "bash", State: proto.ApprovalPending}})
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 1 && len(x.a.st.Data.Approvals()) == 1 })
	if len(x.notified) != 1 || x.notified[0].Title != "bash waits for your approval" {
		t.Fatalf("notified %+v, want the approval's alone", x.notified)
	}

	flip("Notifications")
	if sw, _ := x.tt.AccessByRole(ui.RoleSwitch, "Tool approvals"); !sw.Disabled || !sw.Checked {
		t.Errorf("with notifications off, Tool approvals is %+v: want it on and grayed out", sw)
	}
	x.snapshot(t, "settings-notifications-off")
	x.srv.Emit(proto.Event{Type: proto.EventApproval, Approval: &proto.Approval{ID: "a2", SessionID: 10, Tool: "bash", State: proto.ApprovalPending}})
	x.until(t, func() bool { return len(x.a.st.Data.Approvals()) == 2 })
	if len(x.notified) != 1 {
		t.Errorf("notified %+v with notifications off", x.notified)
	}

	// Back on, as at first: the service keeps no entry.
	flip("Notifications")
	flip("Asks and questions")
	if st, _ := x.store.Load(); len(st.Notify) != 0 {
		t.Errorf("saved switches %+v, want none: all on", st.Notify)
	}
}

// iOS shows no notification and has no Notifications page: its badge
// alone (ADR-0049).
func TestTheIPadNeitherNotifiesNorOffersTheSwitches(t *testing.T) {
	x := newTestApp(t)
	opts := x.options()
	opts.Platform = "ios"
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(x.a.View, 1180, 820, ui.TesterOptions{})
	x.connect(t)
	x.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(notifyAsk(1, "Shows nothing"))})
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 1 })
	if len(x.notified) != 0 {
		t.Errorf("the iPad notified %+v", x.notified)
	}
	if !slices.Equal(x.badges, []int{1}) {
		t.Errorf("the iPad's badges %v, want 1", x.badges)
	}
	x.loop.Post(func() { x.a.openSettings("") })
	x.until(t, func() bool { return x.a.prefs.layout.Open })
	if x.tt.HasText("Notify me when") || x.tt.HasText("Finished runs") {
		t.Error("the iPad's Settings offer notifications")
	}
}
