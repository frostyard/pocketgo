package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// planHarness is a team harness with irulan, an expert with a home, and
// a plan P1 led by duncan, started by odrade's session, its planning
// session briefed.
type planHarness struct {
	*teamHarness
	plan   int64
	thread store.ThreadRow
	// timers are the consult deadlines the server armed.
	timers []time.Duration
}

func newPlanHarness(t *testing.T) *planHarness {
	t.Helper()
	h := &planHarness{teamHarness: newTeamHarness(t)}
	if err := os.WriteFile(filepath.Join(h.profileDir, "irulan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Irulan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.consultTimer = func(d time.Duration, _ func()) func() {
		h.mu.Lock()
		h.timers = append(h.timers, d)
		h.mu.Unlock()
		return func() {}
	}
	text, err := h.server.TeamDelegate(context.Background(), TeamCall{Session: h.super.ID, Task: 1, CallID: "plan"},
		team.Delegation{Agent: "duncan", Task: "Plan the signing of the ISO.", Title: "Signing", Plan: &team.PlanRef{Start: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Started plan P1, led by Duncan in thread T1: Signing") {
		t.Fatalf("start text %q", text)
	}
	h.plan = 1
	if h.thread, err = h.store.Thread(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	return h
}

// lead runs team_plan as the plan's lead.
func (h *planHarness) lead(call string, c team.PlanCall) (string, error) {
	return h.server.TeamPlan(context.Background(), TeamCall{Session: h.thread.Session, Task: 2, CallID: call}, c)
}

func (h *planHarness) facts() store.PlanFacts {
	h.t.Helper()
	f, err := h.store.PlanFacts(context.Background(), h.plan)
	if err != nil {
		h.t.Fatal(err)
	}
	return f
}

// settled writes the plan, drafts its tasks (t1 by vera in
// frostyard/example, t2 after it, t3 by duncan in his home) and calls
// ready.
func (h *planHarness) settled() store.PlanFacts {
	h.t.Helper()
	if _, err := h.lead("w1", team.PlanCall{Action: team.PlanWrite, Text: "# Signing\n\nSign with the TPM."}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.lead("g1", team.PlanCall{Action: team.PlanTasks, Tasks: []team.Task{
		{ID: "t1", Title: "Keys", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"Keys are made"}},
		{ID: "t2", Title: "Sign", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"The ISO is signed"}, Deps: []string{"t1"}},
		{ID: "t3", Title: "Docs", Agent: "duncan", Acceptance: []string{"It is written down"}},
	}}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.lead("r1", team.PlanCall{Action: team.PlanReady}); err != nil {
		h.t.Fatal(err)
	}
	return h.facts()
}

func (h *planHarness) approve(requestID string) proto.Plan {
	h.t.Helper()
	f := h.facts()
	plan, err := h.client.ApprovePlan(context.Background(), h.plan, proto.ApprovePlan{RequestID: requestID, Rev: f.Plan.Rev, Graph: f.Graph.Version})
	if err != nil {
		h.t.Fatal(err)
	}
	return plan
}

// sentTo are the texts of the messages sent to session, in order.
func (h *planHarness) sentTo(session int64) []string {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	var out []string
	for _, key := range h.engine.order {
		if h.engine.to[key] == session {
			out = append(out, h.engine.texts[key])
		}
	}
	return out
}

func countWith(texts []string, part string) int {
	n := 0
	for _, text := range texts {
		if strings.Contains(text, part) {
			n++
		}
	}
	return n
}

// A plan starts in its own thread: the lead plans in its home, with the
// planning brief and team_plan, and the thread takes no place on the desk
// (ADR-0020).
func TestAPlanStartsWithItsPlanningSession(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if !h.thread.IsPlanThread() || h.thread.State != store.ThreadOpen || h.thread.Cwd != h.home || h.thread.Agent != "duncan" {
		t.Fatalf("the plan thread %+v", h.thread)
	}
	spec := h.engine.specs[len(h.engine.specs)-1]
	if spec.Plan != h.plan || spec.Thread != h.thread.ID || spec.Profile.ID != "duncan" || spec.Consult != 0 {
		t.Fatalf("the planning session's spec %+v", spec)
	}
	brief := h.engine.texts["thread:"+h.thread.Owner+":brief"]
	if !strings.Contains(brief, "lead plan P1 in thread T1: Signing") || !strings.Contains(brief, "team_plan write") {
		t.Fatalf("brief %q", brief)
	}
	f := h.facts()
	if f.Plan.State != team.PlanDrafting || f.Plan.Lead != "duncan" || f.Plan.SupervisorSession != h.super.ID {
		t.Fatalf("plan %+v", f.Plan)
	}
	// The same call finds the same plan.
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "plan"},
		team.Delegation{Agent: "duncan", Task: "Plan the signing of the ISO.", Title: "Signing", Plan: &team.PlanRef{Start: true}}); err != nil {
		t.Fatal(err)
	}
	if plans, _ := h.store.Plans(ctx); len(plans) != 1 {
		t.Fatalf("a replay started %d plans", len(plans))
	}
	// The desk's cap counts no plan thread; the open plans have their own.
	h.server.SetDeskCap(1)
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "work"}, team.Delegation{Agent: "duncan", Task: "Look around"}); err != nil {
		t.Fatalf("a delegation beside a plan, with a cap of 1: %v", err)
	}
	for i, call := range []string{"p2", "p3"} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: call}, team.Delegation{Agent: "irulan", Task: "Plan " + call, Plan: &team.PlanRef{Start: true}}); err != nil {
			t.Fatalf("plan %d: %v", i+2, err)
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "p4"}, team.Delegation{Agent: "irulan", Task: "Plan more", Plan: &team.PlanRef{Start: true}}); err == nil ||
		!strings.Contains(err.Error(), "3 plans are open") {
		t.Fatalf("a fourth plan: %v", err)
	}
	for name, d := range map[string]team.Delegation{
		"a repository": {Agent: "vera", Task: "Plan", Repository: "frostyard/example", Plan: &team.PlanRef{Start: true}},
		"a bad plan":   {Agent: "duncan", Task: "Plan", Plan: &team.PlanRef{Start: true, ID: 1}},
	} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: name}, d); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	// An agent starts none.
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.thread.Session, CallID: "x"}, team.Delegation{Agent: "irulan", Task: "Plan", Plan: &team.PlanRef{Start: true}}); err == nil ||
		!strings.Contains(err.Error(), "only a supervisor") {
		t.Fatalf("a plan started by an agent: %v", err)
	}
}

