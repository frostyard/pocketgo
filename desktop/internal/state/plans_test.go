package state

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

func reviewPlan(id int64, rev string, version int) proto.Plan {
	return proto.Plan{ID: id, Thread: 10 + id, Lead: "duncan", Title: "Plan", State: proto.PlanReview, Rev: rev,
		Graph:     &proto.PlanGraph{Version: version, Rev: rev, Tasks: []proto.PlanTask{{ID: "t1", Agent: "vera", State: proto.TaskWaiting}}},
		UpdatedAt: time.Date(2026, 11, 19, 12, 0, int(id), 0, time.UTC)}
}

// Only a plan in review, settled, with the graph drafted for its revision
// waits for the owner's approval, and an approval names that revision and
// that graph (ADR-0020).
func TestPlanSeenIsWhatAnApprovalNames(t *testing.T) {
	rev := strings.Repeat("a", 64)
	good := reviewPlan(1, rev, 2)
	if got, graph, ok := PlanSeen(good); !ok || got != rev || graph != 2 {
		t.Fatalf("PlanSeen = %q %d %v", got, graph, ok)
	}
	change := func(f func(*proto.Plan)) proto.Plan {
		p := good
		g := *p.Graph
		p.Graph = &g
		f(&p)
		return p
	}
	for name, p := range map[string]proto.Plan{
		"drafting":          change(func(p *proto.Plan) { p.State = proto.PlanDrafting }),
		"approved":          change(func(p *proto.Plan) { p.State = proto.PlanApproved; p.Graph.Approved = true }),
		"no graph":          change(func(p *proto.Plan) { p.Graph = nil }),
		"an older graph":    change(func(p *proto.Plan) { p.Graph.Rev = strings.Repeat("b", 64) }),
		"an approved one":   change(func(p *proto.Plan) { p.Graph.Approved = true }),
		"no text":           change(func(p *proto.Plan) { p.Rev, p.Graph.Rev = "", "" }),
		"a gate that waits": change(func(p *proto.Plan) { p.Waiting = []string{"A1 open"} }),
	} {
		if _, _, ok := PlanSeen(p); ok || ToApprove(p) {
			t.Errorf("%s: waits for an approval", name)
		}
	}
}

// Plans merge by the rule of the threads: an event after a fetch began
// wins over the fetch's copy; the Inbox counts the plans to approve, oldest
// first.
func TestPlansMergeAndWaitInTheInbox(t *testing.T) {
	var d Data
	rev := strings.Repeat("a", 64)
	f := d.BeginPlans()
	approved := reviewPlan(1, rev, 1)
	approved.State, approved.Graph.Approved = proto.PlanApproved, true
	d.Apply(proto.Event{Type: proto.EventPlan, Plan: &approved})
	d.SetPlans(f, []proto.Plan{reviewPlan(1, rev, 1), reviewPlan(2, rev, 1), reviewPlan(3, rev, 3)})
	if p, _ := d.Plan(1); p.State != proto.PlanApproved {
		t.Fatalf("the fetch undid the event: %s", p.State)
	}
	got := d.PlansToApprove()
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("to approve %+v", got)
	}
	for _, k := range d.Inbox() {
		if k.List == "plans" && (k.Count != 2 || k.Status != ListLoaded) {
			t.Errorf("the Inbox counts %+v", k)
		}
	}
	d.SetPlansError(d.BeginPlans(), "timeout")
	if status, err := d.PlansStatus(); status != ListFailed || err != "timeout" {
		t.Fatalf("status %v %q", status, err)
	}
	if len(d.Plans()) != 3 {
		t.Error("a failed fetch dropped the plans")
	}
}

// A plan's own thread waits for the owner while its plan does, and takes
// no place on the desk, as pocketd counts it.
func TestAPlanThreadOnTheBoard(t *testing.T) {
	var d Data
	rev := strings.Repeat("a", 64)
	planThread := proto.Thread{ID: 11, Agent: "duncan", State: proto.ThreadOpen, Plan: 1}
	task := proto.Thread{ID: 12, Agent: "vera", State: proto.ThreadOpen, Plan: 1, PlanTask: "t1"}
	d.SetThreads(d.BeginThreads(), []proto.Thread{planThread, task})
	if !PlanThread(planThread) || PlanThread(task) {
		t.Fatal("PlanThread")
	}
	if m := d.NextMover(planThread); m != MoverSpecialist {
		t.Errorf("a plan thread with no plan loaded goes to %v", m)
	}
	d.SetPlans(d.BeginPlans(), []proto.Plan{reviewPlan(1, rev, 1)})
	if m := d.NextMover(planThread); m != MoverOwner {
		t.Errorf("a plan thread whose plan waits for the owner goes to %v", m)
	}
	if n := d.OnTheDesk(); n != 1 {
		t.Errorf("on the desk %d, want only the task's thread", n)
	}
}

