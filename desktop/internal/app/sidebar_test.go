package app

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// seedAgents gives the fake pocketd a team and its sessions: the owner's
// own, the Clerk Odrade's (one NEW, one a week old), the supervisor
// Sheeana's running, and a specialist's waiting for an approval.
func (x *testApp) seedAgents() {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.SetProfiles([]proto.Profile{
		{ID: "duncan", Name: "Duncan", Role: proto.RoleAgent, Home: "/home/team/agents/duncan", Cwd: "/home/team/agents/duncan", Runs: model,
			Repositories: []string{"frostyard/nsl", "frostyard/snow"}},
		{ID: "irulan", Name: "Irulan", Role: proto.RoleAgent, Runs: model},
		{ID: "odrade", Name: "Odrade", Role: proto.RoleClerk, Runs: proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4-flash"}},
		{ID: "sheeana", Name: "Sheeana", Role: proto.RoleSupervisor, Runs: model},
	})
	x.srv.AddSession(proto.Session{ID: 1, Title: "Plan the Q4 roadmap", Cwd: "/home/team/agents/sheeana", Model: model, Profile: "sheeana",
		State: proto.SessionRunning, UpdatedAt: at(-time.Minute)})
	x.srv.AddSession(proto.Session{ID: 2, Title: "T7 · Fix the flaky CI in nsl", Cwd: "/home/team/projects/nsl--duncan-fix-ci", Model: model,
		Profile: "duncan", State: proto.SessionWaiting, UpdatedAt: at(-2 * time.Minute)})
	x.srv.AddSession(proto.Session{ID: 3, Title: "Clerk: Desk batch", Cwd: "/home/team/agents/odrade", Model: model, Profile: "odrade",
		State: proto.SessionIdle, UpdatedAt: at(-3 * time.Minute)})
	x.srv.AddSession(proto.Session{ID: 4, Title: "Homewiki: garden plan", Cwd: "/home/team/projects/homewiki", Model: model,
		State: proto.SessionIdle, UpdatedAt: at(-4 * time.Minute)})
	x.srv.AddSession(proto.Session{ID: 5, Title: "Odrade: desk sweep", Cwd: "/home/team/agents/odrade", Model: model, Profile: "odrade",
		State: proto.SessionIdle, UpdatedAt: at(-8 * 24 * time.Hour)})
	x.srv.AddApproval(proto.Approval{ID: "a1", SessionID: 2, CallID: "b1", Tool: "bash",
		Arguments: []byte(`{"command":"git push --force-with-lease origin duncan/fix-ci"}`), State: proto.ApprovalPending, RequestedAt: at(-time.Minute)})
}

// untilProfiles waits until the profiles loaded, as the sidebar names the
// agents by them.
func (x *testApp) untilProfiles(t *testing.T) {
	t.Helper()
	x.until(t, func() bool {
		status, _ := x.a.st.Data.ProfilesStatus()
		return status == state.ListLoaded && x.a.st.InFlight() == 0
	})
}

// The sidebar is pocket's: the places, then the team, the owner's own
// sessions first, then each agent with its role, a count of its sessions
// that wait and a spinner while one runs; then the recent sessions, each
// tagged with the agent it belongs to, NEW until the owner opens it, and
// dimmed when old.
func TestTheSidebarGroupsSessionsByAgentSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	// The owner saw the desk sweep as it is, a week and a day ago.
	x.a.settings.Seen = map[string]map[int64]time.Time{x.srv.URL: {5: at(-8 * 24 * time.Hour)}}
	x.connect(t)
	x.untilProfiles(t)
	x.hasAll(t, "PocketGo", "Inbox", "Work Board", "Team", "Recent", "You", "Duncan, 1 waiting for you", "Irulan", "Odrade, clerk",
		"Sheeana, supervisor, busy", "Plan the Q4 roadmap, Sheeana, busy",
		"T7 · Fix the flaky CI in nsl, waits for you: bash git push --force-with-lease origin duncan/fix-ci",
		"Clerk: Desk batch, finished, not read", "Homewiki: garden plan, finished, not read", "Odrade: desk sweep")
	// "Clerk: Desk batch [Odrade]": the tag names the agent the session
	// belongs to.
	if !slices.Contains(x.tt.Texts(), "Odrade") {
		t.Errorf("no tag names Odrade: %q", x.tt.Texts())
	}
	if r, _ := x.a.sidebarData().Session(5); !r.Dim || r.Mark != state.MarkSeen {
		t.Errorf("the desk sweep, seen and a week old, shows %+v, want it dimmed", r)
	}
	x.a.sidebar.SetOpen(state.SectionTeam, state.GroupKey("odrade"), true)
	x.a.sidebar.SetOpen(state.SectionTeam, state.GroupKey(""), true)
	x.settle()
	x.snapshot(t, "sidebar-team")

	// Opening a session ends its NEW, and the mark is kept for the next
	// start; the session the window shows is never NEW.
	x.click(t, "Clerk: Desk batch, finished, not read")
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 3 })
	if st, _ := x.store.Load(); !st.Seen[x.srv.URL][3].Equal(at(-3 * time.Minute)) {
		t.Errorf("the seen mark kept is %v", st.Seen[x.srv.URL])
	}
	// An update while it shows leaves it read; once the owner left it, a
	// later one makes it NEW again.
	s3 := proto.Session{ID: 3, Title: "Clerk: Desk batch", Cwd: "/home/team/agents/odrade", Model: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"},
		Profile: "odrade", State: proto.SessionIdle, UpdatedAt: at(-30 * time.Second)}
	x.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s3})
	x.until(t, func() bool { s, _ := x.a.st.Data.Session(3); return s.UpdatedAt.Equal(s3.UpdatedAt) })
	if r, _ := x.a.sidebarData().Session(3); r.Mark != state.MarkSeen {
		t.Errorf("the session shown is marked %d", r.Mark)
	}
	x.click(t, "Inbox")
	s3.UpdatedAt = at(-10 * time.Second)
	x.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s3})
	x.until(t, func() bool { s, _ := x.a.st.Data.Session(3); return s.UpdatedAt.Equal(s3.UpdatedAt) })
	if r, _ := x.a.sidebarData().Session(3); r.Mark != state.MarkNew {
		t.Errorf("a session updated after the owner left it is marked %d, want NEW", r.Mark)
	}
	if st, _ := x.store.Load(); !st.Seen[x.srv.URL][3].Equal(at(-30 * time.Second)) {
		t.Errorf("leaving the session kept the mark %v, want the update it showed", st.Seen[x.srv.URL][3])
	}

	// A session opened under its agent stays chosen there.
	x.click(t, "Homewiki: garden plan, finished, not read")
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 4 })
	if k, section, ok := x.a.sidebar.Selected(); !ok || k != state.SessionKey(4) || section != state.SectionTeam {
		t.Errorf("chose %+v in %q (%v), want session 4 in the team, where it was clicked", k, section, ok)
	}
	// Going to a place from elsewhere chooses it.
	x.a.goBoard()
	x.settle()
	if k, _, _ := x.a.sidebar.Selected(); k != state.PlaceKey(sidebarBoard) {
		t.Errorf("the board shows, and the sidebar chose %+v", k)
	}

	// The filter lists what it finds, in place of the sections.
	x.typeInto(t, "Filter sessions", "desk")
	x.hasAll(t, "Sessions", "Clerk: Desk batch", "Odrade: desk sweep")
	for _, gone := range []string{"Team", "Recent", "Plan the Q4 roadmap"} {
		if slices.Contains(x.tt.Texts(), gone) {
			t.Errorf("the filter shows %q", gone)
		}
	}
	x.snapshot(t, "sidebar-filter")
}

