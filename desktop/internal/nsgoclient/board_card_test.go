package nsgoclient

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// A thread's card names its agent, not its id, says where it works and
// what its sessions used, and counts its session's changes.
func TestAThreadCardSaysWhoWorksWhereAndWhatItUsed(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	s.AddSession(proto.Session{ID: 11, Title: "T1 · Fix the flaky arm64 job", Cwd: "/home/team/projects/nsl", Profile: "vera",
		Usage: proto.Usage{Input: 9000, Output: 3000, Cost: 0.42}})
	// A worker its session started counts with it.
	s.AddSession(proto.Session{ID: 12, Title: "Worker", StartedBy: 11, Usage: proto.Usage{Input: 500, Output: 500, Cost: 0.08}})
	s.AddSession(proto.Session{ID: 13, Title: "T3 · Draft the notes", Profile: "vera", Usage: proto.Usage{Input: 9000, Output: 3000}})
	s.AddThread(proto.Thread{ID: 1, Title: "Fix the flaky arm64 job", Agent: "vera", Session: 11, State: proto.ThreadOpen,
		Repository: "frostyard/nsl", Branch: "vera/fix-the-flaky-arm64-job-t1"})
	s.AddThread(proto.Thread{ID: 2, Title: "Pin the runner image", Agent: "vera", State: proto.ThreadOpen, Repository: "frostyard/nsl"})
	s.AddThread(proto.Thread{ID: 3, Title: "Draft the notes", Agent: "vera", Session: 13, State: proto.ThreadOpen, Cwd: "/home/team/agents/vera"})
	s.AddThread(proto.Thread{ID: 4, Title: "Bump Go to 1.27", Agent: "teg", State: proto.ThreadStarting})
	s.SetChanges(proto.Changes{SessionID: 11, Repository: true, Branch: "vera/fix-the-flaky-arm64-job-t1", Added: 54, Removed: 9,
		Files: []proto.ChangedFile{
			{Path: ".github/workflows/ci.yml", Added: 6, Removed: 2, Committed: true},
			{Path: "internal/run/retry.go", Added: 30, Removed: 7, Committed: true},
			{Path: "internal/run/retry_test.go", Added: 18, New: true, Uncommitted: true},
			{Path: "docs/arm64-runner.png", Binary: true, New: true, Uncommitted: true},
		}})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	for id, says := range map[int64]string{
		// Its repository and branch, and its cost with its worker's.
		1: `"T1 · Vera · frostyard/nsl on vera/fix-the-flaky-arm64-job-t1 · $0.50"`,
		// A repository with no branch yet.
		2: `"T2 · Vera · frostyard/nsl"`,
		// No repository: the last 2 elements of its folder, and its
		// tokens when it cost nothing.
		3: `"T3 · Vera · in agents/vera · 12k tokens"`,
		// A profile the list does not name shows by its id, and a thread
		// with no folder yet says so.
		4: `"T4 · teg · its folder is not set up yet"`,
	} {
		app.WaitFor(card(id)+" >> text:"+says, 5*time.Second)
	}
	// Only a session that changed something shows a count.
	app.WaitFor(card(1)+" >> label:Changes: 4 files +54 −9", 5*time.Second)
	if app.Exists(card(3) + " >> label:/^Changes/") {
		t.Error("a thread whose session changed nothing counts its changes")
	}
	before := len(s.RequestsTo(http.MethodGet, proto.Path(proto.PathChanges, 11)))
	app.Press(card(1) + " >> label:Changes: 4 files +54 −9")
	// It opens the thread's session, and reads its changes again.
	app.WaitFor("label:Message", 5*time.Second)
	app.WaitUntil(5*time.Second, func() bool { return len(s.RequestsTo(http.MethodGet, proto.Path(proto.PathChanges, 11))) > before })
	app.Press("label:Session details")
	app.WaitFor("text:4 files +54 −9", 5*time.Second)
}

// A thread's brief shows on its card: a short one whole, and one that
// folds as a report does (state.ReportFolds) on 3 lines, with all of it in
// the card's details. A card with nothing folded has no details.
func TestAThreadsBriefShowsOnItsCard(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	brief := strings.TrimSpace(strings.Repeat("Read the failing run's log, find the test that flakes, and fix it without a retry. ", 12))
	s.AddThread(proto.Thread{ID: 4, Title: "Fix the flaky tests", Agent: "vera", State: proto.ThreadOpen, Task: brief})
	s.AddThread(proto.Thread{ID: 5, Title: "Bump Go to 1.27", Agent: "vera", State: proto.ThreadOpen, Task: "Bump go.work and run the gates."})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor(card(5)+` >> text:"Bump go.work and run the gates."`, 5*time.Second)
	if app.Exists("label:Details for T5") {
		t.Error("a card whose brief shows whole has details")
	}
	cut := app.Find(card(4) + ` >> text:"` + brief + `"`).Bounds()
	app.Press("label:Details for T4")
	app.WaitUntil(3*time.Second, func() bool { return app.Find(card(4)+` >> text:"`+brief+`"`).Bounds().Size.Height > cut.Size.Height })
	app.Press("label:Details for T4")
	app.WaitUntil(3*time.Second, func() bool { return app.Find(card(4)+` >> text:"`+brief+`"`).Bounds().Size.Height == cut.Size.Height })
}

