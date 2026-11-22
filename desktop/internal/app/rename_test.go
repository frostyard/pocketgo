package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// seedChani gives the fake pocketd a private agent, Chani, and her
// session, which pocketd says the owner may rename (ADR-0058).
func (x *testApp) seedChani() {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "chani", Name: "Chani", Role: proto.RoleAgent, Home: "/home/team/agents/chani", Cwd: "/home/team/agents/chani",
		Private: true, Runs: model})
	x.srv.AddSession(proto.Session{ID: 21, Title: "Session 21", Cwd: "/home/team/agents/chani", Model: model, Profile: "chani", Route: "profile",
		Renamable: true, State: proto.SessionIdle, UpdatedAt: at(-30 * time.Second)})
}

// renames are the titles the window sent to rename session id.
func (x *testApp) renames(t *testing.T, id int64) []string {
	t.Helper()
	var out []string
	for _, r := range x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathTitle, id)) {
		var body proto.RenameSession
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body.Title)
	}
	return out
}

// The header renames a private agent's session (ADR-0058): Rename turns
// the title into a field, Enter renames the session, whose new title shows
// in the header and the sidebar at once, and Escape gives up, sending
// nothing. A team agent's session shows no Rename, and a double click on
// its title edits nothing.
func TestTheHeaderRenamesAPrivateAgentsSessionSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.seedChani()
	x.connect(t)
	x.untilProfiles(t)
	x.a.openSession(21)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.click(t, "Rename this session")
	if !x.a.titleEdit.Editing(21) {
		t.Fatal("Rename did not edit the title")
	}
	if _, ok := x.tt.AccessByRole(ui.RoleTextField, "Session title"); !ok {
		t.Fatalf("no title field in %q", x.tt.Texts())
	}
	if _, ok := x.tt.AccessByRole(ui.RoleButton, "Rename this session"); ok {
		t.Error("Rename shows while the title is edited")
	}
	x.tt.Key(ui.Cmd, ui.KeyA)
	x.tt.Type("Chani: reading list")
	x.settle()
	if x.a.titleEdit.Draft != "Chani: reading list" {
		t.Fatalf("the field holds %q", x.a.titleEdit.Draft)
	}
	if _, ok := x.tt.AccessByRole(ui.RoleButton, "Cancel"); ok {
		t.Error("a desktop's header shows Cancel: Escape gives up")
	}
	x.snapshot(t, "session-rename-header")
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return !x.a.st.Renaming(21).Busy })
	x.settle()
	if got := x.renames(t, 21); !slices.Equal(got, []string{"Chani: reading list"}) {
		t.Fatalf("renames sent %q", got)
	}
	if x.a.titleEdit.Editing(21) {
		t.Fatal("Enter left the title edited")
	}
	x.hasAll(t, "Chani: reading list")
	if r, _ := x.a.sidebarData().Session(21); r.Title != "Chani: reading list" {
		t.Errorf("the sidebar shows %q", r.Title)
	}

	// Escape gives up.
	x.click(t, "Rename this session")
	x.a.titleEdit.Draft = "Something else"
	x.settle()
	x.tt.Key(0, ui.KeyEscape)
	x.settle()
	if x.a.titleEdit.Editing(21) || len(x.renames(t, 21)) != 1 {
		t.Fatalf("Escape: editing %v, renames %q", x.a.titleEdit.Editing(21), x.renames(t, 21))
	}

	// A team agent's session offers no rename.
	x.a.openSession(2)
	x.until(t, func() bool {
		return x.a.st.Data.Open() != nil && x.a.st.Data.Open().ID == 2 && x.a.st.Data.Open().Loaded
	})
	if _, ok := x.tt.AccessByRole(ui.RoleButton, "Rename this session"); ok {
		t.Error("a team agent's session offers Rename")
	}
	x.a.startRename(2)
	if x.a.titleEdit.Editing(2) {
		t.Error("a team agent's title is edited")
	}
}

