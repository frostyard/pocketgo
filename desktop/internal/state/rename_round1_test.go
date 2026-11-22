package state

import (
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// No edit of a session's title starts while a rename of it is on its way:
// its end would drop the second title (Codex review of #118, round 1).
// Once the first is in, one may start again.
func TestNoSecondRenameStartsWhileOneIsOnItsWay(t *testing.T) {
	h := newHarness(t)
	s := session(1, "Notes", 0)
	s.Profile, s.Renamable = "chani", true
	h.srv.AddSession(s)
	h.srv.AddSession(session(2, "Fix CI", 0))
	h.connect(t)
	if !h.store.MayRename(1) || h.store.MayRename(2) || h.store.MayRename(9) {
		t.Fatalf("may rename: 1 %v, 2 %v, 9 %v", h.store.MayRename(1), h.store.MayRename(2), h.store.MayRename(9))
	}
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathTitle, 1))
	h.store.Rename(1, "Garden plan")
	if !h.store.Renaming(1).Busy || h.store.MayRename(1) {
		t.Fatalf("a rename on its way: %+v, may rename %v", h.store.Renaming(1), h.store.MayRename(1))
	}
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.Renaming(1).Busy })
	if !h.store.MayRename(1) {
		t.Fatal("no edit may start once the rename is in")
	}
}

// An edit ends, sending nothing, once its session may no longer be
// renamed: an event withdrew it, or the session is gone (Codex review of
// #118, round 1). One pocketd still takes stays.
func TestAnEditWhoseRenameIsWithdrawnEnds(t *testing.T) {
	var d Data
	f := d.Begin()
	s := session(1, "Notes", 0)
	s.Renamable = true
	d.SetSessions(f, []proto.Session{s})
	var e TitleEdit
	e.Start(s)
	e.Draft = "Garden plan"
	e.Check(&d)
	if !e.Editing(1) || e.Draft != "Garden plan" {
		t.Fatalf("an edit pocketd takes ended: %+v", e)
	}
	withdrawn := s
	withdrawn.Renamable = false
	d.Apply(proto.Event{Type: proto.EventSession, Session: &withdrawn})
	e.Check(&d)
	if e.Editing(1) {
		t.Fatal("an edit pocketd no longer takes stayed")
	}
	if _, _, send := e.Finish(); send {
		t.Fatal("a withdrawn edit went")
	}
	e.Start(proto.Session{ID: 9, Title: "Gone"})
	e.Check(&d)
	if e.Editing(9) {
		t.Fatal("an edit of a session the client does not know stayed")
	}
}