// team_plan is the plan's lead's alone, checked by its session's thread.
func TestOnlyThePlansLeadPlans(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	work, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "work"}, team.Delegation{Agent: "irulan", Task: "Look around"})
	if err != nil {
		t.Fatal(err)
	}
	_ = work
	other, _ := h.store.Thread(ctx, 2)
	for name, session := range map[string]int64{"the supervisor": h.super.ID, "another thread's specialist": other.Session} {
		if _, err := h.server.TeamPlan(ctx, TeamCall{Session: session, CallID: "c"}, team.PlanCall{Action: team.PlanRead}); err == nil || !strings.Contains(err.Error(), "for a plan's lead") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if text, err := h.lead("read", team.PlanCall{Action: team.PlanRead}); err != nil || !strings.Contains(text, "Plan P1 (Signing), led by Duncan in thread T1: drafting; settled.") ||
		!strings.Contains(text, "no text yet") {
		t.Fatalf("read %q %v", text, err)
	}
}

// The lead writes the plan from the revision it read, drafts its tasks,
// and asks for the approval only once the gate is settled: every answer
// written in, and no consult running. Each call answers as it did when run
// again (ADR-0020).
func TestTheLeadWritesDraftsAndReadiesThroughTheGate(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("w0", team.PlanCall{Action: team.PlanTasks, Tasks: []team.Task{{ID: "t1", Title: "x", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"ok"}}}}); err == nil ||
		!strings.Contains(err.Error(), "has no text") {
		t.Fatalf("tasks before a text: %v", err)
	}
	first, err := h.lead("w1", team.PlanCall{Action: team.PlanWrite, Text: "# Signing"})
	if err != nil || !strings.Contains(first, "Wrote plan P1: it is at rev "+team.ShortRev(team.Rev("# Signing"))) {
		t.Fatalf("write %q %v", first, err)
	}
	if _, err := h.lead("w2", team.PlanCall{Action: team.PlanWrite, Text: "# Again"}); err == nil || !strings.Contains(err.Error(), "changed since rev") {
		t.Fatalf("a write with no rev on a written plan: %v", err)
	}
	if _, err := h.lead("w3", team.PlanCall{Action: team.PlanWrite, Text: "# Again", Rev: team.Rev("# Older")}); err == nil || !strings.Contains(err.Error(), "Read it again") {
		t.Fatalf("a stale write: %v", err)
	}
	// Run again, a call answers as it did, whatever the plan is now.
	if again, err := h.lead("w1", team.PlanCall{Action: team.PlanWrite, Text: "# Signing"}); err != nil || again != first {
		t.Fatalf("a write run again: %q %v", again, err)
	}
	if _, err := h.lead("g0", team.PlanCall{Action: team.PlanTasks, Tasks: []team.Task{{ID: "t1", Title: "x", Agent: "nobody", Acceptance: []string{"ok"}}}}); err == nil {
		t.Fatal("a task of no agent was drafted")
	}
	if _, err := h.lead("g1", team.PlanCall{Action: team.PlanTasks, Tasks: []team.Task{{ID: "t1", Title: "Keys", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"Keys are made"}}}}); err != nil {
		t.Fatal(err)
	}
	// The lead asks the owner, as every specialist does.
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.thread.Session, Task: 3, CallID: "m1"}, team.Report{Status: team.StatusWorking, Message: "One question.",
		Asks: []team.Ask{{Question: "Which key?", Options: []string{"TPM", "File"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.lead("r1", team.PlanCall{Action: team.PlanReady}); err == nil || !strings.Contains(err.Error(), "waiting: A1 open") {
		t.Fatalf("ready with an open ask: %v", err)
	}
	if _, err := h.lead("w4", team.PlanCall{Action: team.PlanWrite, Text: "# Signing, TPM", Rev: team.Rev("# Signing"), Asks: []int64{1}}); err == nil ||
		!strings.Contains(err.Error(), "A1 waits for the owner's answer") {
		t.Fatalf("a write naming an open ask: %v", err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "The TPM"}); err != nil {
		t.Fatal(err)
	}
	// The answer reaches the lead once, with a line to write it in.
	if got := h.sentTo(h.thread.Session); countWith(got, "The owner answered A1") != 1 || countWith(got, "team_plan write, naming 1 in asks") != 1 {
		t.Fatalf("the lead got %q", got)
	}
	if _, err := h.lead("r2", team.PlanCall{Action: team.PlanReady}); err == nil || !strings.Contains(err.Error(), "waiting: A1 not written") {
		t.Fatalf("ready with an answer not written: %v", err)
	}
	if _, err := h.lead("w5", team.PlanCall{Action: team.PlanWrite, Text: "# Signing, TPM", Rev: team.Rev("# Signing"), Asks: []int64{9}}); err == nil ||
		!strings.Contains(err.Error(), "A9 is no ask of plan P1") {
		t.Fatalf("a write naming another ask: %v", err)
	}
	if _, err := h.lead("w6", team.PlanCall{Action: team.PlanWrite, Text: "# Signing, TPM", Rev: team.Rev("# Signing"), Asks: []int64{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.lead("r3", team.PlanCall{Action: team.PlanReady}); err == nil || !strings.Contains(err.Error(), "no task graph for its rev") {
		t.Fatalf("ready with the tasks of an older rev: %v", err)
	}
	if _, err := h.lead("g2", team.PlanCall{Action: team.PlanTasks, Tasks: []team.Task{{ID: "t1", Title: "Keys", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"Keys are made"}}}}); err != nil {
		t.Fatal(err)
	}
	ready, err := h.lead("r4", team.PlanCall{Action: team.PlanReady})
	if err != nil || !strings.Contains(ready, "waits for the owner's approval") || h.facts().Plan.State != team.PlanReview {
		t.Fatalf("ready %q %v", ready, err)
	}
	// The supervisor is not told that the plan waits: the owner approves,
	// and the Inbox tells them.
	if got := h.sentTo(h.super.ID); countWith(got, "P1") != 0 {
		t.Fatalf("the supervisor was told %q", got)
	}
	// A write takes it back to drafting.
	if _, err := h.lead("w7", team.PlanCall{Action: team.PlanWrite, Text: "# Signing, TPM.", Rev: team.Rev("# Signing, TPM")}); err != nil {
		t.Fatal(err)
	}
	if got := h.facts().Plan.State; got != team.PlanDrafting {
		t.Fatalf("a write in review left %s", got)
	}
}

// The owner approves exactly the revision and graph they saw, once; the
// supervisor is told the ready tasks once (ADR-0020).
func TestTheOwnerApprovesWhatTheySaw(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	f := h.settled()
	plans, err := h.client.Plans(ctx)
	if err != nil || len(plans) != 1 || plans[0].State != proto.PlanReview || plans[0].Rev != f.Plan.Rev || plans[0].Graph == nil ||
		plans[0].Graph.Version != 1 || len(plans[0].Graph.Tasks) != 3 || plans[0].Graph.Approved || len(plans[0].Waiting) != 0 {
		t.Fatalf("plans %+v %v", plans, err)
	}
	for name, body := range map[string]proto.ApprovePlan{
		"another rev":   {RequestID: "x1", Rev: team.Rev("other"), Graph: 1},
		"another graph": {RequestID: "x2", Rev: f.Plan.Rev, Graph: 2},
	} {
		var perr *proto.Error
		if _, err := h.client.ApprovePlan(ctx, h.plan, body); !errors.As(err, &perr) || perr.Status != http.StatusConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	var perr *proto.Error
	if _, err := h.client.ApprovePlan(ctx, h.plan, proto.ApprovePlan{RequestID: "x3", Rev: "short", Graph: 1}); !errors.As(err, &perr) || perr.Status != http.StatusBadRequest {
		t.Errorf("a short rev: %v", err)
	}
	if _, err := h.client.ApprovePlan(ctx, 9, proto.ApprovePlan{RequestID: "x4", Rev: f.Plan.Rev, Graph: 1}); !errors.As(err, &perr) || perr.Status != http.StatusNotFound {
		t.Errorf("no plan: %v", err)
	}
	approved := h.approve("a1")
	if approved.State != proto.PlanApproved || approved.ApprovedBy != "laptop" || !approved.Graph.Approved {
		t.Fatalf("approved %+v", approved)
	}
	states := map[string]proto.TaskState{}
	for _, task := range approved.Graph.Tasks {
		states[task.ID] = task.State
	}
	if states["t1"] != proto.TaskReady || states["t2"] != proto.TaskWaiting || states["t3"] != proto.TaskReady {
		t.Fatalf("task states %v", states)
	}
	// A retry answers with the plan, and tells the supervisor nothing more;
	// a new request is refused.
	if again := h.approve("a1"); again.State != proto.PlanApproved {
		t.Fatalf("a retry %+v", again)
	}
	if _, err := h.client.ApprovePlan(ctx, h.plan, proto.ApprovePlan{RequestID: "a2", Rev: f.Plan.Rev, Graph: 1}); !errors.As(err, &perr) || perr.Status != http.StatusConflict {
		t.Fatalf("a second approval: %v", err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.sentTo(h.super.ID)
	// The note names what each delivery takes: only the lead reads the
	// graph (Codex review of #25, round 1).
	if countWith(got, "[P1 · approved]") != 1 || countWith(got, "- t1 · Keys: agent vera, repository frostyard/example") != 1 ||
		countWith(got, "- t3 · Docs: agent duncan, no repository (its home)") != 1 {
		t.Fatalf("the supervisor got %q", got)
	}
	// Once approved, the plan no longer changes.
	if _, err := h.lead("w9", team.PlanCall{Action: team.PlanWrite, Text: "# Changed", Rev: f.Plan.Rev}); err == nil || !strings.Contains(err.Error(), "no longer change") {
		t.Fatalf("a write after the approval: %v", err)
	}
}

// A task is delivered only through the gate, in the transaction that opens
// its thread: approved, ready, and to its agent and repository.
func TestTheGateHoldsTheDelivery(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	deliver := func(call, agent, repo, task string) (string, error) {
		return h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: call},
			team.Delegation{Agent: agent, Task: "Build it.", Repository: repo, Plan: &team.PlanRef{ID: h.plan, Task: task}})
	}
	h.settled()
	if _, err := deliver("d0", "vera", "frostyard/example", "t1"); err == nil || !strings.Contains(err.Error(), "not approved: it is review") {
		t.Fatalf("a delivery before the approval: %v", err)
	}
	h.approve("a1")
	for name, tc := range map[string]struct{ agent, repo, task, want string }{
		"a waiting task": {"vera", "frostyard/example", "t2", "t2 waits for t1"},
		"another agent":  {"duncan", "", "t1", "t1 is for vera, not duncan"},
		"no such task":   {"vera", "frostyard/example", "t9", `no task "t9"`},
		"no repository":  {"vera", "", "t1", "repositor"},
		"no plan":        {"vera", "frostyard/example", "t1", "no plan P7"},
	} {
		d := team.Delegation{Agent: tc.agent, Task: "Build it.", Repository: tc.repo, Plan: &team.PlanRef{ID: h.plan, Task: tc.task}}
		if name == "no plan" {
			d.Plan.ID = 7
		}
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: name}, d); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	text, err := deliver("d1", "vera", "frostyard/example", "t1")
	if err != nil || !strings.Contains(text, "Delivered task t1 of plan P1 to Vera") {
		t.Fatalf("deliver %q %v", text, err)
	}
	thread, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"}.key())
	if err != nil || thread.Plan != h.plan || thread.PlanTask != "t1" || thread.Title != "P1 t1 · Keys" {
		t.Fatalf("the task's thread %+v %v", thread, err)
	}
	brief := h.engine.texts["thread:"+thread.Owner+":brief"]
	// The brief carries the plan as approved: a decision written there
	// reaches the task's specialist (Codex review of #25, round 3).
	if !strings.Contains(brief, "task t1 of plan P1 (Signing)") || !strings.Contains(brief, "- Keys are made") ||
		!strings.Contains(brief, "Sign with the TPM.") {
		t.Fatalf("brief %q", brief)
	}
	f := h.facts()
	if f.Plan.State != team.PlanBuilding || f.Task("t1").State != team.TaskDelegated || f.Task("t1").Thread != thread.ID {
		t.Fatalf("after the delivery %+v %+v", f.Plan, f.Task("t1"))
	}
	if _, err := deliver("d2", "vera", "frostyard/example", "t1"); err == nil || !strings.Contains(err.Error(), "delivered already, in T") {
		t.Fatalf("a second delivery: %v", err)
	}
	if again, err := deliver("d1", "vera", "frostyard/example", "t1"); err != nil || again != text {
		t.Fatalf("the call run again: %q %v", again, err)
	}
	// Another supervisor delivers none of this plan's tasks.
	leto, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: leto.ID, CallID: "l"}, team.Delegation{Agent: "duncan", Task: "x", Plan: &team.PlanRef{ID: h.plan, Task: "t3"}}); err == nil ||
		!strings.Contains(err.Error(), "started by another supervisor") {
		t.Fatalf("another supervisor: %v", err)
	}
	// An open ask of the plan holds every delivery.
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.thread.Session, Task: 6, CallID: "m"}, team.Report{Status: team.StatusWorking, Message: "Wait.",
		Asks: []team.Ask{{Question: "Also docs?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver("d3", "duncan", "", "t3"); err == nil || !strings.Contains(err.Error(), "waiting: A1 open") {
		t.Fatalf("a delivery with an open ask: %v", err)
	}
}

// A task's thread that closes with its work done makes the task done and
// readies the ones it frees; the supervisor is told once. The last makes
// the plan done and closes its thread (ADR-0020).
func TestTasksAreDoneAtTheirThreadsClose(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.settled()
	h.approve("a1")
	deliver := func(call, agent, repo, task string) store.ThreadRow {
		t.Helper()
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: call},
			team.Delegation{Agent: agent, Task: "Build it.", Repository: repo, Plan: &team.PlanRef{ID: h.plan, Task: task}}); err != nil {
			t.Fatal(err)
		}
		thread, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: call}.key())
		if err != nil {
			t.Fatal(err)
		}
		return thread
	}
	done := func(thread store.ThreadRow, call string) {
		t.Helper()
		if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: call}, team.Report{Status: team.StatusDone, Message: "No change needed."}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, Task: 8, CallID: "close-" + call}, thread.ID); err != nil {
			t.Fatal(err)
		}
	}
	t1 := deliver("d1", "vera", "frostyard/example", "t1")
	// Closed with its work not done: ready again, and the supervisor is
	// told once.
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, Task: 8, CallID: "close-early"}, t1.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.facts().Task("t1").State; got != team.TaskReady {
		t.Fatalf("t1 is %s", got)
	}
	t1 = deliver("d1b", "vera", "frostyard/example", "t1")
	done(t1, "r1")
	f := h.facts()
	if f.Task("t1").State != team.TaskDone || f.Task("t2").State != team.TaskReady {
		t.Fatalf("t1 %s, t2 %s", f.Task("t1").State, f.Task("t2").State)
	}
	t2 := deliver("d2", "vera", "frostyard/example", "t2")
	done(t2, "r2")
	t3 := deliver("d3", "duncan", "", "t3")
	done(t3, "r3")
	f = h.facts()
	if f.Plan.State != team.PlanDone || f.Thread.State != store.ThreadClosed || f.Thread.Report != team.PlanClosedReport(h.plan) {
		t.Fatalf("plan %s, its thread %+v", f.Plan.State, f.Thread)
	}
	for range 2 {
		if err := h.server.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := h.sentTo(h.super.ID)
	for part, want := range map[string]int{"[P1 · approved]": 1, "[P1 · t1 ready again]": 1, "[P1 · t1 done]": 1, "[P1 · t2 done]": 0, "[P1 · done]": 1} {
		if n := countWith(got, part); n != want {
			t.Errorf("the supervisor got %d of %q, want %d: %q", n, part, want, got)
		}
	}
	// The plan's thread event went out with its close.
	if states := h.threadStates(h.thread.ID); states[len(states)-1] != proto.ThreadClosed {
		t.Fatalf("the plan thread's events %v", states)
	}
}