// sessionsShown counts the rows that show title, in every section.
func (x *testApp) sessionsShown(title string) int {
	n := 0
	for _, s := range x.tt.Texts() {
		if s == title {
			n++
		}
	}
	return n
}

// A click on an agent's row unfolds its sessions and a second click folds
// them, as its arrow does (the owner's choice); the row the main pane
// shows stays chosen.
func TestAClickOnAnAgentsRowFoldsItsSessions(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.connect(t)
	x.untilProfiles(t)
	odrade := state.GroupKey("odrade")
	if x.a.sidebar.IsOpen(state.SectionTeam, odrade) || x.sessionsShown("Odrade: desk sweep") != 1 {
		t.Fatalf("Odrade's sessions show before a click: %q", x.tt.Texts())
	}
	x.click(t, "Odrade, clerk")
	if !x.a.sidebar.IsOpen(state.SectionTeam, odrade) || x.sessionsShown("Odrade: desk sweep") != 2 {
		t.Errorf("a click left Odrade's sessions folded: %q", x.tt.Texts())
	}
	if k, _, _ := x.a.sidebar.Selected(); k != state.PlaceKey(sidebarInbox) || x.a.place != placeInbox {
		t.Errorf("the click chose %+v and the window shows %v; want the Inbox kept", k, x.a.place)
	}
	x.click(t, "Odrade, clerk")
	if x.a.sidebar.IsOpen(state.SectionTeam, odrade) || x.sessionsShown("Odrade: desk sweep") != 1 {
		t.Errorf("a second click left Odrade's sessions unfolded: %q", x.tt.Texts())
	}

	// The arrow unfolds them once, not again by the click on the row.
	title, ok := x.tt.Find("Odrade")
	if !ok {
		t.Fatalf("no Odrade in %q", x.tt.Texts())
	}
	x.tt.ClickAt(title.X-36, title.Y+title.H/2)
	x.settle()
	if !x.a.sidebar.IsOpen(state.SectionTeam, odrade) {
		t.Error("a click on the arrow left Odrade's sessions folded")
	}
}

