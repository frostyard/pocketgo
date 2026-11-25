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
	// The row is named by its title and its agent; its title is pressed.
	app.Press("sidebar-panel >> text:Draft announcement")
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

// TestASessionsRowSaysItsAgentAndStepsBackWhenOld checks a session's row:
// its tag names the agent it belongs to, except under that agent's own
// group; the owner's own session is tagged by its folder; a session that
// waits says for how many approvals; and one the owner saw idle long ago
// is dimmed. It holds for the NSGo client what the MyGo client's
// sidebar rows show (frostui.SourceRow's Tag, Count and Dim).
func TestASessionsRowSaysItsAgentAndStepsBackWhenOld(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade"})
	now := pocketdtest.Now
	s.AddSession(proto.Session{ID: 1, Title: "Desk batch", Profile: "odrade", State: proto.SessionWaiting, UpdatedAt: now.Add(-time.Minute)})
	s.AddSession(proto.Session{ID: 2, Title: "Odrade: desk sweep", Profile: "odrade", State: proto.SessionIdle, UpdatedAt: now.Add(-30 * 24 * time.Hour)})
	s.AddSession(proto.Session{ID: 3, Title: "Garden plan", Cwd: "/home/team/projects/homewiki", State: proto.SessionIdle, UpdatedAt: now.Add(-2 * time.Minute)})
	s.AddApproval(proto.Approval{ID: "a1", SessionID: 1, CallID: "c1", Tool: "write", State: proto.ApprovalPending, Arguments: []byte(`{"path":"a"}`)})
	s.AddApproval(proto.Approval{ID: "a2", SessionID: 1, CallID: "c2", Tool: "write", State: proto.ApprovalPending, Arguments: []byte(`{"path":"b"}`)})
	app := signedInApp(t, s, 1230, 800)
	// In Recent, a row names its agent, or the owner's folder.
	app.WaitFor("sidebar-panel >> label:/^Desk batch, Odrade, waiting/", 5*time.Second)
	app.Find("sidebar-panel >> text:Waiting 2")
	app.Find("sidebar-panel >> label:/^Garden plan, homewiki/")
	// A title that starts with the agent's name has no tag.
	app.Find("sidebar-panel >> label:/^Odrade: desk sweep($|, [^O])/")
	// The session last updated a month ago is dimmed; the fresh ones are not.
	app.Find("id:dim-session-2")
	if app.Exists("id:dim-session-1") || app.Exists("id:dim-session-3") {
		t.Error("a session updated minutes ago is dimmed")
	}
	app.Screenshot("sidebar-tags")
	// Under its agent's own group the row does not say the agent again.
	named := len(app.Query("sidebar-panel >> text:/^Odrade$/"))
	app.Press("sidebar-panel >> label:/^Odrade, /")
	app.WaitUntil(5*time.Second, func() bool { return len(app.Query("sidebar-panel >> text:Desk batch")) == 2 })
	if n := len(app.Query("sidebar-panel >> text:/^Odrade$/")); n != named {
		t.Errorf("Odrade shows %d times with its group open and %d with it closed: a row under the group names it again", n, named)
	}
}