// A thread a message reopened says so, and a thread's card names the
// thread it carries on and the one that carries it on (ADR-0061), as
// TestAReopenedThreadOnTheBoardSnapshot holds for the MyGo client.
func TestAReopenedThreadOnTheBoard(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "lucilla", Name: "Lucilla", Role: "specialist"})
	s.AddProfile(proto.Profile{ID: "murbella", Name: "Murbella", Role: "specialist"})
	reopened := at(-time.Minute)
	s.AddThread(proto.Thread{ID: 77, Title: "Take the review points on core#283", Agent: "lucilla", Supervisor: 1, Session: 9,
		Repository: "frostyard/core", Branch: "lucilla/core-283-t77", State: proto.ThreadOpen, Report: "Done at e0bfba6.",
		Successor: 84, ReopenedAt: &reopened, CreatedAt: at(-3 * time.Hour), UpdatedAt: at(-time.Minute)})
	s.AddThread(proto.Thread{ID: 84, Title: "Re-check Murbella's blocking point", Agent: "murbella", Supervisor: 1, Session: 12,
		State: proto.ThreadOpen, Follows: 77, CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-2 * time.Minute)})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor(card(77)+` >> text:"reopened"`, 5*time.Second)
	if app.Exists(card(84) + ` >> text:"reopened"`) {
		t.Error("a thread no message reopened says reopened")
	}
	app.WaitFor(card(77)+` >> text:"Carried on in T84."`, 5*time.Second)
	app.WaitFor(card(84)+` >> text:"Carries on T77."`, 5*time.Second)
	// Each state in the MyGo client's words, beside the title.
	app.Find(card(77) + ` >> text:"open"`)
}

// The owner closes a thread from the board after a confirmation that
// names the thread and says what the close does: the request goes once.
// TestTheOwnerClosesAThreadFromTheBoard holds this for the MyGo client.
func TestTheOwnerClosesAThreadFromTheBoard(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	s.AddThread(proto.Thread{ID: 4, Title: "Bump Go to 1.27", Agent: "vera", Supervisor: 1, State: proto.ThreadBlocked,
		Report: "The module proxy refuses 1.27.", CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor("text:1 thread on the desk", 5*time.Second)
	app.Find(card(4) + ` >> text:"blocked"`)
	app.Press("text:Close Thread")
	app.WaitFor(`role:alertdialog >> text:"Close T4?"`, 3*time.Second)
	app.Find(`role:alertdialog >> text:"T4 leaves the desk: Vera can no longer report on it, and a brief not started yet is withdrawn. Its sessions, its open asks and its pull requests stay."`)
	closes := func() []pocketdtest.Request {
		return s.RequestsTo(http.MethodPost, proto.Path(proto.PathCloseThread, 4))
	}
	if len(closes()) != 0 {
		t.Fatal("the close went before the owner confirmed it")
	}
	// pocketd's event reaches the client before its answer, which is
	// held: the thread is in the Closed group, and its card says the
	// close is on its way.
	release := s.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathCloseThread, 4))
	app.Press(`role:alertdialog >> role:button "Close T4"`)
	app.WaitFor("text:No thread is on the desk", 5*time.Second)
	app.Press("label:Closed")
	app.WaitFor(card(4)+` >> text:"Closing T4…"`, 5*time.Second)
	release()
	app.WaitForGone("text:Closing T4…", 5*time.Second)
	var sent proto.CloseThread
	if reqs := closes(); len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &sent) != nil || sent.RequestID == "" {
		t.Fatalf("the close sent %+v", reqs)
	}
	app.Find(groupOf("closed") + " >> " + card(4) + ` >> text:"closed"`)
	if app.Exists("text:Close Thread") {
		t.Error("a closed thread offers Close Thread")
	}
}