// A click that folds the group of the session chosen keeps it folded: the
// choice goes to the session in Recent, and choosing it again does not
// unfold the group. A click on an agent with no session moves no choice
// (Codex review of #40, round 1).
func TestAFoldThatHidesTheChosenSessionStaysFolded(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.connect(t)
	x.untilProfiles(t)
	odrade := state.GroupKey("odrade")
	x.click(t, "Odrade, clerk")
	x.click(t, "Clerk: Desk batch, finished, not read") // under Odrade, drawn before Recent's
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 3 })
	if k, section, _ := x.a.sidebar.Selected(); k != state.SessionKey(3) || section != state.SectionTeam {
		t.Fatalf("chose %+v in %q, want session 3 under Odrade", k, section)
	}
	x.click(t, "Odrade, clerk")
	if x.a.sidebar.IsOpen(state.SectionTeam, odrade) {
		t.Error("folding the group of the session chosen unfolded it again")
	}
	if k, section, _ := x.a.sidebar.Selected(); k != state.SessionKey(3) || section != state.SectionRecent {
		t.Errorf("chose %+v in %q, want session 3 in Recent", k, section)
	}
	x.click(t, "Irulan")
	if k, section, _ := x.a.sidebar.Selected(); k != state.SessionKey(3) || section != state.SectionRecent || x.a.st.Data.OpenID() != 3 {
		t.Errorf("a click on Irulan, who has no session, chose %+v in %q", k, section)
	}
}

// A session no row shows, opened from elsewhere, leaves no row chosen: the
// Work Board's row, chosen before, would stand for it, and a click on it
// would not go back to the board (Codex review of #40, round 1).
func TestASessionNoRowShowsLeavesNoRowChosen(t *testing.T) {
	x := newTestApp(t)
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	for i := range state.RecentSessions + 1 {
		x.srv.AddSession(proto.Session{ID: int64(i + 1), Title: "task " + itoa(i+1), Cwd: "/home/team/projects/nsl", Model: model,
			State: proto.SessionIdle, UpdatedAt: at(-time.Duration(i) * time.Minute)})
	}
	x.connect(t)
	x.click(t, "Work Board")
	old := int64(state.RecentSessions + 1)
	x.a.openSession(old)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	if k, _, chosen := x.a.sidebar.Selected(); chosen {
		t.Errorf("session %d, which no row shows, left %+v chosen", old, k)
	}
	x.click(t, "Work Board")
	if x.a.place != placeBoard {
		t.Errorf("a click on Work Board left the window on %v", x.a.place)
	}
}