// A plan thread's report links no pull request: its tasks open them.
func TestAPlanThreadLinksNoPullRequest(t *testing.T) {
	h := newPlanHarness(t)
	_, err := h.server.TeamMessage(context.Background(), TeamCall{Session: h.thread.Session, CallID: "m"}, team.Report{Status: team.StatusDone, Message: "Opened one.",
		PullRequests: []team.PullRef{{Repository: "frostyard/example", Number: 3}}})
	if err == nil || !strings.Contains(err.Error(), "links no pull request") {
		t.Fatalf("a plan thread's pull request: %v", err)
	}
}

// Closing the plan's thread drops the plan and stops its consults.
func TestClosingThePlanThreadDropsIt(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.CloseThread(ctx, h.thread.ID, proto.CloseThread{RequestID: "close"}); err != nil {
		t.Fatal(err)
	}
	f := h.facts()
	if f.Plan.State != team.PlanDropped || f.Consults[0].State != team.ConsultStopped {
		t.Fatalf("plan %s, consult %+v", f.Plan.State, f.Consults[0])
	}
	plans, err := h.client.Plans(ctx)
	if err != nil || plans[0].State != proto.PlanDropped {
		t.Fatalf("plans %+v %v", plans, err)
	}
}

// A consult runs from its expert's profile in its home with read and
// team_finding alone, gets its question once, and answers once; the
// finding reaches the lead once (ADR-0020).
func TestAConsultReadsAndAnswersOnce(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		agents []string
		want   string
	}{
		"the lead":      {[]string{"duncan"}, "leads the plan"},
		"no home":       {[]string{"vera"}, "no home or workspace"},
		"a supervisor":  {[]string{"odrade"}, "supervisor"},
		"an unknown id": {[]string{"nobody"}, "nobody"},
	} {
		if _, err := h.lead("k-"+name, team.PlanCall{Action: team.PlanConsult, Agents: tc.agents, Question: "Holds?"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if _, err := h.lead("k-ask", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?", Ask: 4}); err == nil || !strings.Contains(err.Error(), "A4 is no ask") {
		t.Fatalf("a consult on another ask: %v", err)
	}
	text, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Do TPM keys survive a re-image?"})
	if err != nil || !strings.Contains(text, "Consulting K1 (irulan) on plan P1") {
		t.Fatalf("consult %q %v", text, err)
	}
	h.runScheduled(5)
	f := h.facts()
	c := f.Consults[0]
	if c.State != team.ConsultRunning || c.Session == 0 {
		t.Fatalf("consult %+v", c)
	}
	spec := h.engine.specs[len(h.engine.specs)-1]
	if spec.Consult != c.ID || spec.Profile.ID != "irulan" || spec.Cwd != h.home || spec.Thread != 0 || spec.Plan != 0 || spec.Title != "K1 · consult on P1" {
		t.Fatalf("the consult's spec %+v", spec)
	}
	if got := h.sentTo(c.Session); len(got) != 1 || !strings.Contains(got[0], "Duncan, who leads plan P1 (Signing), consults you as K1") || !strings.Contains(got[0], "re-image") {
		t.Fatalf("the consult got %q", got)
	}
	if got := f.Gate(); !slices.Equal(got, []string{"K1 running"}) {
		t.Fatalf("gate %v", got)
	}
	// The gate holds ready while it runs.
	if _, err := h.lead("w", team.PlanCall{Action: team.PlanWrite, Text: "# Plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.lead("r", team.PlanCall{Action: team.PlanReady}); err == nil || !strings.Contains(err.Error(), "K1 running") {
		t.Fatalf("ready with a consult running: %v", err)
	}
	// team_finding is a consult's alone.
	if _, err := h.server.TeamFinding(ctx, TeamCall{Session: h.thread.Session, CallID: "f"}, team.Finding{Stance: team.StanceSupports, Summary: "x"}); err == nil {
		t.Fatal("the lead filed a finding")
	}
	call := TeamCall{Session: c.Session, Task: 9, CallID: "f1"}
	filed, err := h.server.TeamFinding(ctx, call, team.Finding{Stance: team.StanceObjects, Summary: "They do not survive."})
	if err != nil || !strings.Contains(filed, "Filed your finding as K1") {
		t.Fatalf("finding %q %v", filed, err)
	}
	if again, err := h.server.TeamFinding(ctx, call, team.Finding{Stance: team.StanceObjects, Summary: "They do not survive."}); err != nil || again != filed {
		t.Fatalf("the call run again: %q %v", again, err)
	}
	if _, err := h.server.TeamFinding(ctx, TeamCall{Session: c.Session, Task: 10, CallID: "f2"}, team.Finding{Stance: team.StanceSupports, Summary: "Second thoughts."}); err == nil ||
		!strings.Contains(err.Error(), "filed its finding already") {
		t.Fatalf("a second finding: %v", err)
	}
	for range 2 {
		if err := h.server.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.sentTo(h.thread.Session); countWith(got, "[K1 · Irulan · consult on P1] objects: They do not survive.") != 1 {
		t.Fatalf("the lead got %q", got)
	}
	if got := h.sentTo(c.Session); len(got) != 1 {
		t.Fatalf("the consult got its question %d times", len(got))
	}
	if got := h.facts().Gate(); len(got) != 0 {
		t.Fatalf("an answered consult holds the gate: %v", got)
	}
}

// A consult with no finding 60 minutes after its call fails, once, and its
// lead is told once; the server wakes a pass at its deadline.
func TestAConsultWithNoFindingFailsAtItsDeadline(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	armed := slices.Clone(h.timers)
	h.mu.Unlock()
	if len(armed) == 0 || armed[len(armed)-1] < team.ConsultLimit-time.Minute || armed[len(armed)-1] > team.ConsultLimit+time.Minute {
		t.Fatalf("timers %v, want one at the deadline", armed)
	}
	h.clock.Advance(team.ConsultLimit - time.Second)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.facts().Consults[0].State; got != team.ConsultRunning {
		t.Fatalf("a consult before its deadline is %s", got)
	}
	h.clock.Advance(2 * time.Second)
	for range 2 {
		if err := h.server.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	c := h.facts().Consults[0]
	if c.State != team.ConsultFailed || !strings.Contains(c.Why, "no finding 60 minutes") {
		t.Fatalf("consult %+v", c)
	}
	if got := h.sentTo(h.thread.Session); countWith(got, "[K1 · Irulan · consult on P1] It ended without a finding") != 1 {
		t.Fatalf("the lead got %q", got)
	}
	if _, err := h.server.TeamFinding(ctx, TeamCall{Session: c.Session, CallID: "late"}, team.Finding{Stance: team.StanceNeutral, Summary: "Late."}); err == nil ||
		!strings.Contains(err.Error(), "K1 is failed") {
		t.Fatalf("a late finding: %v", err)
	}
}

// Plan events go out with the changes that made them: the start, a write,
// an ask and its answer, the approval.
func TestPlanEventsLeaveWithTheirChange(t *testing.T) {
	h := newPlanHarness(t)
	plans := func() []proto.Plan {
		h.mu.Lock()
		defer h.mu.Unlock()
		var out []proto.Plan
		for _, e := range h.events {
			if e.Type == proto.EventPlan {
				out = append(out, *e.Plan)
			}
		}
		return out
	}
	if got := plans(); len(got) == 0 || got[len(got)-1].State != proto.PlanDrafting {
		t.Fatalf("the start's events %+v", got)
	}
	if _, err := h.lead("w", team.PlanCall{Action: team.PlanWrite, Text: "# Plan"}); err != nil {
		t.Fatal(err)
	}
	if got := plans(); got[len(got)-1].Rev != team.Rev("# Plan") {
		t.Fatalf("after the write %+v", got[len(got)-1])
	}
	if _, err := h.server.TeamMessage(context.Background(), TeamCall{Session: h.thread.Session, CallID: "m"}, team.Report{Status: team.StatusWorking, Message: "Q",
		Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	if got := plans(); !slices.Equal(got[len(got)-1].Waiting, []string{"A1 open"}) {
		t.Fatalf("after the ask %+v", got[len(got)-1].Waiting)
	}
	if _, err := h.client.AnswerAsk(context.Background(), 1, proto.AnswerAsk{RequestID: "a", Answer: "That."}); err != nil {
		t.Fatal(err)
	}
	if got := plans(); !slices.Equal(got[len(got)-1].Waiting, []string{"A1 not written"}) {
		t.Fatalf("after the answer %+v", got[len(got)-1].Waiting)
	}
}

// The round's close of a task's thread, whose pull request merged, makes
// the task done and wakes a delivery pass for the supervisor's note: no
// later event is needed to bring it.
func TestARoundsCloseBringsThePlansNote(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.settled()
	h.approve("a1")
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"},
		team.Delegation{Agent: "vera", Task: "Build it.", Repository: "frostyard/example", Plan: &team.PlanRef{ID: h.plan, Task: "t1"}}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"}.key())
	if err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "report", thread.ID, store.ThreadOpen, "PR", nil,
		[]store.PullRow{{Repository: "frostyard/example", Number: 9, GitHubID: 1, Title: "t1", URL: "u", State: store.PullMerged, Head: "h", HeadSHA: strings.Repeat("a", 40), Base: "main"}},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	if err := h.server.closeIfMerged(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.facts().Task("t1").State; got != team.TaskDone {
		t.Fatalf("t1 is %s", got)
	}
	h.runScheduled(20)
	if got := h.sentTo(h.super.ID); countWith(got, "[P1 · t1 done]") != 1 {
		t.Fatalf("the supervisor got %q", got)
	}
}

// An answer to a plan's ask tells the lead to write it in only while the
// plan can still change.
func TestAnAnswerAfterTheApprovalAsksForNoWrite(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.settled()
	h.approve("a1")
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.thread.Session, Task: 6, CallID: "m"}, team.Report{Status: team.StatusWorking, Message: "Q",
		Asks: []team.Ask{{Question: "Also docs?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a", Answer: "Yes."}); err != nil {
		t.Fatal(err)
	}
	got := h.sentTo(h.thread.Session)
	if countWith(got, `The owner answered A1 ("Also docs?"): Yes.`) != 1 || countWith(got, "naming 1 in asks") != 0 {
		t.Fatalf("the lead got %q", got)
	}
}

// A delivery that raced the owner's approval briefs its specialist with
// the task as approved: the brief comes from the facts the gate checked,
// never from the read before (Codex review of #25, round 1).
func TestADeliveryRacingTheApprovalGetsTheApprovedTask(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	f := h.settled()
	h.server.afterPlanRead = func() {
		h.server.afterPlanRead = nil
		if _, _, err := h.store.ApprovePlan(ctx, "race", h.plan, "laptop", func(store.PlanFacts) error { return nil }); err != nil {
			t.Error(err)
		}
	}
	if f.Plan.State != team.PlanReview {
		t.Fatalf("plan %s", f.Plan.State)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"},
		team.Delegation{Agent: "vera", Task: "Build it.", Repository: "frostyard/example", Plan: &team.PlanRef{ID: h.plan, Task: "t1"}}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"}.key())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(thread.Task, "- Keys are made") || thread.Title != "P1 t1 · Keys" {
		t.Fatalf("the task's thread %q, %q", thread.Title, thread.Task)
	}
}

// A consult's session starts from its expert's profile as it is when the
// session starts, under the team's locks: an expert made private while
// its folder was checked starts nothing, and its consult fails (Codex
// review of #25, round 1).
func TestAConsultStartsFromTheProfileAsItIsThen(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	h.server.afterConsultSpec = func() {
		if err := os.WriteFile(filepath.Join(h.profileDir, "irulan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Irulan\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	created := h.engine.created
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if h.engine.created != created {
		t.Fatalf("a consult session started from the profile as it was")
	}
	c := h.facts().Consults[0]
	if c.State != team.ConsultFailed || !strings.Contains(c.Why, "private") {
		t.Fatalf("consult %+v", c)
	}
}

// A finding filed after the consult's deadline is refused, also before a
// pass ended the consult, as after a restart (Codex review of #25, round
// 1).
func TestALateFindingIsRefusedBeforeAnyPass(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	c := h.facts().Consults[0]
	h.clock.Advance(team.ConsultLimit)
	if _, err := h.server.TeamFinding(ctx, TeamCall{Session: c.Session, CallID: "late"}, team.Finding{Stance: team.StanceNeutral, Summary: "Late."}); err == nil ||
		!strings.Contains(err.Error(), "they are over") {
		t.Fatalf("a late finding: %v", err)
	}
	if got := h.facts().Consults[0].State; got != team.ConsultRunning {
		t.Fatalf("consult %s before any pass", got)
	}
}

// queueQuestion has the consult's session busy, and its question wait in
// the session's queue as it is sent, as a busy run holds it.
func (h *planHarness) queueQuestion(t *testing.T) store.ConsultRow {
	t.Helper()
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.runFirst = 1
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if !strings.HasSuffix(key, ":question") {
			return
		}
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		if _, sent := h.engine.sent[key]; sent {
			return
		}
		if h.engine.queued == nil {
			h.engine.queued = map[int64]bool{}
		}
		h.engine.queued[int64(len(h.engine.sent)+100)] = true // the id Send gives it
	}
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	c := h.facts().Consults[0]
	h.engine.mu.Lock()
	sub, sent := h.engine.sent["consult:"+c.Owner+":question"]
	queued := h.engine.queued[sub]
	h.engine.mu.Unlock()
	if !sent || !queued {
		t.Fatalf("the question was sent %v, queued %v", sent, queued)
	}
	return c
}

func (h *planHarness) withdrew(key string) bool {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	sub := h.engine.sent[key]
	return h.engine.withdrawn[sub]
}

// A consult that ends while its question waits in its session's queue has
// the question withdrawn first: the expert never spends a run on a consult
// that takes no finding (Codex review of #25, round 1).
func TestAnEndedConsultsQueuedQuestionIsWithdrawn(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	c := h.queueQuestion(t)
	h.clock.Advance(team.ConsultLimit + time.Second)
	_ = h.server.DeliverPending(ctx)
	if got := h.facts().Consults[0]; got.State != team.ConsultFailed {
		t.Fatalf("consult %+v", got)
	}
	if !h.withdrew("consult:" + c.Owner + ":question") {
		t.Fatal("the queued question of an expired consult stayed queued")
	}
}

// Closing the plan's thread withdraws its consults' queued questions
// before it stops them.
func TestClosingThePlanThreadWithdrawsTheQuestions(t *testing.T) {
	h := newPlanHarness(t)
	c := h.queueQuestion(t)
	if _, err := h.client.CloseThread(context.Background(), h.thread.ID, proto.CloseThread{RequestID: "close"}); err != nil {
		t.Fatal(err)
	}
	if !h.withdrew("consult:" + c.Owner + ":question") {
		t.Fatal("the queued question of a stopped consult stayed queued")
	}
	if got := h.facts().Consults[0].State; got != team.ConsultStopped {
		t.Fatalf("consult %s", got)
	}
}

// The owner's message to a consult's session waits until its question is
// in its transcript: the expert answers the question first (Codex review
// of #25, round 1).
func TestAConsultTakesNoMessageBeforeItsQuestion(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onCreate = func(spec SessionSpec) {
		if spec.Consult == 0 {
			return
		}
		h.engine.mu.Lock()
		h.engine.failTo[h.engine.next+1] = true // its question cannot go yet
		h.engine.mu.Unlock()
	}
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	c := h.facts().Consults[0]
	if c.State != team.ConsultRunning {
		t.Fatalf("consult %+v", c)
	}
	var perr *proto.Error
	if _, err := h.client.Send(ctx, c.Session, proto.SendMessage{RequestID: "m1", Text: "Hi"}); !errors.As(err, &perr) || perr.Status != http.StatusConflict ||
		!strings.Contains(perr.Message, "does not have its question yet") {
		t.Fatalf("a message before the question: %v", err)
	}
	h.engine.mu.Lock()
	h.engine.failTo = nil
	h.engine.mu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, c.Session, proto.SendMessage{RequestID: "m2", Text: "Hi"}); err != nil {
		t.Fatalf("a message after the question: %v", err)
	}
}

// A consult's question goes only once its session is watched: a watch that
// failed holds it, so the question and the answer reach the event stream
// (Codex review of #25, round 1).
func TestAQuestionWaitsForAWatch(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("no watch")
	h.engine.mu.Unlock()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	_ = h.server.DeliverPending(ctx)
	c := h.facts().Consults[0]
	if c.State != team.ConsultRunning {
		t.Fatalf("consult %+v", c)
	}
	if got := h.sentTo(c.Session); len(got) != 0 {
		t.Fatalf("the question went to an unwatched session: %q", got)
	}
	h.engine.mu.Lock()
	h.engine.watchErr = nil
	h.engine.mu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.sentTo(c.Session); len(got) != 1 {
		t.Fatalf("the question went %d times", len(got))
	}
	h.engine.mu.Lock()
	watched := h.engine.watching[c.Session]
	h.engine.mu.Unlock()
	if !watched {
		t.Fatal("the consult's session is not watched")
	}
}

// A consult's session keeps no lessons: team_memory refuses it at the team
// API, whatever tools it was offered (Codex review of #25, round 1).
func TestAConsultKeepsNoLessons(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	c := h.facts().Consults[0]
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: c.Session, CallID: "m"}, team.Memory{Action: team.MemoryAdd, Text: "Consults are fun."}); err == nil ||
		!strings.Contains(err.Error(), "not a consult's") {
		t.Fatalf("a consult's lesson: %v", err)
	}
	if rows, _ := h.store.LessonsOf(ctx, "irulan"); len(rows) != 0 {
		t.Fatalf("lessons %+v", rows)
	}
}

// team_plan read shows each task with its acceptance lines, which a new
// draft must give again (Codex review of #25, round 1).
func TestReadShowsEachTasksAcceptance(t *testing.T) {
	h := newPlanHarness(t)
	h.settled()
	text, err := h.lead("read", team.PlanCall{Action: team.PlanRead})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- t1: Keys, by vera in frostyard/example; waiting\n  - acceptance: Keys are made", "  - acceptance: The ISO is signed",
		"- t3: Docs, by duncan in duncan's home; waiting\n  - acceptance: It is written down"} {
		if !strings.Contains(text, want) {
			t.Errorf("read lacks %q:\n%s", want, text)
		}
	}
}

// The plan routes answer a wrong method with 405, as every route does
// (Codex review of #25, round 1).
func TestThePlanRoutesRefuseAWrongMethod(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}
	for _, c := range []struct{ method, path string }{
		{"POST", proto.PathPlans},
		{"GET", proto.Path(proto.PathApprovePlan, 1)},
		{"DELETE", proto.Path(proto.PathApprovePlan, 1)},
	} {
		response, e := h.raw(c.method, c.path, "", headers)
		if response.StatusCode != http.StatusMethodNotAllowed || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%s %s: %d %+v", c.method, c.path, response.StatusCode, e)
		}
	}
}

// An answer that clears the last open ask of an approved plan tells the
// supervisor, once, that its ready tasks may go: the gate refused them
// for that ask (Codex review of #25, round 2).
func TestAnAnswerThatClearsTheGateTellsTheSupervisor(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.settled()
	h.approve("a1")
	for _, call := range []string{"m1", "m2"} {
		if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.thread.Session, Task: 6, CallID: call}, team.Report{Status: team.StatusWorking, Message: "Q",
			Asks: []team.Ask{{Question: "Also " + call + "?"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"},
		team.Delegation{Agent: "vera", Task: "Build it.", Repository: "frostyard/example", Plan: &team.PlanRef{ID: h.plan, Task: "t1"}}); err == nil {
		t.Fatal("a delivery went with open asks")
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "answer1", Answer: "Yes."}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	if got := h.sentTo(h.super.ID); countWith(got, "answered]") != 0 {
		t.Fatalf("an answer that left an ask open told the supervisor %q", got)
	}
	if _, err := h.client.AnswerAsk(ctx, 2, proto.AnswerAsk{RequestID: "answer2", Answer: "No."}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	_ = h.server.DeliverPending(ctx)
	got := h.sentTo(h.super.ID)
	if countWith(got, "[P1 · A2 answered] Plan P1 has no open question now") != 1 || countWith(got, "- t1 · Keys: agent vera, repository frostyard/example") != 2 {
		t.Fatalf("the supervisor got %q", got)
	}
}

// team_plan read names the agents a task can go to and a consult can ask
// (Codex review of #25, round 2).
func TestReadNamesTheAgents(t *testing.T) {
	h := newPlanHarness(t)
	text, err := h.lead("read", team.PlanCall{Action: team.PlanRead})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Agents a task can go to", "- irulan (Irulan): its home", "- vera (Vera): repositories frostyard/example; no home, so no consult",
		"- duncan (Duncan): its home; it leads this plan, so no consult"} {
		if !strings.Contains(text, want) {
			t.Errorf("read lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "- odrade") || strings.Contains(text, "- clerk") {
		t.Errorf("read names one that takes no task:\n%s", text)
	}
}

// One consult that fails to end holds back no other consult's start
// (Codex review of #25, round 2).
func TestAConsultThatFailsToEndHoldsNoOtherStart(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(h.profileDir, "moneo.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Moneo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := h.queueQuestion(t)
	h.clock.Advance(team.ConsultLimit - time.Minute)
	if _, err := h.lead("k2", team.PlanCall{Action: team.PlanConsult, Agents: []string{"moneo"}, Question: "And you?"}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.withdrawFails = "consult:" + first.Owner + ":question"
	h.engine.mu.Unlock()
	h.clock.Advance(2 * time.Minute)
	if err := h.server.DeliverPending(ctx); err == nil {
		t.Fatal("a withdrawal that failed passed unnoticed")
	}
	f := h.facts()
	if f.Consults[0].State != team.ConsultRunning {
		t.Fatalf("K1 ended with its question queued: %+v", f.Consults[0])
	}
	if f.Consults[1].State != team.ConsultRunning {
		t.Fatalf("K2 did not start beside K1's failure: %+v", f.Consults[1])
	}
}

// A consult's question that has not gone by its deadline never goes, also
// before a pass ends the consult (Codex review of #25, round 2).
func TestAnExpiredQuestionIsNeverPlaced(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onCreate = func(spec SessionSpec) {
		if spec.Consult != 0 {
			h.engine.mu.Lock()
			h.engine.failTo[h.engine.next+1] = true
			h.engine.mu.Unlock()
		}
	}
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	c := h.facts().Consults[0]
	h.engine.mu.Lock()
	h.engine.failTo = nil
	h.engine.mu.Unlock()
	h.clock.Advance(team.ConsultLimit)
	if _, err := h.server.deliverNotes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.sentTo(c.Session); len(got) != 0 {
		t.Fatalf("an expired consult got its question: %q", got)
	}
}

// A consult's session starts only in a home that is a folder when the
// session starts, checked again under the team's locks (Codex review of
// #25, round 2).
func TestAConsultsHomeIsCheckedWhenItsSessionStarts(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), "irulan")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.profileDir, "irulan.md"), []byte("---\nmodel: faux/faux-1\nhome: "+home+"\n---\n# Irulan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	h.server.afterConsultSpec = func() {
		if err := os.Remove(home); err != nil {
			t.Error(err)
		}
	}
	created := h.engine.created
	_ = h.server.DeliverPending(ctx)
	if h.engine.created != created {
		t.Fatal("a consult session started in a home that is gone")
	}
	if c := h.facts().Consults[0]; c.State != team.ConsultFailed || !strings.Contains(c.Why, "not a folder") {
		t.Fatalf("consult %+v", c)
	}
}

// A consult session an earlier run made keeps the profile it was made
// from, also when the profile cannot be read at the restart (Codex review
// of #25, round 2).
func TestARecoveredConsultSessionKeepsItsProfile(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	c := h.facts().Consults[0]
	// The earlier run made the session, and stopped before recording it.
	if _, err := h.engine.CreateSession(ctx, "consult:"+c.Owner, SessionSpec{Title: "K1 · consult on P1", Cwd: h.home, Model: model,
		Profile: &profiles.Profile{ID: "irulan", Role: profiles.RoleAgent}, Consult: c.ID}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.profileDir, "irulan.md")); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	c = h.facts().Consults[0]
	if c.State != team.ConsultRunning || c.Session == 0 {
		t.Fatalf("consult %+v", c)
	}
	row, err := h.store.Session(ctx, c.Session)
	if err != nil || row.Profile != "irulan" {
		t.Fatalf("the recovered session %+v %v", row, err)
	}
}

// A ready task the desk's cap refused is named to the supervisor, once,
// when a place on the desk frees: an idle supervisor never waits on a
// plan that could go on (Codex review of #25, round 3).
func TestAFreedPlaceOnTheDeskTellsTheSupervisor(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.settled()
	h.approve("a1")
	h.server.SetDeskCap(2)
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 4, CallID: "w"}, team.Delegation{Agent: "irulan", Task: "Look around"}); err != nil {
		t.Fatal(err)
	}
	work, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, Task: 4, CallID: "w"}.key())
	if err != nil {
		t.Fatal(err)
	}
	deliver := func(call, agent, repo, task string) error {
		_, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: call},
			team.Delegation{Agent: agent, Task: "Build it.", Repository: repo, Plan: &team.PlanRef{ID: h.plan, Task: task}})
		return err
	}
	if err := deliver("d1", "vera", "frostyard/example", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := deliver("d3", "duncan", "", "t3"); !errors.Is(err, team.ErrDeskFull) {
		t.Fatalf("t3 with the desk full: %v", err)
	}
	if _, err := h.client.CloseThread(ctx, work.ID, proto.CloseThread{RequestID: "close-work"}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	_ = h.server.DeliverPending(ctx)
	got := h.sentTo(h.super.ID)
	if countWith(got, "[P1 · a place on the desk is free] T"+strconv.FormatInt(work.ID, 10)+" closed") != 1 ||
		countWith(got, "- t3 · Docs: agent duncan, no repository (its home)") != 2 {
		t.Fatalf("the supervisor got %q", got)
	}
	// Told once: the next close tells nothing more of it.
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 4, CallID: "w2"}, team.Delegation{Agent: "irulan", Task: "Again"}); err != nil {
		t.Fatal(err)
	}
	again, _ := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, Task: 4, CallID: "w2"}.key())
	if _, err := h.client.CloseThread(ctx, again.ID, proto.CloseThread{RequestID: "close-again"}); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	if got := h.sentTo(h.super.ID); countWith(got, "a place on the desk is free") != 1 {
		t.Fatalf("the supervisor got the free place twice: %q", got)
	}
}

