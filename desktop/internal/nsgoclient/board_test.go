package nsgoclient

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
)

// groupOf is the selector of a group of the board, by its mover's id.
func groupOf(id string) string { return "id:board-group-" + id }

// The board groups its threads by who moves them next, in the order of
// state.Movers, each group with its count and the closed one folded, as
// TestTheWorkBoardSnapshot holds for the MyGo client. A group the owner
// folded stays folded when a thread changes, and a thread whose state
// changes who moves it goes to that mover's group.
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
	s.AddThread(proto.Thread{ID: 45, Agent: "builder", Title: "Verify signing access", Task: "Waiting for access to the release signing key.", State: proto.ThreadBlocked,
		Shift: &proto.Shift{Stop: "the release is signed", MaxRounds: 6, Round: 2, State: proto.ShiftPaused, Why: "the owner wrote to its session"}})
	s.AddThread(proto.Thread{ID: 42, Agent: "builder", Session: 3, State: proto.ThreadOpen, Title: "Package the browser client", Task: "Build the WebAssembly bundle for the release candidate."})
	review := proto.Thread{ID: 41, Agent: "reviewer", Supervisor: 1, State: proto.ThreadDone, Title: "Review the storage migration", Task: "Check the migration against the previous release.",
		Report: "Migration results\n\n" + strings.Repeat("- Round trip verified.\n", 32) + "\nAll 32 round trips passed."}
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
	// The four groups, in the order of who moves next, each with its
	// count and its threads.
	var above core.Points = -1
	for _, group := range []struct {
		id, title, count string
		threads          []int64
	}{
		{"owner", "Waits for you", "1", []int64{45}},
		{"specialists", "With the specialists", "3", []int64{42, 43, 44}},
		{"supervisors", "With the supervisors", "1", []int64{41}},
		{"closed", "Closed", "15", nil},
	} {
		frame := app.Find(groupOf(group.id)).Bounds()
		if frame.Origin.Y <= above {
			t.Fatalf("%s does not stand under the group before it", group.title)
		}
		above = frame.Origin.Y
		app.Find(groupOf(group.id) + " >> label:" + group.title)
		app.Find(groupOf(group.id) + ` >> text:"` + group.count + `"`)
		for _, id := range group.threads {
			app.Find(groupOf(group.id) + " >> " + card(id))
		}
	}
	// A card is as wide as its group, and the groups stand one under
	// another.
	if group, first := app.Find(groupOf("specialists")).Bounds(), app.Find(card(42)).Bounds(); first.Size.Width != group.Size.Width {
		t.Errorf("a card is %v wide in a group %v wide", first.Size.Width, group.Size.Width)
	}
	if app.Exists("text:/Archived change/") || app.Exists("text:All 32 round trips passed.") {
		t.Fatal("the closed group and a long report's rest should start folded")
	}
	app.Screenshot("work-board")
	app.Press("label:With the specialists")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Check the desktop release") })
	app.Find("text:Review the storage migration")
	app.Press(card(41) + " >> text:Show the Whole Report")
	app.WaitFor("text:All 32 round trips passed.", 3*time.Second)
	review.Title = "Review the completed migration"
	s.Emit(proto.Event{Type: proto.EventThread, Thread: &review})
	app.WaitFor("text:Review the completed migration", 5*time.Second)
	app.Find("text:All 32 round trips passed.")
	if app.Exists("text:Check the desktop release") {
		t.Fatal("refresh reopened a folded section")
	}
	app.Press(card(41) + " >> text:Show Less")
	app.Press("label:With the supervisors")
	app.Press("label:Closed")
	app.WaitFor("text:1–12 of 15", 3*time.Second)
	app.Press("label:Next")
	app.WaitFor("text:13–15 of 15", 3*time.Second)
	app.Press("label:Closed")
	app.Press("label:With the specialists")
	app.WaitFor(groupOf("specialists")+" >> text:Check the desktop release", 3*time.Second)
	// Its specialist reports it done: its supervisor moves it next.
	thread.State = proto.ThreadDone
	s.Emit(proto.Event{Type: proto.EventThread, Thread: &thread})
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Check the desktop release") })
	app.Find(groupOf("specialists") + ` >> text:"2"`)
	app.Find(groupOf("supervisors") + ` >> text:"2"`)
	app.Press("label:With the supervisors")
	app.WaitFor(groupOf("supervisors")+" >> text:Check the desktop release", 3*time.Second)
	thread.State = proto.ThreadClosed
	s.Emit(proto.Event{Type: proto.EventThread, Thread: &thread})
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Check the desktop release") })
	app.Press("label:Closed")
	app.WaitFor("text:1–12 of 16", 3*time.Second)
	// In a narrow window a card keeps its buttons inside it: they wrap.
	app.Resize(390, 730)
	inside := func(part string) bool {
		box, b := app.Find(card(42)).Bounds(), app.Find(card(42)+" >> "+part).Bounds()
		return box.Size.Width < 390 && b.Origin.X >= box.Origin.X && b.Origin.X+b.Size.Width <= box.Origin.X+box.Size.Width
	}
	for _, part := range []string{"text:Open builder's Session", "text:Close Thread", `text:"open"`} {
		app.WaitUntil(3*time.Second, func() bool { return inside(part) })
	}
}