// A double click on a private agent's session's title edits it, and the
// focus leaving the field renames it; a refused rename keeps the title and
// says why above the composer.
func TestADoubleClickOnTheTitleEditsIt(t *testing.T) {
	x := newTestApp(t)
	x.seedChani()
	x.connect(t)
	x.a.openSession(21)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	// The header's title, right of the sidebar, which shows it too.
	var r ui.Rect
	for _, n := range x.tt.AccessAll() {
		if n.Name == "Session 21" && n.Bounds.X > 300 {
			r = n.Bounds
		}
	}
	if r.W == 0 {
		t.Fatalf("no title in the header: %q", x.tt.Texts())
	}
	x.tt.ClickAt(r.X+r.W/2, r.Y+r.H/2)
	x.tt.ClickAt(r.X+r.W/2, r.Y+r.H/2)
	x.settle()
	if !x.a.titleEdit.Editing(21) {
		t.Fatal("a double click did not edit the title")
	}
	x.a.titleEdit.Draft = "Reading list"
	x.settle()
	// pocketd no longer takes it: the profile stopped being private.
	s, _ := x.srv.Session(21)
	s.Session.Renamable = false
	x.srv.AddSession(s.Session)
	// The owner leaves for the Inbox: the field leaves, and the edit ends.
	x.a.goInbox()
	x.until(t, func() bool { return !x.a.st.Renaming(21).Busy && len(x.renames(t, 21)) == 1 })
	x.a.openSession(21)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.hasAll(t, "Session 21", "Renaming the session failed: bad_request: session 21 is not a private agent's: only a private agent's session may be renamed")
}

// The sidebar renames a private agent's session in place (ADR-0058): its
// row's menu adds Rename…, which shows a field of its title, and Enter
// renames it. A team session's row offers Pinned alone, and F2 on it
// edits nothing.
func TestTheSidebarRenamesAPrivateAgentsSessionSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.seedChani()
	x.connect(t)
	x.untilProfiles(t)
	x.rightClick(t, "Session 21")
	if got := x.tt.Menu(); !slices.Equal(got, []string{"Pinned", "Rename…"}) {
		t.Fatalf("the row's menu is %q, want Pinned and Rename…", got)
	}
	x.chooseMenuItem(t, "Rename…")
	x.settle()
	if !x.a.sidebar.Renaming() {
		t.Fatal("Rename… did not show the row's field")
	}
	x.tt.Key(ui.Cmd, ui.KeyA)
	x.tt.Type("Chani: reading list")
	x.settle()
	x.snapshot(t, "sidebar-rename")
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return !x.a.st.Renaming(21).Busy && len(x.renames(t, 21)) == 1 })
	x.settle()
	if got := x.renames(t, 21); got[0] != "Chani: reading list" {
		t.Fatalf("renames sent %q", got)
	}
	x.hasAll(t, "Chani: reading list")

	x.rightClick(t, "Homewiki: garden plan")
	if got := x.tt.Menu(); !slices.Equal(got, []string{"Pinned"}) {
		t.Fatalf("a team session's menu is %q, want Pinned alone", got)
	}
	x.tt.CloseMenu()
	x.a.sidebar.StartRename(state.SectionRecent, state.SessionKey(4))
	if x.a.sidebar.Renaming() {
		t.Fatal("a team session's row renames")
	}
}

// The iPhone renames a private agent's session from its top bar
// (ADR-0058): Rename shows the field under it, in the note's place, and
// Enter renames the session. A row's Rename… in the Sessions tab opens the
// session with its field.
func TestThePhoneRenamesAPrivateAgentsSessionSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.seedChani()
	x.phoneApp(t, true, nil)
	x.tap(t, "Sessions")
	x.tap(t, "Session 21")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == 21 && o.Loaded })
	x.within(t, ui.RoleButton, "Rename this session")
	x.tap(t, "Rename this session")
	x.within(t, ui.RoleTextField, "Session title")
	x.within(t, ui.RoleButton, "Cancel")
	// Cancel gives up, with no Escape on the phone's keyboard, and the
	// field's leaving renames nothing first.
	x.tt.Type(" draft")
	x.tap(t, "Cancel")
	if x.a.titleEdit.Editing(21) || len(x.renames(t, 21)) != 0 {
		t.Fatalf("Cancel: editing %v, renames %q", x.a.titleEdit.Editing(21), x.renames(t, 21))
	}
	x.tap(t, "Rename this session")
	x.tt.Key(ui.Cmd, ui.KeyA)
	x.tt.Type("Chani: reading list")
	x.settle()
	x.snapshot(t, "phone-session-rename")
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return !x.a.st.Renaming(21).Busy && len(x.renames(t, 21)) == 1 })
	x.settle()
	x.hasAll(t, "Chani: reading list")

	x.tap(t, "Back to Sessions")
	x.rightClick(t, "Chani: reading list")
	if got := x.tt.Menu(); !slices.Equal(got, []string{"Pinned", "Rename…"}) {
		t.Fatalf("the row's menu is %q, want Pinned and Rename…", got)
	}
	x.chooseMenuItem(t, "Rename…")
	x.settle()
	if o := x.a.shownSession(); o == nil || o.ID != 21 || !x.a.titleEdit.Editing(21) {
		t.Fatalf("Rename… shows %+v, editing %v", o, x.a.titleEdit.Editing(21))
	}
	x.within(t, ui.RoleTextField, "Session title")
	x.snapshot(t, "phone-rename-from-list")
}
