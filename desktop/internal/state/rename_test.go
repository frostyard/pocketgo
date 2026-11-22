package state

import (
	"net/http"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The sidebar's menu offers Rename… only on a session pocketd says may be
// renamed, a private agent's (ADR-0058): no other session's row, nested
// or not, no group's and no place's offers it, nor a session the client
// does not know.
func TestOnlyAPrivateAgentsSessionOffersRename(t *testing.T) {
	var d Data
	f := d.Begin()
	mine := session(1, "Notes", 0)
	mine.Profile, mine.Renamable = "chani", true
	team := session(2, "Fix CI", 0)
	team.Profile = "vera"
	own := session(3, "Mine", 0)
	d.SetSessions(f, []proto.Session{mine, team, own})
	if id, ok := d.RenameOf(SessionKey(1)); !ok || id != 1 || !d.Renamable(1) {
		t.Errorf("a private agent's session: %d %v", id, ok)
	}
	for _, k := range []SidebarKey{SessionKey(2), SessionKey(3), SessionKey(9), GroupKey("chani"), PlaceKey(PlaceInbox), SearchKey()} {
		if id, ok := d.RenameOf(k); ok {
			t.Errorf("row %+v offers Rename… of session %d", k, id)
		}
	}
	if d.Renamable(2) || d.Renamable(3) || d.Renamable(9) {
		t.Error("a session that is no private agent's may be renamed")
	}
}

// The header's edit sends a changed title alone, trimmed: an unchanged
// one sends nothing, an empty one asks for the default unless the session
// has it, and Escape sends nothing.
func TestTheHeadersEditSendsAChangedTitle(t *testing.T) {
	var e TitleEdit
	if _, _, send := e.Finish(); send || e.Editing(0) {
		t.Fatal("no edit sent something")
	}
	e.Start(proto.Session{ID: 4, Title: "Notes"})
	if !e.Editing(4) || e.Editing(5) || e.Draft != "Notes" {
		t.Fatalf("the edit %+v", e)
	}
	e.Draft = "  Garden plan "
	if id, title, send := e.Finish(); id != 4 || title != "Garden plan" || !send || e.Editing(4) {
		t.Fatalf("a changed title: %d %q %v", id, title, send)
	}
	e.Start(proto.Session{ID: 4, Title: "Notes"})
	e.Draft = " Notes "
	if _, _, send := e.Finish(); send {
		t.Fatal("an unchanged title went")
	}
	e.Start(proto.Session{ID: 4, Title: "Notes"})
	e.Draft = "  "
	if _, title, send := e.Finish(); title != "" || !send {
		t.Fatalf("an empty title: %q %v", title, send)
	}
	e.Start(proto.Session{ID: 4, Title: "Session 4"})
	e.Draft = ""
	if _, _, send := e.Finish(); send {
		t.Fatal("the default went to a session that has it")
	}
	e.Start(proto.Session{ID: 4, Title: "Notes"})
	e.Draft = "Beds"
	e.Cancel()
	if _, _, send := e.Finish(); send || e.Editing(4) {
		t.Fatal("a cancelled edit went")
	}
}

// A rename goes to pocketd and the session shows its title at once. A
// title over 200 bytes goes nowhere and says why. A rename that failed and
// is tried again unchanged keeps its request id; once one went, a rename
// is a new request.
func TestARenameTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	s := session(1, "Notes", 0)
	s.Profile, s.Renamable = "chani", true
	h.srv.AddSession(s)
	h.connect(t)
	rename := func(title string) string {
		t.Helper()
		h.store.Rename(1, title)
		h.loop.RunUntil(t, func() bool { return !h.store.Renaming(1).Busy })
		return h.store.Renaming(1).Err
	}
	p := proto.Path(proto.PathTitle, 1)
	if err := rename(strings.Repeat("a", MaxTitleSize+1)); !strings.Contains(err, "more than the 200 a title may have") {
		t.Fatalf("a long title: %q", err)
	}
	if n := len(h.srv.RequestsTo(http.MethodPost, p)); n != 0 {
		t.Fatalf("a long title went: %d requests", n)
	}
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	if err := rename(" Garden plan "); err == "" {
		t.Fatal("the failure was not kept")
	}
	if got, _ := h.store.Data.Session(1); got.Title != "Notes" {
		t.Fatalf("a failed rename shows %q", got.Title)
	}
	if err := rename("Garden plan"); err != "" {
		t.Fatal(err)
	}
	if got, _ := h.store.Data.Session(1); got.Title != "Garden plan" || !got.UpdatedAt.Equal(s.UpdatedAt) {
		t.Fatalf("the session shows %+v", got)
	}
	if err := rename("Beds"); err != "" {
		t.Fatal(err)
	}
	if err := rename("Garden plan"); err != "" {
		t.Fatal(err)
	}
	ids := sentIDs(t, h, p)
	if len(ids) != 4 || ids[0] != ids[1] || ids[2] == ids[1] || ids[3] == ids[1] || ids[3] == ids[2] {
		t.Fatalf("request ids %q", ids)
	}
}

// A rename whose answer was lost, and that took effect, keeps no request
// id once the session shows another title: the same title again, after
// another device renamed the session, is a new request, which pocketd
// applies. pocketd's refusal shows as the rename's error.
func TestARenameSupersededSinceIsANewRequest(t *testing.T) {
	h := newHarness(t)
	s := session(1, "Notes", 0)
	s.Profile, s.Renamable = "chani", true
	h.srv.AddSession(s)
	h.srv.AddSession(session(2, "Fix CI", 0))
	h.connect(t)
	p := proto.Path(proto.PathTitle, 1)
	h.srv.LoseAnswerOnce(http.MethodPost, p)
	h.store.Rename(1, "Garden plan")
	h.loop.RunUntil(t, func() bool { return !h.store.Renaming(1).Busy })
	if h.store.Renaming(1).Err == "" {
		t.Fatal("the lost answer was not kept")
	}
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Title == "Garden plan" })
	// Another device renames it back.
	back, _ := h.store.Data.Session(1)
	back.Title = "Notes"
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &back})
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Title == "Notes" })
	h.store.Rename(1, "Garden plan")
	h.loop.RunUntil(t, func() bool { return !h.store.Renaming(1).Busy })
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Title == "Garden plan" })
	if ids := sentIDs(t, h, p); len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("request ids %q, want two", ids)
	}
	h.store.Rename(2, "Renamed")
	h.loop.RunUntil(t, func() bool { return !h.store.Renaming(2).Busy })
	if err := h.store.Renaming(2).Err; !strings.Contains(err, "only a private agent's session may be renamed") {
		t.Fatalf("pocketd's refusal: %q", err)
	}
	if got, _ := h.store.Data.Session(2); got.Title != "Fix CI" {
		t.Fatalf("a refused rename shows %q", got.Title)
	}
}

// A rename's answer applies as the session's event would: an event that
// changed the session after the rename began wins over the answer's copy.
func TestALaterEventWinsOverARenamesAnswer(t *testing.T) {
	var d Data
	f := d.Begin()
	s := session(1, "Notes", 0)
	d.SetSessions(f, []proto.Session{s})
	began := d.Begin()
	later := s
	later.Title = "Beds"
	d.Apply(proto.Event{Type: proto.EventSession, Session: &later})
	answer := s
	answer.Title = "Garden plan"
	d.Renamed(began, answer)
	if got, _ := d.Session(1); got.Title != "Beds" {
		t.Fatalf("the answer undid a later event: %q", got.Title)
	}
}
