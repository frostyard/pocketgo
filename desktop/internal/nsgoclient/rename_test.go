package nsgoclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// renames are the renames of session id that went to pocketd.
func renames(s *pocketdtest.Server, id int64) int {
	return len(s.RequestsTo(http.MethodPost, proto.Path(proto.PathTitle, id)))
}

// TestASessionRenamesFromItsSidebarRow checks the row's menu: Rename…
// shows the session, as choosing its row would, and turns its title into
// a field, which stays, with what was typed, while pocketd's events change
// the window; Enter renames the session. It holds for the NSGo client
// what TestRenameFromTheSidebarShowsItsSession holds for the MyGo client.
func TestASessionRenamesFromItsSidebarRow(t *testing.T) {
	s := pocketdtest.New()
	seedChani(s)
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("sidebar-panel >> text:Session 21", 5*time.Second)
	app.RightClick("sidebar-panel >> text:Session 21")
	app.WaitFor("role:menuitem Pinned", 5*time.Second)
	app.Screenshot("sidebar-row-menu")
	app.Press("role:menuitem Rename…")
	// The session shows, with its title a field.
	app.WaitFor("label:Message", 5*time.Second)
	app.WaitFor("id:session-header >> label:Session title", 5*time.Second)
	app.Clear("label:Session title")
	app.Type("label:Session title", "Chani: reading list")
	// Another session arrives: the window changes around the field.
	arrived := proto.Session{ID: 30, Title: "Arrived since", Cwd: "/work", Model: opus, State: proto.SessionIdle, UpdatedAt: at(-time.Second)}
	s.AddSession(arrived)
	s.Emit(proto.Event{Type: proto.EventSession, Session: &arrived})
	app.WaitFor("sidebar-panel >> text:Arrived since", 5*time.Second)
	if got := app.Find("id:session-header >> label:Session title").Text; got != "Chani: reading list" {
		t.Fatalf("after the window changed the field holds %q", got)
	}
	if renames(s, 21) != 0 {
		t.Fatal("the window's change renamed the session")
	}
	app.Key("enter")
	app.WaitFor("id:session-header >> text:Chani: reading list", 5*time.Second)
	app.WaitForGone("label:Session title", 5*time.Second)
	if n := renames(s, 21); n != 1 {
		t.Errorf("%d renames went", n)
	}
}

// TestF2RenamesTheSessionShownAndEscapeGivesUp checks F2, which turns the
// shown session's title into a field, and Escape, which sends nothing.
func TestF2RenamesTheSessionShownAndEscapeGivesUp(t *testing.T) {
	s := pocketdtest.New()
	seedChani(s)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Session 21")
	app.Key("f2")
	app.WaitFor("id:session-header >> label:Session title", 5*time.Second)
	app.Clear("label:Session title")
	app.Type("label:Session title", "Never sent")
	app.Key("escape")
	app.WaitForGone("label:Session title", 5*time.Second)
	app.Find("id:session-header >> text:Session 21")
	if n := renames(s, 21); n != 0 {
		t.Errorf("%d renames went after Escape", n)
	}
	// A session pocketd names has no rename: F2 does nothing on it.
	openSession(app, "Homewiki: garden plan")
	app.Key("f2")
	app.Advance(time.Second)
	if app.Exists("label:Session title") {
		t.Error("F2 renames a session that may not be renamed")
	}
}

// TestTheTitleRenamesBesideIt checks the pencil beside the open session's
// title and a double click on the title: the title becomes a field there,
// and the focus leaving it renames the session. A session that may not be
// renamed has no pencil.
func TestTheTitleRenamesBesideIt(t *testing.T) {
	s := pocketdtest.New()
	seedChani(s)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Session 21")
	app.Screenshot("session-rename-pencil")
	app.Press("label:Rename this session")
	app.WaitFor("id:session-header >> label:Session title", 5*time.Second)
	if app.Exists("label:Rename this session") {
		t.Error("the pencil shows while the title is a field")
	}
	app.Clear("label:Session title")
	app.Type("label:Session title", "Chani: reading list")
	app.Screenshot("session-rename-title")
	// The focus leaves the field for the message box.
	app.Press("label:Message")
	app.WaitFor("id:session-header >> text:Chani: reading list", 5*time.Second)
	if n := renames(s, 21); n != 1 {
		t.Errorf("%d renames went", n)
	}
	// A double click on the title starts an edit too; one that changes
	// nothing sends nothing.
	app.DoublePress("id:session-header >> text:Chani: reading list")
	app.WaitFor("id:session-header >> label:Session title", 5*time.Second)
	app.Key("enter")
	app.WaitForGone("label:Session title", 5*time.Second)
	if n := renames(s, 21); n != 1 {
		t.Errorf("%d renames went, the unchanged title among them", n)
	}
	openSession(app, "Homewiki: garden plan")
	if app.Exists("label:Rename this session") {
		t.Error("a session that may not be renamed has the pencil")
	}
}

// TestASessionPinsFromItsRowsMenu checks the row's menu on a session
// pocketd names: it has Pinned and no Rename…, and Pinned moves the
// session to the Pinned group and back.
func TestASessionPinsFromItsRowsMenu(t *testing.T) {
	s := pocketdtest.New()
	seedChani(s)
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("sidebar-panel >> text:Homewiki: garden plan", 5*time.Second)
	if app.Exists("sidebar-panel >> text:Pinned") {
		t.Fatal("a Pinned group shows with no pin")
	}
	app.RightClick("sidebar-panel >> text:Homewiki: garden plan")
	app.WaitFor("role:menuitem Pinned", 5*time.Second)
	if app.Exists("role:menuitem Rename…") {
		t.Error("a session that may not be renamed has Rename… in its menu")
	}
	app.Press("role:menuitem Pinned")
	app.WaitFor("sidebar-panel >> text:Pinned", 5*time.Second)
	app.Screenshot("sidebar-pinned")
	app.RightClick("sidebar-panel >> text:Homewiki: garden plan")
	app.Press("role:menuitem Pinned")
	app.WaitForGone("sidebar-panel >> text:Pinned", 5*time.Second)
}

// TestAPhoneRenamesBesideTheTitle checks a compact window: Rename… of a
// row's menu shows the session with its title a field in the top bar, and
// Cancel, for a keyboard with no Escape, sends nothing.
func TestAPhoneRenamesBesideTheTitle(t *testing.T) {
	s := pocketdtest.New()
	seedChani(s)
	app := phoneApp(t, s)
	app.SelectTab(tabBar, tabSessions)
	app.WaitFor("text:Session 21", patience())
	app.LongPress("text:Session 21")
	app.Press("role:menuitem Rename…")
	app.WaitFor("id:compact-bar >> label:Session title", patience())
	app.Clear("label:Session title")
	app.Type("label:Session title", "Never sent")
	app.Press("role:button Cancel")
	app.WaitForGone("label:Session title", patience())
	app.Find("id:compact-bar >> text:Session 21")
	if n := renames(s, 21); n != 0 {
		t.Errorf("%d renames went after Cancel", n)
	}
	app.Press("label:Rename this session")
	app.WaitFor("id:compact-bar >> label:Session title", patience())
	app.Clear("label:Session title")
	app.Type("label:Session title", "Chani: reading list")
	app.Key("enter")
	app.WaitFor("id:compact-bar >> text:Chani: reading list", patience())
	if n := renames(s, 21); n != 1 {
		t.Errorf("%d renames went", n)
	}
}
