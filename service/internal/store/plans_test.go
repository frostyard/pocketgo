package store

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

func allow(PlanFacts) error { return nil }

// briefOf builds a task thread's title and task from the facts the gate
// checked, as pocketd does.
func briefOf(task string) func(PlanFacts) (string, string) {
	return func(f PlanFacts) (string, string) { return task, "Build " + f.Task(task).Title }
}

func says(text string) func(PlanFacts) string { return func(PlanFacts) string { return text } }

// asking is a consult question that is text whatever the plan.
func asking(text string) func(PlanRow, *AskRow) string {
	return func(PlanRow, *AskRow) string { return text }
}

// newPlan starts a plan led by duncan for supervisor session 1, with its
// planning session 50 attached and its thread open.
func newPlan(t *testing.T, s *Store, key string) (ThreadRow, PlanRow) {
	t.Helper()
	thread, plan, existed, err := s.CreatePlanThread(ctx, ThreadRow{Key: key, Title: "Signing", Agent: "duncan", SupervisorSession: 1, Task: "Plan the signing"},
		func(int) error { return nil })
	if err != nil || existed {
		t.Fatalf("start a plan: %v %v", existed, err)
	}
	session := int64(50 + plan.ID)
	if _, err := s.AttachThread(ctx, thread.ID, session, "/home/duncan", "", "brief"); err != nil {
		t.Fatal(err)
	}
	if thread, err = s.OpenThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	return thread, plan
}

var graph = []team.Task{
	{ID: "t1", Title: "Keys", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"Keys are made"}},
	{ID: "t2", Title: "Sign", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"The ISO is signed"}, Deps: []string{"t1"}},
	{ID: "t3", Title: "Docs", Agent: "duncan", Acceptance: []string{"It is written down"}},
}

