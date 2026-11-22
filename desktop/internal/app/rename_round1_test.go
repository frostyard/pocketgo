package app

import (
	"net/http"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// Rename… on a row of a session the main pane does not show shows that
// session, as choosing the row would: the row the rename leaves chosen
// stands for the session shown (Codex review of #118, round 1).
func TestRenameFromTheSidebarShowsItsSession(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.seedChani()
	x.connect(t)
	x.untilProfiles(t)
	x.a.openSession(4)
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 4 })
	x.rightClick(t, "Session 21")
	x.chooseMenuItem(t, "Rename…")
	x.settle()
	if !x.a.sidebar.Renaming() || x.a.place != placeSession || x.a.st.Data.OpenID() != 21 {
		t.Fatalf("Rename… shows session %d (place %v), renaming %v", x.a.st.Data.OpenID(), x.a.place, x.a.sidebar.Renaming())
	}
	x.tt.Key(ui.Cmd, ui.KeyA)
	x.tt.Type("Chani: reading list")
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return !x.a.st.Renaming(21).Busy && len(x.renames(t, 21)) == 1 })
	if k, _, chosen := x.a.sidebar.Selected(); !chosen || k != state.SessionKey(21) || x.a.st.Data.OpenID() != 21 {
		t.Fatalf("the row chosen is %+v (%v), the session shown %d", k, chosen, x.a.st.Data.OpenID())
	}
}

// No edit of a session's title starts while a rename of it is on its way,
// by any of its ways: the header's double click and button, the sidebar's
// F2 and Rename… (Codex review of #118, round 1).
func TestNoEditStartsWhileARenameIsOnItsWay(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.seedChani()
	x.connect(t)
	x.untilProfiles(t)
	x.a.openSession(21)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	release := x.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathTitle, 21))
	x.a.st.Rename(21, "Garden plan")
	x.settle()
	x.a.startRename(21)
	if x.a.titleEdit.Editing(21) {
		t.Error("the header edits the title while a rename is on its way")
	}
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Rename this session"); !ok || !b.Disabled {
		t.Errorf("the header's Rename while a rename is on its way: %+v %v", b, ok)
	}
	x.a.sidebar.StartRename(state.SectionRecent, state.SessionKey(21))
	if x.a.sidebar.Renaming() {
		t.Error("the sidebar edits the title while a rename is on its way")
	}
	x.a.renameRow(state.SessionKey(21), state.SectionRecent)
	if x.a.sidebar.Renaming() || x.a.titleEdit.Editing(21) {
		t.Error("Rename… edits the title while a rename is on its way")
	}
	release()
	x.until(t, func() bool { return !x.a.st.Renaming(21).Busy })
	x.a.startRename(21)
	if !x.a.titleEdit.Editing(21) {
		t.Error("no edit starts once the rename is in")
	}
}

// An edit of the title ends, sending nothing, once pocketd no longer says
// the session may be renamed: Enter then renames nothing (Codex review of
// #118, round 1).
func TestAnEditEndsWhenItsRenameIsWithdrawn(t *testing.T) {
	x := newTestApp(t)
	x.seedChani()
	x.connect(t)
	x.a.openSession(21)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.click(t, "Rename this session")
	x.tt.Type(" draft")
	x.settle()
	s, _ := x.a.st.Data.Session(21)
	s.Renamable = false
	x.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
	x.until(t, func() bool { return !x.a.st.Data.Renamable(21) })
	x.settle()
	if x.a.titleEdit.Editing(21) {
		t.Fatal("the field stayed once the rename was withdrawn")
	}
	x.tt.Key(0, ui.KeyEnter)
	x.a.goInbox()
	x.settle()
	if got := x.renames(t, 21); len(got) != 0 {
		t.Fatalf("a withdrawn edit sent %q", got)
	}
}