// A thread goes to another group when who moves it next changes, with no
// change of its own state: an ask of it takes it to the owner's group,
// where its card offers the Inbox, and the ask's end takes it back
// (state.Data.NextMover).
func TestAThreadMovesGroupWhenWhoMovesItNextChanges(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	s.AddSession(proto.Session{ID: 9, Title: "T4 · Fix the flaky tests", Profile: "vera"})
	s.AddThread(proto.Thread{ID: 4, Title: "Fix the flaky tests", Agent: "vera", Session: 9, State: proto.ThreadOpen,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor(groupOf("specialists")+" >> "+card(4), 5*time.Second)
	if app.Exists(groupOf("owner")) || app.Exists(groupOf("supervisors")) || app.Exists(groupOf("closed")) {
		t.Error("a group with no thread shows")
	}
	if app.Exists("text:Open the Inbox") {
		t.Error("a thread that does not wait for the owner offers the Inbox")
	}
	ask := proto.Ask{ID: 6, Thread: 4, SessionID: 9, Question: "Skip the arm64 job?", State: proto.AskOpen}
	s.Emit(proto.Event{Type: proto.EventAsk, Ask: &ask})
	app.WaitFor(groupOf("owner")+" >> "+card(4)+` >> text:"A6 waits for your answer."`, 5*time.Second)
	app.Find(groupOf("owner") + ` >> text:"1"`)
	if app.Exists(groupOf("specialists")) {
		t.Error("the thread's old group still shows, with no thread")
	}
	// One ask: Open the Inbox is the way to it, and goes to its card.
	if app.Exists("text:View ask A6") {
		t.Error("a thread with one ask offers a second way to it")
	}
	app.Press(card(4) + " >> text:Open the Inbox")
	app.WaitFor("label:Answer to A6", 5*time.Second)
	app.Press("label:Back")
	app.WaitFor(groupOf("owner")+" >> "+card(4), 5*time.Second)
	ask.State = proto.AskWithdrawn
	s.Emit(proto.Event{Type: proto.EventAsk, Ask: &ask})
	app.WaitFor(groupOf("specialists")+" >> "+card(4)+` >> text:"Vera works on it."`, 5*time.Second)
	if app.Exists(groupOf("owner")) || app.Exists("text:Open the Inbox") {
		t.Error("a thread whose ask ended still waits for the owner")
	}
}

// A thread's report shows on its card with no press. One that folds
// (state.ReportFolds) shows its first 4 lines and Show the Whole Report,
// which opens it and then closes it as Show Less; a short one shows whole,
// with no button. TestTheWorkBoardSnapshot and
// TestAShortReportThatWrapsShowsWhole hold this for the MyGo client.
func TestAThreadsReportShowsOnItsCard(t *testing.T) {
	s := pocketdtest.New()
	seedTeam(s)
	app := signedInApp(t, s, 1230, 1100)
	app.Press("sidebar-panel >> text:Work board")
	// A short report, whole.
	app.WaitFor(card(1)+` >> text:"The job times out on the 2-year-old runner image."`, 5*time.Second)
	if app.Exists(card(1)+" >> text:Show the Whole Report") || app.Exists(card(1)+" >> text:Show Less") {
		t.Error("a short report offers to unfold")
	}
	// A report of 5 lines: 4 of them, then the button.
	app.Find(card(2) + ` >> text:"Pinned the runner to ubuntu-24.04 and raised the timeout to 30 minutes."`)
	app.Find(card(2) + " >> text:/One flaked on the network/")
	if app.Exists("text:/Nothing else changed/") {
		t.Fatal("a report that folds shows its fifth line before the owner unfolds it")
	}
	app.Screenshot("work-board-report")
	app.Press(card(2) + " >> text:Show the Whole Report")
	app.WaitFor(card(2)+" >> text:/Nothing else changed/", 3*time.Second)
	if app.Exists(card(2) + " >> text:Show the Whole Report") {
		t.Error("an unfolded report still offers to unfold")
	}
	app.Press(card(2) + " >> text:Show Less")
	app.WaitForGone("text:/Nothing else changed/", 3*time.Second)
	app.Find(card(2) + " >> text:Show the Whole Report")
}

// A report cut inside a fenced code block keeps the rest of its card as
// it is: the button, the pull request and the thread's actions follow it.
func TestAReportCutInsideACodeBlockKeepsItsCard(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	s.AddThread(proto.Thread{ID: 4, Title: "Bump Go to 1.27", Agent: "vera", Supervisor: 1, State: proto.ThreadDone,
		Report: "Ran the gates:\n\n```sh\nmake verify\nmake race\nmake ci\n```\n\nAll three passed.", CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor(card(4)+" >> text:/make verify/", 5*time.Second)
	if app.Exists("text:/make race/") || app.Exists("text:All three passed.") {
		t.Fatal("the report shows more than its first 4 lines")
	}
	report, button, close := app.Find("id:report-4").Bounds(), app.Find(card(4)+" >> text:Show the Whole Report").Bounds(), app.Find(card(4)+" >> text:Close Thread").Bounds()
	if button.Origin.Y < report.Origin.Y || close.Origin.Y < report.Origin.Y+report.Size.Height {
		t.Errorf("the report at %v, its button at %v, Close Thread at %v: want the button in the report's box and the actions under it", report, button, close)
	}
	app.Press(card(4) + " >> text:Show the Whole Report")
	app.WaitFor(card(4)+" >> text:All three passed.", 3*time.Second)
	app.Find(card(4) + " >> text:/make ci/")
}

func TestWorkBoardPlanningSections(t *testing.T) {
	s := pocketdtest.New()
	s.SetSchedules(proto.Schedules{Schedules: []proto.Schedule{{ID: "weekly-review", When: "09:00", Message: "Review open changes."}}})
	s.AddProposal(proto.Proposal{ID: 3, Rev: 1, State: proto.ProposalProposed, Title: "Check the release notes", Task: "Compare the notes with merged changes."})
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Work board")
	app.WaitFor("text:Check the release notes", 5*time.Second)
	app.Find("text:weekly-review")
	app.Press("label:Schedules")
	app.Clear("label:Until time")
	app.Type("label:Until time", "17:30")
	app.Press("text:Pick")
	app.WaitFor("text:Unpick", 3*time.Second)
	app.Press("label:Night shift")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("label:Until time") })
	app.Press("label:Night shift")
	if got := app.Find("label:Until time").Text; got != "17:30" {
		t.Fatalf("the launch's end after reopening = %q", got)
	}
	app.Find("text:Unpick")
	if app.Exists("text:weekly-review") {
		t.Fatal("changing a proposal reopened another section")
	}
}