// approvedPlan is newPlan written, drafted, ready and approved.
func approvedPlan(t *testing.T, s *Store, key string) (ThreadRow, PlanFacts) {
	t.Helper()
	thread, plan := newPlan(t, s, key)
	if _, _, _, err := s.WritePlan(ctx, key+":w", plan.ID, "# Signing", nil, allow, says("written")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.DraftGraph(ctx, key+":g", plan.ID, graph, allow, says("drafted")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ReadyPlan(ctx, key+":r", plan.ID, allow, says("ready")); err != nil {
		t.Fatal(err)
	}
	facts, _, err := s.ApprovePlan(ctx, key+":approve", plan.ID, "laptop", allow)
	if err != nil {
		t.Fatal(err)
	}
	return thread, facts
}

func taskStates(f PlanFacts) map[string]string {
	out := map[string]string{}
	for _, t := range f.Graph.Tasks {
		out[t.ID] = t.State
	}
	return out
}

func TestAPlanStartsWithItsThread(t *testing.T) {
	s, _ := open(t)
	refused := errors.New("refused")
	if _, _, _, err := s.CreatePlanThread(ctx, ThreadRow{Key: "p0", Title: "x", Agent: "duncan", SupervisorSession: 1, Task: "x"},
		func(int) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("a refused start: %v", err)
	}
	if plans, _ := s.Plans(ctx); len(plans) != 0 {
		t.Fatalf("a refused start left %+v", plans)
	}
	var seen []int
	start := func(key string) (ThreadRow, PlanRow, bool) {
		thread, plan, existed, err := s.CreatePlanThread(ctx, ThreadRow{Key: key, Title: "Signing", Agent: "duncan", SupervisorSession: 1, Task: "Plan it"},
			func(open int) error { seen = append(seen, open); return nil })
		if err != nil {
			t.Fatal(err)
		}
		return thread, plan, existed
	}
	thread, plan, existed := start("p1")
	if existed || plan.State != team.PlanDrafting || plan.ThreadID != thread.ID || plan.Lead != "duncan" || plan.SupervisorSession != 1 || plan.Key != "p1" {
		t.Fatalf("plan %+v of thread %+v", plan, thread)
	}
	if !thread.IsPlanThread() || thread.Plan != plan.ID || thread.State != ThreadStarting || len(thread.Owner) != 16 {
		t.Fatalf("the plan thread %+v", thread)
	}
	again, same, existed := start("p1")
	if !existed || again.ID != thread.ID || same.ID != plan.ID {
		t.Fatalf("the same call made %+v %+v", again, same)
	}
	start("p2")
	if !slices.Equal(seen, []int{0, 1}) {
		t.Fatalf("check saw %v open plans, want 0 then 1 (a replay checks nothing)", seen)
	}
	// A plan's own thread takes no place on the desk.
	if n, _ := s.OpenThreads(ctx); n != 0 {
		t.Fatalf("plan threads on the desk: %d", n)
	}
	// A key that made a thread of other work is no plan's.
	if _, _, err := s.CreateThread(ctx, ThreadRow{Key: "other", Title: "x", Agent: "vera", SupervisorSession: 1, Task: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.CreatePlanThread(ctx, ThreadRow{Key: "other", Title: "x", Agent: "duncan", SupervisorSession: 1, Task: "x"},
		func(int) error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("a key of another thread: %v", err)
	}
}

func TestAPlanCallChangesOnce(t *testing.T) {
	s, _ := open(t)
	_, plan := newPlan(t, s, "p1")
	refused := errors.New("stale")
	if _, _, _, err := s.WritePlan(ctx, "w0", plan.ID, "# Signing", nil, func(PlanFacts) error { return refused }, says("x")); !errors.Is(err, refused) {
		t.Fatalf("a refused write: %v", err)
	}
	if got, _ := s.Plan(ctx, plan.ID); got.Text != "" {
		t.Fatalf("a refused write changed the plan: %+v", got)
	}
	if _, found, _ := s.PlanCallResult(ctx, "w0"); found {
		t.Fatal("a refused call was recorded")
	}
	facts, result, replayed, err := s.WritePlan(ctx, "w1", plan.ID, "# Signing", nil, allow, func(f PlanFacts) string { return "at " + team.ShortRev(f.Plan.Rev) })
	if err != nil || replayed || facts.Plan.Text != "# Signing" || facts.Plan.Rev != team.Rev("# Signing") || result != "at "+team.ShortRev(team.Rev("# Signing")) {
		t.Fatalf("write %+v %q %v %v", facts.Plan, result, replayed, err)
	}
	// Run again, the call answers as it did and changes nothing, whatever
	// the plan is now.
	if _, _, _, err := s.WritePlan(ctx, "w2", plan.ID, "# Signing v2", nil, allow, says("second")); err != nil {
		t.Fatal(err)
	}
	facts, again, replayed, err := s.WritePlan(ctx, "w1", plan.ID, "# Signing", nil, func(PlanFacts) error { return refused }, says("other"))
	if err != nil || !replayed || again != result || facts.Plan.Text != "# Signing v2" {
		t.Fatalf("a replay: %q %v %v, plan %q", again, replayed, err, facts.Plan.Text)
	}
	if got, found, _ := s.PlanCallResult(ctx, "w1"); !found || got != result {
		t.Fatalf("recorded %q %v", got, found)
	}
}

func TestTheGateFollowsTheAsksAndConsults(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	gate := func() []string {
		t.Helper()
		f, err := s.PlanFacts(ctx, plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		return f.Gate()
	}
	if got := gate(); len(got) != 0 {
		t.Fatalf("a new plan waits for %v", got)
	}
	_, asks, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadOpen, "Two questions", []AskRow{
		{Key: "r1:ask:0", SessionID: thread.Session, Question: "Which key?"},
		{Key: "r1:ask:1", SessionID: thread.Session, Question: "Which ISO?"}}, reportText)
	if err != nil || len(asks) != 2 {
		t.Fatal(err)
	}
	a1, a2 := asks[0], asks[1]
	if got := gate(); !slices.Equal(got, []string{"A1 open", "A2 open"}) {
		t.Fatalf("gate %v", got)
	}
	if _, _, err := s.AnswerAsk(ctx, "answer1", a1.ID, "The TPM", "laptop"); err != nil {
		t.Fatal(err)
	}
	if got := gate(); !slices.Equal(got, []string{"A2 open", "A1 not written"}) {
		t.Fatalf("gate %v", got)
	}
	// A write names the asks it writes in: only an answered ask of the
	// plan thread is written.
	if _, _, _, err := s.WritePlan(ctx, "w1", plan.ID, "# With the TPM", []int64{a1.ID, a2.ID}, allow, says("w")); err != nil {
		t.Fatal(err)
	}
	if got := gate(); !slices.Equal(got, []string{"A2 open"}) {
		t.Fatalf("gate %v", got)
	}
	if _, _, err := s.AnswerAsk(ctx, "answer2", a2.ID, "The live one", "laptop"); err != nil {
		t.Fatal(err)
	}
	if got := gate(); !slices.Equal(got, []string{"A2 not written"}) {
		t.Fatalf("a write before the answer wrote it: %v", got)
	}
	if _, _, _, err := s.WritePlan(ctx, "w2", plan.ID, "# Both", []int64{a2.ID}, allow, says("w")); err != nil {
		t.Fatal(err)
	}
	if got := gate(); len(got) != 0 {
		t.Fatalf("gate %v", got)
	}
	// A consult holds it while it starts and runs.
	facts, _, _, err := s.AddConsults(ctx, "k", plan.ID, []ConsultRow{{Key: "k:0", Agent: "vera", Question: "Holds?"}}, allow, says("asked"))
	if err != nil || len(facts.Consults) != 1 || facts.Consults[0].State != team.ConsultStarting || len(facts.Consults[0].Owner) != 16 {
		t.Fatalf("consults %+v %v", facts.Consults, err)
	}
	if got := gate(); !slices.Equal(got, []string{"K1 starting"}) {
		t.Fatalf("gate %v", got)
	}
	if _, _, err := s.EndConsult(ctx, facts.Consults[0].ID, team.ConsultFailed, "timeout", "ended"); err != nil {
		t.Fatal(err)
	}
	if got := gate(); len(got) != 0 {
		t.Fatalf("an ended consult holds the plan: %v", got)
	}
	// In review, a new ask takes the plan back to drafting.
	if _, _, _, err := s.DraftGraph(ctx, "g", plan.ID, graph, allow, says("g")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ReadyPlan(ctx, "ready", plan.ID, allow, says("ready")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Plan(ctx, plan.ID); got.State != team.PlanReview {
		t.Fatalf("ready left %s", got.State)
	}
	if _, _, _, err := s.FileReport(ctx, "r2", thread.ID, ThreadOpen, "Progress", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Plan(ctx, plan.ID); got.State != team.PlanReview {
		t.Fatalf("a report with no ask moved the plan to %s", got.State)
	}
	if _, _, _, err := s.FileReport(ctx, "r3", thread.ID, ThreadOpen, "One more", []AskRow{{Key: "r3:ask:0", SessionID: thread.Session, Question: "And?"}}, reportText); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Plan(ctx, plan.ID); got.State != team.PlanDrafting {
		t.Fatalf("a new ask left the plan %s", got.State)
	}
}

func TestADraftReplacesTheDraft(t *testing.T) {
	s, _ := open(t)
	_, plan := newPlan(t, s, "p1")
	if _, _, _, err := s.WritePlan(ctx, "w", plan.ID, "# One", nil, allow, says("w")); err != nil {
		t.Fatal(err)
	}
	f1, _, _, err := s.DraftGraph(ctx, "g1", plan.ID, graph, allow, says("g"))
	if err != nil || f1.Graph.Version != 1 || f1.Graph.State != GraphDraft || f1.DraftRev() != team.Rev("# One") || len(f1.Graph.Tasks) != 3 {
		t.Fatalf("draft %+v %v", f1.Graph, err)
	}
	if got := f1.Graph.Tasks[1]; got.ID != "t2" || !slices.Equal(got.Deps, []string{"t1"}) || got.State != team.TaskWaiting || got.Acceptance[0] != "The ISO is signed" {
		t.Fatalf("t2 %+v", got)
	}
	f2, _, _, err := s.DraftGraph(ctx, "g2", plan.ID, graph[:1], allow, says("g"))
	if err != nil || f2.Graph.Version != 2 || len(f2.Graph.Tasks) != 1 {
		t.Fatalf("second draft %+v %v", f2.Graph, err)
	}
	var superseded int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM plan_graphs WHERE plan_id = ? AND state = ?`, plan.ID, GraphSuperseded).Scan(&superseded); err != nil || superseded != 1 {
		t.Fatalf("superseded %d %v", superseded, err)
	}
	// A write after the draft leaves the draft on the old revision.
	f3, _, _, err := s.WritePlan(ctx, "w2", plan.ID, "# Two", nil, allow, says("w"))
	if err != nil || f3.DraftRev() == f3.Plan.Rev {
		t.Fatalf("the draft follows the text: %v", err)
	}
}

func TestApproveHoldsOnceForTheOwnersRequest(t *testing.T) {
	s, _ := open(t)
	_, plan := newPlan(t, s, "p1")
	if _, _, _, err := s.WritePlan(ctx, "w", plan.ID, "# Signing", nil, allow, says("w")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.DraftGraph(ctx, "g", plan.ID, graph, allow, says("g")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ReadyPlan(ctx, "r", plan.ID, allow, says("r")); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("not as seen")
	if _, _, err := s.ApprovePlan(ctx, "a0", plan.ID, "laptop", func(PlanFacts) error { return refused }); !errors.Is(err, ErrConflict) || !errors.Is(err, refused) {
		t.Fatalf("a refused approval: %v", err)
	}
	facts, replayed, err := s.ApprovePlan(ctx, "a1", plan.ID, "laptop", allow)
	if err != nil || replayed || facts.Plan.State != team.PlanApproved || facts.Plan.ApprovedBy != "laptop" || facts.Plan.ApprovedAt == nil || facts.Graph.State != GraphApproved {
		t.Fatalf("approve %+v %v %v", facts.Plan, replayed, err)
	}
	if got := taskStates(facts); got["t1"] != team.TaskReady || got["t2"] != team.TaskWaiting || got["t3"] != team.TaskReady {
		t.Fatalf("task states %v", got)
	}
	note, err := s.Note(ctx, "plan:"+facts.Thread.Owner+":approved")
	if err != nil || note.SessionID != 1 || !strings.Contains(note.Text, "[P1 · approved]") || !strings.Contains(note.Text, "- t1 · Keys: agent vera, repository frostyard/example") ||
		!strings.Contains(note.Text, "- t3 · Docs: agent duncan, no repository (its home)") || strings.Contains(note.Text, "t2 ·") {
		t.Fatalf("the supervisor's note %+v %v", note, err)
	}
	if _, replayed, err := s.ApprovePlan(ctx, "a1", plan.ID, "laptop", func(PlanFacts) error { return refused }); err != nil || !replayed {
		t.Fatalf("a retry: %v %v", replayed, err)
	}
	if _, _, err := s.ApprovePlan(ctx, "a1", plan.ID+1, "laptop", allow); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("the request for another plan: %v", err)
	}
	var notes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("%d notes, want one", notes)
	}
}

func TestADeliveryChecksTheGateWhereItOpensTheThread(t *testing.T) {
	s, _ := open(t)
	_, facts := approvedPlan(t, s, "p1")
	plan := facts.Plan.ID
	refused := errors.New("t2 waits")
	if _, _, err := s.CreateTaskThread(ctx, ThreadRow{Key: "d0", Title: "Sign", Agent: "vera", SupervisorSession: 1, Task: "x"}, plan, "t2",
		func(PlanFacts) error { return refused }, briefOf("t2")); !errors.Is(err, refused) {
		t.Fatalf("a refused delivery: %v", err)
	}
	if n, _ := s.OpenThreads(ctx); n != 0 {
		t.Fatalf("a refused delivery left %d threads", n)
	}
	var seen PlanFacts
	thread, existed, err := s.CreateTaskThread(ctx, ThreadRow{Key: "d1", Title: "Keys", Agent: "vera", SupervisorSession: 1, Repository: "frostyard/example", Task: "x"}, plan, "t1",
		func(f PlanFacts) error { seen = f; return nil }, briefOf("t1"))
	if err != nil || existed || thread.Plan != plan || thread.PlanTask != "t1" || thread.IsPlanThread() || seen.Task("t1") == nil ||
		thread.Task != "Build Keys" || thread.Title != "t1" {
		t.Fatalf("deliver %+v %v %v", thread, existed, err)
	}
	after, _ := s.PlanFacts(ctx, plan)
	if after.Plan.State != team.PlanBuilding || taskStates(after)["t1"] != team.TaskDelegated || after.Task("t1").Thread != thread.ID {
		t.Fatalf("after the delivery %+v %v", after.Plan, taskStates(after))
	}
	// A task's thread takes its place on the desk.
	if n, _ := s.OpenThreads(ctx); n != 1 {
		t.Fatalf("desk %d", n)
	}
	if again, existed, err := s.CreateTaskThread(ctx, ThreadRow{Key: "d1"}, plan, "t1", func(PlanFacts) error { return refused }, briefOf("t1")); err != nil || !existed || again.ID != thread.ID {
		t.Fatalf("a retry of the call: %+v %v %v", again, existed, err)
	}
}

func deliver(t *testing.T, s *Store, plan int64, key, task string) ThreadRow {
	t.Helper()
	thread, _, err := s.CreateTaskThread(ctx, ThreadRow{Key: key, Title: task, Agent: "vera", SupervisorSession: 1, Task: "x"}, plan, task, allow, briefOf(task))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachThread(ctx, thread.ID, 70+thread.ID, "/w", "", "brief"); err != nil {
		t.Fatal(err)
	}
	if thread, err = s.OpenThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	return thread
}

func TestATaskThreadsCloseSettlesItsTask(t *testing.T) {
	s, _ := open(t)
	planThread, facts := approvedPlan(t, s, "p1")
	plan := facts.Plan.ID
	owner := facts.Thread.Owner
	t1 := deliver(t, s, plan, "d1", "t1")
	// Closed with no pull request and no done report: ready again, and
	// the supervisor is told.
	if _, err := s.CloseThread(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	f, _ := s.PlanFacts(ctx, plan)
	if taskStates(f)["t1"] != team.TaskReady {
		t.Fatalf("t1 is %s", taskStates(f)["t1"])
	}
	if note, err := s.Note(ctx, "plan:"+owner+":again:"+t1.Owner); err != nil || !strings.Contains(note.Text, "t1 ready again") {
		t.Fatalf("again note %+v %v", note, err)
	}
	// One that closes while it starts makes it ready again and is noted
	// too: a start can fail after the supervisor was told the task went
	// (Codex review of #25, round 5).
	starting, _, err := s.CreateTaskThread(ctx, ThreadRow{Key: "d1b", Title: "t1", Agent: "vera", SupervisorSession: 1, Task: "x"}, plan, "t1", allow, briefOf("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailThread(ctx, starting.ID, "no sign-in"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.PlanFacts(ctx, plan); taskStates(f)["t1"] != team.TaskReady {
		t.Fatalf("t1 after a failed start is %s", taskStates(f)["t1"])
	}
	if note, err := s.Note(ctx, "plan:"+owner+":again:"+starting.Owner); err != nil || !strings.Contains(note.Text, "t1 ready again") {
		t.Fatalf("a failed start's note %+v %v", note, err)
	}
	// A done report, then the close: done, and t2 is ready now.
	t1 = deliver(t, s, plan, "d1c", "t1")
	if _, _, _, err := s.FileReport(ctx, "t1r", t1.ID, ThreadDone, "Done, no change needed", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.PlanFacts(ctx, plan)
	if got := taskStates(f); got["t1"] != team.TaskDone || got["t2"] != team.TaskReady {
		t.Fatalf("states %v", got)
	}
	if note, err := s.Note(ctx, "plan:"+owner+":ready:"+t1.Owner); err != nil || !strings.Contains(note.Text, "[P1 · t1 done]") || !strings.Contains(note.Text, "- t2 · Sign: agent vera, repository frostyard/example") {
		t.Fatalf("ready note %+v %v", note, err)
	}
	// An older delivery's close changes nothing.
	if _, err := s.CloseThread(ctx, starting.ID); err != nil {
		t.Fatal(err)
	}
	// A merged pull request, through the round's close.
	t2 := deliver(t, s, plan, "d2", "t2")
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "t2r", t2.ID, ThreadOpen, "PR", nil, []PullRow{pull("frostyard/example", 9, PullMerged)}, pullReportText); err != nil {
		t.Fatal(err)
	}
	if _, closed, err := s.CloseMergedThread(ctx, t2.ID, func([]PullRow) string { return "merged" }); err != nil || !closed {
		t.Fatalf("close merged %v %v", closed, err)
	}
	f, _ = s.PlanFacts(ctx, plan)
	if got := taskStates(f); got["t2"] != team.TaskDone || f.Plan.State != team.PlanBuilding {
		t.Fatalf("states %v, plan %s", got, f.Plan.State)
	}
	// t2's done freed nothing: no note.
	if _, err := s.Note(ctx, "plan:"+owner+":ready:"+t2.Owner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a note that names no ready task: %v", err)
	}
	// The last task's done makes the plan done and closes its thread.
	t3 := deliver(t, s, plan, "d3", "t3")
	if _, _, _, err := s.FileReport(ctx, "t3r", t3.ID, ThreadDone, "Written", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThreadAs(ctx, "owner-close", t3.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.PlanFacts(ctx, plan)
	if f.Plan.State != team.PlanDone || f.Thread.State != ThreadClosed || f.Thread.Report != team.PlanClosedReport(plan) {
		t.Fatalf("plan %s, its thread %+v", f.Plan.State, f.Thread)
	}
	if note, err := s.Note(ctx, "plan:"+owner+":done"); err != nil || note.SessionID != planThread.SupervisorSession || !strings.Contains(note.Text, "[P1 · done]") {
		t.Fatalf("done note %+v %v", note, err)
	}
}

func TestClosingThePlanThreadDropsThePlan(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	facts, _, _, err := s.AddConsults(ctx, "k", plan.ID, []ConsultRow{{Key: "k:0", Agent: "vera", Question: "q"}, {Key: "k:1", Agent: "irulan", Question: "q"}}, allow, says("asked"))
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.RecordConsultSession(ctx, "consult:"+facts.Consults[0].Owner, SessionRow{ID: 91, Title: "K1", Cwd: "/home/vera", Provider: "p", Model: "m", CreatedBy: "team"},
		facts.Consults[0].ID, asking("the question"))
	if err != nil || running.State != team.ConsultRunning || running.Session != 91 {
		t.Fatalf("running %+v %v", running, err)
	}
	if _, err := s.CloseThreadAs(ctx, "close", thread.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.PlanFacts(ctx, plan.ID)
	if after.Plan.State != team.PlanDropped {
		t.Fatalf("plan %s", after.Plan.State)
	}
	for _, c := range after.Consults {
		if c.State != team.ConsultStopped || c.Why != "its plan was dropped" || c.EndedAt == nil {
			t.Errorf("consult %+v", c)
		}
	}
	if note, err := s.Note(ctx, "consult:"+facts.Consults[0].Owner+":question"); err != nil || note.DroppedAt == nil {
		t.Fatalf("the question %+v %v", note, err)
	}
	if sessions, _ := s.SessionsWithNotes(ctx); len(sessions) != 0 {
		t.Fatalf("notes wait for %v", sessions)
	}
}

func TestAConsultRunsOnceAndAnswersOnce(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	facts, _, _, err := s.AddConsults(ctx, "k", plan.ID, []ConsultRow{{Key: "k:0", Agent: "vera", Question: "Holds?", AskID: 3}}, allow, says("asked"))
	if err != nil {
		t.Fatal(err)
	}
	c := facts.Consults[0]
	if c.AskID != 3 || c.Question != "Holds?" || c.Deadline() != c.CreatedAt.Add(team.ConsultLimit) {
		t.Fatalf("consult %+v", c)
	}
	key := "consult:" + c.Owner
	session := SessionRow{ID: 91, Title: "K1", Cwd: "/home/vera", Provider: "p", Model: "m", Profile: "vera", CreatedBy: "team"}
	running, err := s.RecordConsultSession(ctx, key, session, c.ID, asking("the question"))
	if err != nil || running.State != team.ConsultRunning {
		t.Fatalf("record %+v %v", running, err)
	}
	// Recorded again, as by a pass after a restart: one question.
	if _, err := s.RecordConsultSession(ctx, key, session, c.ID, asking("the question")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsultOfSession(ctx, 91); err != nil || got.ID != c.ID {
		t.Fatalf("consult of session %+v %v", got, err)
	}
	if note, ok, err := s.OldestNote(ctx, 91); err != nil || !ok || note.Text != "the question" || note.ConsultID != c.ID {
		t.Fatalf("question %+v %v %v", note, ok, err)
	}
	refused := errors.New("no")
	finding := team.Finding{Stance: team.StanceObjects, Summary: " Keys do not survive. "}
	if _, _, _, err := s.FileFinding(ctx, "f0", 91, finding, func(ConsultRow, PlanFacts) error { return refused }, func(ConsultRow) string { return "x" }, "x"); !errors.Is(err, refused) {
		t.Fatalf("a refused finding: %v", err)
	}
	got, result, replayed, err := s.FileFinding(ctx, "f1", 91, finding, func(ConsultRow, PlanFacts) error { return nil },
		func(c ConsultRow) string { return "finding: " + c.Stance + " " + c.Summary }, "Filed.")
	if err != nil || replayed || result != "Filed." || got.State != team.ConsultAnswered || got.Stance != team.StanceObjects || got.Summary != "Keys do not survive." {
		t.Fatalf("finding %+v %q %v %v", got, result, replayed, err)
	}
	note, err := s.Note(ctx, key+":finding")
	if err != nil || note.SessionID != thread.Session || note.Text != "finding: objects Keys do not survive." {
		t.Fatalf("the planning session's note %+v %v", note, err)
	}
	if _, again, replayed, err := s.FileFinding(ctx, "f1", 91, finding, func(ConsultRow, PlanFacts) error { return refused }, nil, "other"); err != nil || !replayed || again != "Filed." {
		t.Fatalf("a replay %q %v %v", again, replayed, err)
	}
	// An answered consult never ends otherwise.
	if _, ended, err := s.EndConsult(ctx, c.ID, team.ConsultFailed, "timeout", "late"); err != nil || ended {
		t.Fatalf("an answered consult ended: %v %v", ended, err)
	}
	if _, _, _, err := s.FileFinding(ctx, "f2", 92, finding, nil, nil, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session of no consult: %v", err)
	}
}

// A consult's question quotes the plan as the transaction that files it
// reads it, a write after the consult's call included, with the ask it is
// on: the expert has no other way to read the plan (the live check of
// 2026-11-20).
func TestAConsultsQuestionQuotesThePlanAsItStarts(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	_, asks, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadOpen, "Q", []AskRow{{Key: "r1:ask:0", SessionID: thread.Session, Question: "Which key?"}}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	facts, _, _, err := s.AddConsults(ctx, "k", plan.ID, []ConsultRow{{Key: "k:0", Agent: "vera", Question: "Holds?", AskID: asks[0].ID}}, allow, says("asked"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.WritePlan(ctx, "w", plan.ID, "# Signing, as written after the call", nil, allow, says("written")); err != nil {
		t.Fatal(err)
	}
	c := facts.Consults[0]
	if _, err := s.RecordConsultSession(ctx, "consult:"+c.Owner, SessionRow{ID: 91, Title: "K1", Cwd: "/home/vera", Provider: "p", Model: "m", CreatedBy: "team"}, c.ID,
		func(p PlanRow, a *AskRow) string {
			on := "no ask"
			if a != nil {
				on = fmt.Sprintf("A%d %q", a.ID, a.Question)
			}
			return fmt.Sprintf("P%d %s at %s, on %s", p.ID, p.Text, p.Rev, on)
		}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("P%d # Signing, as written after the call at %s, on A%d \"Which key?\"", plan.ID, team.Rev("# Signing, as written after the call"), asks[0].ID)
	if note, err := s.Note(ctx, "consult:"+c.Owner+":question"); err != nil || note.Text != want {
		t.Fatalf("the question %+v %v, want %q", note, err, want)
	}
}

func TestAConsultThatEndsDropsItsQuestion(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	facts, _, _, err := s.AddConsults(ctx, "k", plan.ID, []ConsultRow{{Key: "k:0", Agent: "vera", Question: "q"}}, allow, says("asked"))
	if err != nil {
		t.Fatal(err)
	}
	c := facts.Consults[0]
	if _, err := s.RecordConsultSession(ctx, "consult:"+c.Owner, SessionRow{ID: 91, Title: "K1", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "team"}, c.ID, asking("q")); err != nil {
		t.Fatal(err)
	}
	ended, did, err := s.EndConsult(ctx, c.ID, team.ConsultFailed, "no finding 60 minutes after the call", "ended")
	if err != nil || !did || ended.State != team.ConsultFailed || ended.EndedAt == nil {
		t.Fatalf("end %+v %v %v", ended, did, err)
	}
	if q, _ := s.Note(ctx, "consult:"+c.Owner+":question"); q.DroppedAt == nil {
		t.Fatalf("the question was not dropped: %+v", q)
	}
	if end, err := s.Note(ctx, "consult:"+c.Owner+":end"); err != nil || end.SessionID != thread.Session || end.Text != "ended" {
		t.Fatalf("end note %+v %v", end, err)
	}
	if _, did, _ := s.EndConsult(ctx, c.ID, team.ConsultFailed, "again", "again"); did {
		t.Fatal("a consult ended twice")
	}
	// A consult that ended before its session was recorded gets the
	// session, and no question.
	facts, _, _, err = s.AddConsults(ctx, "k2", plan.ID, []ConsultRow{{Key: "k2:0", Agent: "irulan", Question: "q"}}, allow, says("asked"))
	if err != nil {
		t.Fatal(err)
	}
	late := facts.Consults[1]
	if _, _, err := s.EndConsult(ctx, late.ID, team.ConsultFailed, "x", "x"); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecordConsultSession(ctx, "consult:"+late.Owner, SessionRow{ID: 92, Title: "K2", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "team"}, late.ID, asking("q"))
	if err != nil || got.State != team.ConsultFailed || got.Session != 0 {
		t.Fatalf("late record %+v %v", got, err)
	}
	if _, err := s.Note(ctx, "consult:"+late.Owner+":question"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an ended consult got its question: %v", err)
	}
	if _, err := s.Session(ctx, 92); err != nil {
		t.Fatalf("the session was not recorded: %v", err)
	}
}

func TestNotesGoInOrderOncePerKey(t *testing.T) {
	s, _ := open(t)
	_, facts := approvedPlan(t, s, "p1")
	owner := facts.Thread.Owner
	// A second note to the supervisor, after the approval's.
	t1 := deliver(t, s, facts.Plan.ID, "d1", "t1")
	if _, err := s.CloseThread(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SessionsWithNotes(ctx); !slices.Equal(got, []int64{1}) {
		t.Fatalf("sessions %v", got)
	}
	first, ok, err := s.OldestNote(ctx, 1)
	if err != nil || !ok || first.Key != "plan:"+owner+":approved" {
		t.Fatalf("oldest %+v %v %v", first, ok, err)
	}
	if err := s.NoteSubmitted(ctx, first.Key, 33); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.NoteWithdrawn(ctx, first.Key, 0); err != nil || !moved {
		t.Fatalf("withdrawn %v %v", moved, err)
	}
	if moved, _ := s.NoteWithdrawn(ctx, first.Key, 0); moved {
		t.Fatal("a stale attempt moved the note")
	}
	if got, _ := s.Note(ctx, first.Key); got.Attempt != 1 || got.Submission != 0 {
		t.Fatalf("after the withdrawal %+v", got)
	}
	if err := s.NoteSent(ctx, first.Key); err != nil {
		t.Fatal(err)
	}
	second, ok, _ := s.OldestNote(ctx, 1)
	if !ok || second.Key != "plan:"+owner+":again:"+t1.Owner {
		t.Fatalf("next %+v", second)
	}
	if err := s.DropNote(ctx, second.Key); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.OldestNote(ctx, 1); ok {
		t.Fatal("a dropped note waits")
	}
}

func TestPlanQueriesUseTheirIndexes(t *testing.T) {
	s, _ := open(t)
	for query, index := range map[string]string{
		`SELECT COUNT(*) FROM plans WHERE state NOT IN ('done', 'dropped')`:                                                         "plans_open",
		`SELECT ` + consultColumns + ` FROM consults WHERE state IN ('starting', 'running') ORDER BY id`:                            "consults_open",
		`SELECT ` + consultColumns + ` FROM consults WHERE session = 5 AND session != 0`:                                            "consults_session",
		`SELECT ` + noteColumns + ` FROM notes WHERE session_id = 5 AND sent_at IS NULL AND dropped_at IS NULL ORDER BY id LIMIT 1`: "notes_unsent",
		`SELECT session_id FROM notes WHERE sent_at IS NULL AND dropped_at IS NULL GROUP BY session_id ORDER BY MIN(id)`:            "notes_unsent",
		// A plan's gate reads its thread's asks, never the whole history
		// of asks (Codex review of #25, round 2).
		`SELECT ` + askColumns + ` FROM asks WHERE thread_id = 5 ORDER BY id`: "asks_thread",
		strings.ReplaceAll(heldQuery, "?", "'approved'"):                      "plans_held",
	} {
		if plan := planOf(t, s, query); !strings.Contains(plan, index) {
			t.Errorf("%s: plan %q does not use %s", query, plan, index)
		}
	}
}

// An ask filed on a plan's thread, and its answer, change the plan's gate,
// so they move the plan's time, whatever its state (Codex review of #25,
// round 1).
func TestAnAskAndItsAnswerTouchThePlan(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	updated := func() int64 {
		t.Helper()
		got, err := s.Plan(ctx, plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.UpdatedAt.UnixMilli()
	}
	before := updated()
	_, asks, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadOpen, "Q", []AskRow{{Key: "r1:ask:0", SessionID: thread.Session, Question: "Which?"}}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	afterAsk := updated()
	if afterAsk <= before {
		t.Fatalf("an ask left the drafting plan at %d, from %d", afterAsk, before)
	}
	if _, _, err := s.AnswerAsk(ctx, "a1", asks[0].ID, "That.", "laptop"); err != nil {
		t.Fatal(err)
	}
	if got := updated(); got <= afterAsk {
		t.Fatalf("an answer left the plan at %d, from %d", got, afterAsk)
	}
	if got, _ := s.Plan(ctx, plan.ID); got.State != team.PlanDrafting {
		t.Fatalf("plan %s", got.State)
	}
}

// A task's done in a plan the desk's cap held back names every ready
// task, the held ones too, and clears the hold (Codex review of #25,
// round 3).
func TestADoneOfAHeldPlanNamesEveryReadyTask(t *testing.T) {
	s, _ := open(t)
	_, facts := approvedPlan(t, s, "p1")
	t1 := deliver(t, s, facts.Plan.ID, "d1", "t1")
	if err := s.HoldPlan(ctx, facts.Plan.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Plan(ctx, facts.Plan.ID); !got.Held {
		t.Fatal("the plan is not held")
	}
	if _, _, _, err := s.FileReport(ctx, "t1r", t1.ID, ThreadDone, "Done", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	note, err := s.Note(ctx, "plan:"+facts.Thread.Owner+":ready:"+t1.Owner)
	if err != nil || !strings.Contains(note.Text, "- t2 · Sign") || !strings.Contains(note.Text, "- t3 · Docs") {
		t.Fatalf("note %+v %v", note, err)
	}
	if got, _ := s.Plan(ctx, facts.Plan.ID); got.Held {
		t.Fatal("the hold outlived the note")
	}
}

// A task's thread that fails to start names the held tasks with it, as
// any close of a task does, and so frees the hold (Codex review of #25,
// rounds 4 and 5).
func TestAFailedStartNamesTheHeldTasks(t *testing.T) {
	s, _ := open(t)
	_, facts := approvedPlan(t, s, "p1")
	starting, _, err := s.CreateTaskThread(ctx, ThreadRow{Key: "d1", Agent: "vera", SupervisorSession: 1}, facts.Plan.ID, "t1", allow, briefOf("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HoldPlan(ctx, facts.Plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailThread(ctx, starting.ID, "no sign-in"); err != nil {
		t.Fatal(err)
	}
	note, err := s.Note(ctx, "plan:"+facts.Thread.Owner+":again:"+starting.Owner)
	if err != nil || !strings.Contains(note.Text, "- t1 · Keys") || !strings.Contains(note.Text, "- t3 · Docs") {
		t.Fatalf("note %+v %v", note, err)
	}
	if got, _ := s.Plan(ctx, facts.Plan.ID); got.Held {
		t.Fatal("the hold outlived its note")
	}
}

// An ask names the plan whose own thread asked, from the same read: its
// event needs no lookup that could fail after the answer committed (Codex
// review of #25, round 4).
func TestAnAskNamesItsPlan(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	_, asks, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadOpen, "Q", []AskRow{{Key: "r1:ask:0", SessionID: thread.Session, Question: "Which?"}}, reportText)
	if err != nil || len(asks) != 1 || asks[0].Plan != plan.ID {
		t.Fatalf("the plan's ask %+v %v", asks, err)
	}
	answered, _, err := s.AnswerAsk(ctx, "a1", asks[0].ID, "That.", "laptop")
	if err != nil || answered.Plan != plan.ID {
		t.Fatalf("the answered ask %+v %v", answered, err)
	}
	work, _, err := s.CreateThread(ctx, ThreadRow{Key: "w", Title: "x", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, other, _, err := s.FileReport(ctx, "r2", work.ID, ThreadOpen, "Q", []AskRow{{Key: "r2:ask:0", SessionID: 9, Question: "Which?"}}, reportText)
	if err != nil || other[0].Plan != 0 {
		t.Fatalf("another thread's ask %+v %v", other, err)
	}
}

// While an open ask holds an approved plan's gate, a task's close sends no
// delivery note: the answer that clears the gate names every ready task
// (Codex review of #25, round 5).
func TestATaskCloseBehindAnOpenAskWaitsForTheAnswer(t *testing.T) {
	s, _ := open(t)
	planThread, facts := approvedPlan(t, s, "p1")
	t1 := deliver(t, s, facts.Plan.ID, "d1", "t1")
	if err := s.HoldPlan(ctx, facts.Plan.ID); err != nil {
		t.Fatal(err)
	}
	_, asks, _, err := s.FileReport(ctx, "q", planThread.ID, ThreadOpen, "Q", []AskRow{{Key: "q:ask:0", SessionID: planThread.Session, Question: "Also?"}}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.FileReport(ctx, "t1r", t1.ID, ThreadDone, "Done", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Note(ctx, "plan:"+facts.Thread.Owner+":ready:"+t1.Owner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a delivery note behind an open ask: %v", err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a", asks[0].ID, "No.", "laptop"); err != nil {
		t.Fatal(err)
	}
	note, err := s.Note(ctx, "plan:"+facts.Thread.Owner+":cleared:"+asks[0].Key)
	if err != nil || !strings.Contains(note.Text, "- t2 · Sign") || !strings.Contains(note.Text, "- t3 · Docs") {
		t.Fatalf("the cleared note %+v %v", note, err)
	}
}

// A dropped plan holds nothing for the desk: its hold goes with it (Codex
// review of #25, round 7).
func TestADroppedPlanHoldsNothing(t *testing.T) {
	s, _ := open(t)
	planThread, facts := approvedPlan(t, s, "p1")
	if err := s.HoldPlan(ctx, facts.Plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThreadAs(ctx, "drop", planThread.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Plan(ctx, facts.Plan.ID); got.State != team.PlanDropped || got.Held {
		t.Fatalf("plan %s, held %v", got.State, got.Held)
	}
}

// A review thread holds a place on the desk as any thread does: its close,
// by the owner or by its verdict, tells a plan the desk's cap held back
// that the place is free, once.
func TestAReviewsCloseFreesAHeldPlansPlace(t *testing.T) {
	for _, byVerdict := range []bool{false, true} {
		s, _ := open(t)
		p := linkedPull(t, s)
		_, facts := approvedPlan(t, s, "p1")
		if err := s.HoldPlan(ctx, facts.Plan.ID); err != nil {
			t.Fatal(err)
		}
		review, _, err := s.CreateThread(ctx, ThreadRow{Key: "review:x", Title: "Cross-review", Agent: "vera", SupervisorSession: 1, Task: "t",
			ReviewPull: p.ID, ReviewHead: p.HeadSHA})
		if err != nil {
			t.Fatal(err)
		}
		if byVerdict {
			finish := &ReviewFinish{Report: func(int64) string { return "Verdict." }, Author: p.ThreadID}
			if _, _, _, _, err := s.AddReview(ctx, p, review0(p, review.ID), finish); err != nil {
				t.Fatal(err)
			}
		} else if _, _, err := s.CloseReview(ctx, review.ID, "Stopped.", nil); err != nil {
			t.Fatal(err)
		}
		note, err := s.Note(ctx, "plan:"+facts.Thread.Owner+":free:"+review.Owner)
		if err != nil || !strings.Contains(note.Text, "- t1 · Keys") || !strings.Contains(note.Text, "- t3 · Docs") {
			t.Fatalf("by verdict %v: note %+v %v", byVerdict, note, err)
		}
		if got, _ := s.Plan(ctx, facts.Plan.ID); got.Held {
			t.Fatalf("by verdict %v: the hold outlived its note", byVerdict)
		}
	}
}

// A supervisor's message that unblocks a plan's thread withdraws its open
// asks, which hold the gate no more and have no answer to write in; when
// the last of an approved plan's goes, its supervisor hears its ready
// tasks may go, as after an answer (ADR-0037).
func TestAWithdrawnAskLeavesThePlanGate(t *testing.T) {
	s, _ := open(t)
	thread, plan := newPlan(t, s, "p1")
	_, asks, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadBlocked, "Stuck", []AskRow{
		{Key: "r1:ask:0", SessionID: thread.Session, Question: "Which key?"},
		{Key: "r1:ask:1", SessionID: thread.Session, Question: "Which ISO?"}}, reportText)
	if err != nil || len(asks) != 2 {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerAsk(ctx, "answer1", asks[0].ID, "The TPM", "laptop"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Plan(ctx, plan.ID)
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:a", ThreadID: thread.ID, SessionID: thread.Session, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return before.UpdatedAt.Add(time.Second) }
	if _, change, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID); err != nil || len(change.Withdrawn) != 1 || change.Withdrawn[0].Plan != plan.ID {
		t.Fatalf("delivered %+v %v", change, err)
	}
	f, err := s.PlanFacts(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Gate(); !slices.Equal(got, []string{"A" + strconv.FormatInt(asks[0].ID, 10) + " not written"}) {
		t.Fatalf("gate %v, want only the answer to write in", got)
	}
	if !f.Plan.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("the withdrawal did not touch the plan: %v, before %v", f.Plan.UpdatedAt, before.UpdatedAt)
	}
	// An approved plan's last open ask withdrawn tells its supervisor.
	planThread, facts := approvedPlan(t, s, "p2")
	if err := s.HoldPlan(ctx, facts.Plan.ID); err != nil {
		t.Fatal(err)
	}
	_, held, _, err := s.FileReport(ctx, "q", planThread.ID, ThreadBlocked, "Q", []AskRow{{Key: "q:ask:0", SessionID: planThread.Session, Question: "Also?"}}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:b", ThreadID: planThread.ID, SessionID: planThread.Session, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SendDelivered(ctx, "team-send:1:b", planThread.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PlanFacts(ctx, facts.Plan.ID); len(got.Gate()) != 0 {
		t.Fatalf("an approved plan still waits for %v", got.Gate())
	}
	note, err := s.Note(ctx, "plan:"+facts.Thread.Owner+":cleared:"+held[0].Key)
	if err != nil || !strings.Contains(note.Text, "A"+strconv.FormatInt(held[0].ID, 10)+" withdrawn") || !strings.Contains(note.Text, "- t1 · Keys") {
		t.Fatalf("the cleared note %+v %v", note, err)
	}
}