// Closing a plan's own thread drops its plan: the confirmation says so,
// and names the plan. TestClosingAPlanThreadSaysItDropsThePlan holds this
// for the MyGo client.
func TestClosingAPlanThreadSaysItDropsThePlan(t *testing.T) {
	s := pocketdtest.New()
	seedPlan(s)
	s.AddPlan(plan1())
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.Press("text:Close Thread")
	app.WaitFor(`role:alertdialog >> text:"Close T7 and drop P1 (Sign the ISO)?"`, 3*time.Second)
	app.Find(`role:alertdialog >> text:"T7 is where Duncan plans P1 (Sign the ISO). Closing it drops the plan for good: a task of it not delivered yet can no longer be, and its consults stop. The threads of its delivered tasks, its sessions and its asks stay."`)
	app.Find(`role:alertdialog >> role:button "Close T7 and Drop the Plan"`)
	if len(s.RequestsTo(http.MethodPost, proto.Path(proto.PathCloseThread, 7))) != 0 {
		t.Fatal("the close went before the owner confirmed it")
	}
	app.Press("role:alertdialog >> role:button Cancel")
	app.WaitForGone("role:alertdialog", 3*time.Second)
	if len(s.RequestsTo(http.MethodPost, proto.Path(proto.PathCloseThread, 7))) != 0 {
		t.Fatal("a close the owner cancelled went")
	}
}

// The owner stops a thread's shift after a confirmation that names the
// thread and says what the stop does, and resumes it: each request goes
// once (ADR-0024). TestAShiftOnTheBoardSnapshot holds this for the MyGo
// client.
func TestTheOwnerStopsAndResumesAShiftFromTheBoard(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	deadline := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	s.AddThread(proto.Thread{ID: 4, Title: "Fix the flaky tests", Agent: "vera", Supervisor: 1, Session: 9, State: proto.ThreadOpen,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute),
		Shift: &proto.Shift{Stop: "the tests pass", MaxRounds: 8, Deadline: &deadline, State: proto.ShiftRunning, Round: 3,
			Rounds: []proto.ShiftRound{{N: 1, Verdict: "continue"}, {N: 2, Verdict: "continue"}}}})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor(card(4)+" >> text:Shift running: round 3 of 8, until Sat 07:00 UTC.", 5*time.Second)
	// What it stops at and what each round found stay in its details.
	if app.Exists("text:the tests pass") {
		t.Error("a shift's stop condition shows before its details are open")
	}
	app.Press("label:Details for T4")
	app.WaitFor(card(4)+` >> text:"the tests pass"`, 3*time.Second)
	app.Find(card(4) + ` >> text:"Round 2 · continue · "`)
	if app.Exists("text:Resume Shift") {
		t.Error("a running shift offers Resume Shift")
	}
	app.Press("text:Stop Shift")
	app.WaitFor(`role:alertdialog >> text:"Stop T4's shift?"`, 3*time.Second)
	app.Find(`role:alertdialog >> text:"The shift ends: pocketd sends no more rounds, and the supervisor hears that you stopped it. A turn the agent is in goes on to its end; abort its session to cut it short. The thread stays on the desk, and you can resume the shift while it has rounds and time left."`)
	stops := func() int { return len(s.RequestsTo(http.MethodPost, proto.Path(proto.PathStopShift, 4))) }
	if stops() != 0 {
		t.Fatal("the stop went before the owner confirmed it")
	}
	// pocketd's event reaches the client before its answer, which is
	// held: the stop filed a blocked report, so the card is its
	// supervisor's to move, and says the change is on its way.
	release := s.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathStopShift, 4))
	app.Press(`role:alertdialog >> role:button "Stop the Shift"`)
	app.WaitFor(groupOf("supervisors")+" >> "+card(4)+` >> text:"Changing T4's shift…"`, 5*time.Second)
	app.WaitFor("text:Shift ended at round 3 of 8: the owner stopped it.", 5*time.Second)
	// No second change goes while one is on its way.
	if app.Enabled(card(4) + " >> role:button Resume Shift") {
		t.Error("a shift whose stop is on its way offers Resume Shift")
	}
	release()
	app.WaitForGone("text:Changing T4's shift…", 5*time.Second)
	if stops() != 1 {
		t.Fatalf("the stop went %d times", stops())
	}
	if app.Exists("text:Stop Shift") {
		t.Error("an ended shift offers Stop Shift")
	}
	app.Press("text:Resume Shift")
	app.WaitFor("text:Shift running: round 4 of 8", 5*time.Second)
	reqs := s.RequestsTo(http.MethodPost, proto.Path(proto.PathResumeShift, 4))
	var sent proto.ResumeShift
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &sent) != nil || sent.RequestID == "" {
		t.Fatalf("the resume sent %+v", reqs)
	}
}