// Under the team's locks, a consult's folder is checked first and its
// expert's profile read after: a profile made private during that check
// starts no session (Codex review of #25, round 3).
func TestAConsultReadsItsProfileAfterTheFolderCheck(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h.engine.onDir = func(context.Context) error {
		calls++
		if calls == 2 { // the check under the locks
			if err := os.WriteFile(filepath.Join(h.profileDir, "irulan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Irulan\n"), 0o600); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	created := h.engine.created
	_ = h.server.DeliverPending(ctx)
	if h.engine.created != created {
		t.Fatal("a consult session started from a profile read before its folder's check")
	}
	if c := h.facts().Consults[0]; c.State != team.ConsultFailed || !strings.Contains(c.Why, "private") {
		t.Fatalf("consult %+v", c)
	}
}

// A plan whose read fails after its change committed is published by the
// next delivery pass: no change goes without its event (Codex review of
// #25, round 3).
func TestAPlanEventThatFailsToReadGoesLater(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	waiting := func() []string {
		h.mu.Lock()
		defer h.mu.Unlock()
		for i := len(h.events) - 1; i >= 0; i-- {
			if e := h.events[i]; e.Type == proto.EventPlan {
				return e.Plan.Waiting
			}
		}
		return nil
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.thread.Session, CallID: "m"}, team.Report{Status: team.StatusWorking, Message: "Q",
		Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	if got := waiting(); !slices.Equal(got, []string{"A1 open"}) {
		t.Fatalf("after the ask %v", got)
	}
	failed := false
	h.server.planRead = func(ctx context.Context, id int64) (store.PlanFacts, error) {
		if !failed {
			failed = true
			return store.PlanFacts{}, errors.New("database is locked")
		}
		return h.store.PlanFacts(ctx, id)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "answer", Answer: "That."}); err != nil {
		t.Fatal(err)
	}
	if got := waiting(); !slices.Equal(got, []string{"A1 open"}) {
		t.Fatalf("an event went from a failed read: %v", got)
	}
	_ = h.server.DeliverPending(ctx)
	if got := waiting(); !slices.Equal(got, []string{"A1 not written"}) {
		t.Fatalf("after a pass the plan's event says %v", got)
	}
}

// A consult's deadline that passes while its question is on its way, as
// during the session's watch, stops the question right before its
// submission (Codex review of #25, round 3).
func TestADeadlineDuringTheWatchStopsTheQuestion(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onCreate = func(spec SessionSpec) {
		if spec.Consult != 0 {
			h.engine.mu.Lock()
			h.engine.failTo[h.engine.next+1] = true
			h.engine.mu.Unlock()
		}
	}
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	_ = h.server.DeliverPending(ctx)
	c := h.facts().Consults[0]
	h.engine.mu.Lock()
	h.engine.failTo = nil
	h.engine.mu.Unlock()
	h.clock.Advance(team.ConsultLimit - time.Second)
	h.engine.onWatch = func(id int64) {
		if id == c.Session {
			h.clock.Advance(2 * time.Second)
		}
	}
	_, _ = h.server.deliverNotes(ctx)
	if got := h.sentTo(c.Session); len(got) != 0 {
		t.Fatalf("a question went past its deadline: %q", got)
	}
}

// A pass that starts a consult arms its deadline's timer from what it read
// itself: no later read that fails leaves a running consult without one
// (Codex review of #25, round 3).
func TestAConsultsTimerNeedsNoSecondRead(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	h.server.openConsults = func(ctx context.Context) ([]store.ConsultRow, error) {
		reads++
		if reads > 2 {
			return nil, errors.New("database is locked")
		}
		return h.store.OpenConsults(ctx)
	}
	_ = h.server.DeliverPending(ctx)
	if c := h.facts().Consults[0]; c.State != team.ConsultRunning {
		t.Fatalf("consult %+v", c)
	}
	h.mu.Lock()
	armed := len(h.timers)
	h.mu.Unlock()
	if armed == 0 {
		t.Fatal("a running consult has no timer")
	}
}