// An approval tried again after a failure keeps its request id; an
// approval of the plan as it changed since is a new request.
func TestAnApprovalTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	rev := strings.Repeat("a", 64)
	h.srv.AddPlan(reviewPlan(1, rev, 1))
	h.connect(t)
	approve := func() {
		t.Helper()
		p, ok := h.store.Data.Plan(1)
		if !ok {
			t.Fatal("no plan")
		}
		h.store.ApprovePlan(p)
		h.loop.RunUntil(t, func() bool { return !h.store.Approving(1).Busy && h.store.InFlight() == 0 })
	}
	path := proto.Path(proto.PathApprovePlan, 1)
	h.srv.FailOnce(http.MethodPost, path, http.StatusBadGateway)
	approve()
	if h.store.Approving(1).Err == "" {
		t.Fatal("the failure is not kept for the card")
	}
	// The lead wrote again meanwhile: the owner sees another plan.
	changed := reviewPlan(1, strings.Repeat("b", 64), 2)
	h.srv.Emit(proto.Event{Type: proto.EventPlan, Plan: &changed})
	h.loop.RunUntil(t, func() bool { p, _ := h.store.Data.Plan(1); return p.Rev == changed.Rev })
	h.srv.FailOnce(http.MethodPost, path, http.StatusBadGateway)
	approve()
	approve()
	got := sentIDs(t, h, path)
	if len(got) != 3 || got[0] == got[1] || got[1] != got[2] {
		t.Fatalf("request ids %q: want another plan's approval a new id, and its retry the same", got)
	}
	if p, _ := h.store.Data.Plan(1); p.State != proto.PlanApproved {
		t.Fatalf("plan %s", p.State)
	}
	if h.store.Approving(1) != (Pending{}) {
		t.Errorf("an approved plan keeps %+v", h.store.Approving(1))
	}
}

// Once a plan is approved, its thread is the supervisor's while a task is
// ready to deliver, and its tasks' specialists' otherwise; the board's
// note counts the plans that wait for the owner apart from the desk
// (Codex review of #25, round 1).
func TestAnApprovedPlansThreadMovesWithItsTasks(t *testing.T) {
	var d Data
	rev := strings.Repeat("a", 64)
	planThread := proto.Thread{ID: 11, Agent: "duncan", State: proto.ThreadOpen, Plan: 1}
	d.SetThreads(d.BeginThreads(), []proto.Thread{planThread})
	d.SetAsks(d.BeginAsks(), nil)
	d.SetPulls(d.BeginPulls(), nil)
	if got := d.BoardNote(); got != "No thread is on the desk; the plans are not loaded" {
		t.Errorf("with the plans loading: %q", got)
	}
	d.SetPlans(d.BeginPlans(), []proto.Plan{reviewPlan(1, rev, 1)})
	if got := d.BoardNote(); got != "No thread is on the desk; 1 plan waits for your approval" {
		t.Errorf("with a plan to approve: %q", got)
	}
	approved := reviewPlan(1, rev, 1)
	approved.State, approved.Graph.Approved = proto.PlanBuilding, true
	approved.Graph.Tasks[0].State = proto.TaskReady
	d.Apply(proto.Event{Type: proto.EventPlan, Plan: &approved})
	if m := d.NextMover(planThread); m != MoverSupervisor {
		t.Errorf("a plan with a ready task goes to %v", m)
	}
	if got := ReadyTasks(approved); len(got) != 1 || got[0] != "t1" {
		t.Errorf("ready %v", got)
	}
	// An open ask of the plan holds every delivery: the owner moves it,
	// whether or not the ask loaded (Codex review of #25, round 2).
	approved.Waiting = []string{"A3 open"}
	d.Apply(proto.Event{Type: proto.EventPlan, Plan: &approved})
	if m := d.NextMover(planThread); m != MoverOwner {
		t.Errorf("a plan whose gate waits goes to %v", m)
	}
	approved.Waiting = nil
	approved.Graph.Tasks[0].State, approved.Graph.Tasks[0].Thread = proto.TaskDelegated, 12
	d.Apply(proto.Event{Type: proto.EventPlan, Plan: &approved})
	if m := d.NextMover(planThread); m != MoverSpecialist {
		t.Errorf("a plan whose tasks work goes to %v", m)
	}
	if got := d.BoardNote(); got != "No thread is on the desk" {
		t.Errorf("with nothing to approve: %q", got)
	}
	// A drafting plan's open ask waits for the owner too; an answer to
	// write in and a consult that runs wait for the lead (Codex review of
	// #25, round 3).
	drafting := reviewPlan(1, rev, 1)
	drafting.State, drafting.Waiting = proto.PlanDrafting, []string{"A4 not written", "K1 running"}
	d.Apply(proto.Event{Type: proto.EventPlan, Plan: &drafting})
	if m := d.NextMover(planThread); m != MoverSpecialist {
		t.Errorf("a drafting plan with nothing open for the owner goes to %v", m)
	}
	// Also when its lead reported blocked: the answer is the lead's to
	// write in (Codex review of #25, round 4).
	blocked := planThread
	blocked.State = proto.ThreadBlocked
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &blocked})
	if m := d.NextMover(blocked); m != MoverSpecialist {
		t.Errorf("a blocked lead with an answer to write in goes to %v", m)
	}
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &planThread})
	drafting.Waiting = []string{"A3 open", "A4 not written"}
	d.Apply(proto.Event{Type: proto.EventPlan, Plan: &drafting})
	if m := d.NextMover(planThread); m != MoverOwner {
		t.Errorf("a drafting plan with an open ask goes to %v", m)
	}
}
