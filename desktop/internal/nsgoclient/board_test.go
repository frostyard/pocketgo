package nsgoclient

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestWorkBoard(t *testing.T) {
	s := pocketdtest.New()
	for i, name := range []string{"Release coordination", "Desktop verification", "Browser packaging"} {
		profile := []string{"Coordinator", "Editor", "Builder"}[i]
		status := []proto.SessionState{proto.SessionWaiting, proto.SessionRunning, proto.SessionRunning}[i]
		s.AddSession(proto.Session{ID: int64(i + 1), Title: name, Profile: profile, State: status})
	}
	thread := proto.Thread{ID: 43, Agent: "editor", Supervisor: 1, Session: 2, State: proto.ThreadOpen, Title: "Check the desktop release", Repository: "example/desktop", Task: "Verify the upgrade path and capture the results on Linux."}
	s.AddThread(thread)
	s.AddThread(proto.Thread{ID: 44, Agent: "editor", Title: "Prepare upgrade fixtures", Task: "Set up the previous release for a migration test.", State: proto.ThreadStarting})
	s.AddThread(proto.Thread{ID: 45, Agent: "builder", Title: "Verify signing access", Task: "Waiting for access to the release signing key.", State: proto.ThreadBlocked, Shift: &proto.Shift{State: proto.ShiftPaused}})
	s.AddThread(proto.Thread{ID: 42, Agent: "builder", Session: 3, State: proto.ThreadOpen, Title: "Package the browser client", Task: "Build the WebAssembly bundle for the release candidate."})
	review := proto.Thread{ID: 41, Agent: "reviewer", Supervisor: 1, State: proto.ThreadDone, Title: "Review the storage migration", Task: "Check the migration against the previous release.", Report: "Migration results\n\n" + strings.Repeat("- Round trip verified.\n", 32)}
	s.AddThread(review)
	for i := range 15 {
		s.AddThread(proto.Thread{ID: int64(100 + i), Title: fmt.Sprintf("Archived change %d", i), State: proto.ThreadClosed})
	}
	app := signedInApp(t, s, 1230, 800)
	app.Press("label:Settings")
	app.Press("text:Dark")
	app.Press("label:Close settings")
	app.Press("text:Work board")
	app.WaitFor("text:Review the storage migration", 5*time.Second)
	app.Find("label:Closed")
	if app.Exists("text:Archived change 0") || app.Exists("text:Migration results") {
		t.Fatal("archive and report should start folded")
	}
	var previousX float32
	for _, key := range []string{"starting", "open", "blocked", "done"} {
		frame := app.Find("id:board-lane-" + key).Bounds()
		if float32(frame.Origin.X) <= previousX {
			t.Fatalf("%s did not appear to the right of the previous column", key)
		}
		previousX = float32(frame.Origin.X)
	}
	app.Screenshot("work-board")
	app.Press("label:In progress")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Check the desktop release") })
	app.Find("text:Review the storage migration")
	app.Press("label:Details for T41")
	app.Press("label:Report")
	app.WaitFor("text:Migration results", 3*time.Second)
	review.Title = "Review the completed migration"
	s.Emit(proto.Event{Type: proto.EventThread, Thread: &review})
	app.WaitFor("text:Review the completed migration", 5*time.Second)
	app.Find("text:Migration results")
	if app.Exists("text:Check the desktop release") {
		t.Fatal("refresh reopened a folded section")
	}
	app.Press("label:Report")
	app.Press("label:Done")
	app.Press("label:Closed")
	app.WaitFor("text:1–12 of 15", 3*time.Second)
	app.Press("label:Next")
	app.WaitFor("text:13–15 of 15", 3*time.Second)
	app.Press("label:Closed")
	app.Press("label:In progress")
	app.WaitFor("text:Check the desktop release", 3*time.Second)
	thread.State = proto.ThreadDone
	s.Emit(proto.Event{Type: proto.EventThread, Thread: &thread})
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Check the desktop release") })
	app.Press("label:Done")
	app.WaitFor("text:Check the desktop release", 3*time.Second)
	thread.State = proto.ThreadClosed
	s.Emit(proto.Event{Type: proto.EventThread, Thread: &thread})
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Check the desktop release") })
	app.Press("label:Closed")
	app.WaitFor("text:1–12 of 16", 3*time.Second)
	app.Resize(390, 730)
	start := app.Find("id:board-lane-starting").Bounds()
	next := app.Find("id:board-lane-open").Bounds()
	if next.Origin.Y <= start.Origin.Y || next.Origin.X != start.Origin.X {
		t.Fatal("compact board did not stack its columns")
	}
}

func TestWorkBoardPlanningSections(t *testing.T) {
	s := pocketdtest.New()
	s.AddPlan(proto.Plan{ID: 4, Title: "Release plan", State: proto.PlanDrafting, Text: "Build the release candidate."})
	s.SetSchedules(proto.Schedules{Schedules: []proto.Schedule{{ID: "weekly-review", When: "09:00", Message: "Review open changes."}}})
	s.AddProposal(proto.Proposal{ID: 3, Rev: 1, State: proto.ProposalProposed, Title: "Check the release notes", Task: "Compare the notes with merged changes."})
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Work board")
	app.WaitFor("text:Check the release notes", 5*time.Second)
	app.Find("text:weekly-review")
	app.Press("label:Plans")
	app.Press("label:Schedules")
	app.Type("label:Shift end", "17:30")
	app.Press("text:Pick")
	app.WaitFor("text:Unpick", 3*time.Second)
	app.Press("label:Night shift")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("label:Shift end") })
	app.Press("label:Night shift")
	if got := app.Find("label:Shift end").Text; got != "17:30" {
		t.Fatalf("shift end after reopening = %q", got)
	}
	app.Find("text:Unpick")
	if app.Exists("text:Release plan") || app.Exists("text:weekly-review") {
		t.Fatal("changing a proposal reopened another section")
	}
}