// The arrow keys browse the sessions: one chosen shows, and the keyboard
// focus stays in the list, so the next key chooses the next session. A
// click puts it in the session's message field (Codex review of #40,
// round 1).
func TestTheArrowKeysBrowseSessionsWithoutLeavingTheList(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.connect(t)
	x.click(t, "Inbox, 2 things wait for you")
	// Work Board, Terminal, You, then sessions 1 and 2, newest first.
	for range 5 {
		x.tt.Key(0, ui.KeyDown)
		x.settle()
	}
	if got := x.a.st.Data.OpenID(); got != 2 {
		t.Fatalf("five Downs from the Inbox show session %d, want 2", got)
	}
	if x.tt.Focused("Message") {
		t.Error("choosing a session with the keys put the focus in its message field")
	}
	x.click(t, "Draft the release notes, finished, not read")
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.settle()
	if !x.tt.Focused("Message") {
		t.Error("a click on a session left the focus out of its message field")
	}
}

// When the profiles failed to load, the sidebar says so and offers Try
// Again, and no agent shows as gone from them (Codex review of #40, round
// 1).
func TestTheSidebarSaysWhenTheProfilesFailedToLoad(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.srv.FailOnce(http.MethodGet, proto.PathProfiles, http.StatusBadGateway)
	x.connect(t)
	x.until(t, func() bool {
		status, _ := x.a.st.Data.ProfilesStatus()
		return status == state.ListFailed && x.a.st.InFlight() == 0
	})
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "Loading the agents' profiles failed") {
		t.Errorf("no failure in %q", x.tt.Texts())
	}
	if g, _ := x.a.sidebarData().Group("duncan"); g.Unlisted {
		t.Error("Duncan shows as gone from profiles that did not load")
	}
	x.click(t, "Try Again")
	x.untilProfiles(t)
	x.hasAll(t, "Duncan, 1 waiting for you")
	if strings.Contains(strings.Join(x.tt.Texts(), " "), "Loading the agents' profiles failed") {
		t.Error("the failure still shows after the profiles loaded")
	}
}

// An agent's row says on hover who it is and where it works, as pocket's.
func TestAnAgentsRowShowsWhereItWorksOnHover(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.connect(t)
	x.untilProfiles(t)
	at, ok := x.tt.Find("Duncan")
	if !ok {
		t.Fatalf("no Duncan in %q", x.tt.Texts())
	}
	x.tt.Move(at.X+at.W/2, at.Y+at.H/2)
	x.tt.Advance(2 * time.Second)
	x.tt.Frame()
	if !strings.Contains(strings.Join(x.tt.Texts(), "\n"), "frostyard/snow") {
		t.Errorf("hovering Duncan shows no repositories: %q", x.tt.Texts())
	}
}

// A session list that failed to load says so and offers Try Again, and
// the sidebar says there is no session only once the list loaded.
func TestASessionListThatFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.srv.FailOnce(http.MethodGet, proto.PathSessions, http.StatusBadGateway)
	x.connect(t)
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "Loading the sessions failed") {
		t.Errorf("no failure in %q", x.tt.Texts())
	}
	if x.tt.HasText("No sessions yet. Start one with New Session.") {
		t.Error("the sidebar says there is no session though the list did not load")
	}
	x.click(t, "Try Again")
	x.until(t, func() bool {
		status, _ := x.a.st.Data.SessionsStatus()
		return status == state.ListLoaded && x.a.st.InFlight() == 0
	})
	x.hasAll(t, "No sessions yet. Start one with New Session.")
}

// Signing out forgets the sidebar's filter and open groups: they belong to
// the sign-in.
func TestSigningOutResetsTheSidebar(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.connect(t)
	x.untilProfiles(t)
	x.a.sidebar.SetOpen(state.SectionTeam, state.GroupKey("odrade"), true)
	x.a.filter = "desk"
	x.a.signOut()
	x.settle()
	if x.a.filter != "" || x.a.sidebar.IsOpen(state.SectionTeam, state.GroupKey("odrade")) {
		t.Errorf("the sidebar kept filter %q and Odrade open after signing out", x.a.filter)
	}
	if _, ok := x.tt.AccessByRole(ui.RoleTextField, "Device token"); !ok {
		t.Fatal("the sign-in screen does not show")
	}
}
