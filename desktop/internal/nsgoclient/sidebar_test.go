package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestAgentSessionHierarchy(t *testing.T) {
	s := pocketdtest.New()
	for _, p := range []proto.Profile{{ID: "editor", Name: "Editor"}, {ID: "reviewer", Name: "Reviewer"}} {
		s.AddProfile(p)
	}
	for _, session := range []proto.Session{
		{ID: 1, Title: "Release notes", Profile: "editor"},
		{ID: 2, Title: "Check references", StartedBy: 1, Route: "worker-default"},
		{ID: 3, Title: "Draft announcement", Profile: "editor"},
		{ID: 4, Title: "Review the release", Profile: "reviewer"},
		{ID: 5, Title: "My checklist"},
	} {
		session.State = proto.SessionIdle
		s.AddSession(session)
	}
	app := signedInApp(t, s, 1230, 800)
	app.Press("label:Settings")
	app.Press("text:Dark")
	app.Press("label:Close settings")
	app.Press("label:Draft announcement")
	app.WaitFor("label:Message", 3*time.Second)
	app.Press("label:Recent")
	app.Press("label:Editor")
	app.Advance(time.Second)
	start := app.Find("text:Editor").Bounds().Origin.X
	child := app.Find("text:Release notes").Bounds().Origin.X
	worker := app.Find("text:Check references").Bounds().Origin.X
	if child <= start || worker <= child {
		t.Fatalf("sidebar label positions: agent %v, session %v, worker %v", start, child, worker)
	}
	app.Screenshot("agent-sessions")
	app.Type("label:Message", "Keep this draft")
	app.Press("label:Editor")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("label:Check references") })
	if app.Find("label:Message").Text != "Keep this draft" {
		t.Fatal("folding an agent discarded the active draft")
	}
	workerUpdate := proto.Session{ID: 2, Title: "Check references", StartedBy: 1, Route: "worker-default", State: proto.SessionRunning}
	s.Emit(proto.Event{Type: proto.EventSession, Session: &workerUpdate})
	app.WaitFor("label:Editor, 1 running", 3*time.Second)
	waiting := proto.Session{ID: 1, Title: "Release notes", Profile: "editor", State: proto.SessionWaiting}
	s.Emit(proto.Event{Type: proto.EventSession, Session: &waiting})
	app.WaitFor("label:Editor, 1 running, 1 waiting", 3*time.Second)
	app.Press("label:Editor, 1 running, 1 waiting")
	app.WaitFor("label:Check references, running", 3*time.Second)
	app.Find("label:Release notes, waiting")
	workerUpdate.State = proto.SessionIdle
	s.Emit(proto.Event{Type: proto.EventSession, Session: &workerUpdate})
	app.WaitFor("label:Editor, 1 waiting", 3*time.Second)
	waiting.State = proto.SessionIdle
	s.Emit(proto.Event{Type: proto.EventSession, Session: &waiting})
	app.WaitFor("label:Editor", 3*time.Second)
	app.Resize(390, 730)
	app.Press("label:Navigation")
	app.Press("label:Recent")
	app.Press("label:Reviewer")
	app.Advance(time.Second)
	app.WaitFor("label:Review the release", 3*time.Second)
	app.Press("label:Review the release")
	app.WaitFor("label:Message", 3*time.Second)
	if app.Exists("label:Filter sessions") {
		t.Fatal("choosing a session left compact navigation open")
	}
}
