package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// seedPlan gives the fake pocketd a plan led by duncan in its thread T7,
// in review with its graph, and the team that works on it.
func (x *testApp) seedPlan() proto.Plan {
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor"})
	x.srv.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan", Role: "agent", Home: "/home/team/agents/duncan"})
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "agent"})
	x.srv.AddProfile(proto.Profile{ID: "irulan", Name: "Irulan", Role: "agent"})
	x.srv.AddThread(proto.Thread{ID: 7, Title: "Sign the ISO", Agent: "duncan", Supervisor: 1, Session: 17, Cwd: "/home/team/agents/duncan",
		Task: "Plan the signing.", State: proto.ThreadOpen, Plan: 1, CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	rev := strings.Repeat("ab", 32)
	p := proto.Plan{ID: 1, Thread: 7, Supervisor: 1, Lead: "duncan", Title: "Sign the ISO", State: proto.PlanReview, Rev: rev,
		Text: "# Sign the ISO\n\nSign each release with the TPM key; keep the public key in the repository.",
		Graph: &proto.PlanGraph{Version: 2, Rev: rev, Tasks: []proto.PlanTask{
			{ID: "t1", Title: "Make the signing key", Agent: "vera", Repository: "frostyard/snow", Acceptance: []string{"The key is in the TPM"}, State: proto.TaskWaiting},
			{ID: "t2", Title: "Sign the release ISO", Agent: "vera", Repository: "frostyard/snow", Acceptance: []string{"The ISO verifies"},
				Deps: []string{"t1"}, State: proto.TaskWaiting},
			{ID: "t3", Title: "Write the runbook", Agent: "duncan", Acceptance: []string{"It names every step"}, State: proto.TaskWaiting},
		}},
		Consults: []proto.Consult{{ID: 1, Agent: "irulan", Question: "Do TPM keys survive a re-image?", State: proto.ConsultAnswered,
			Stance: "supports", Summary: "They survive a re-image; not a TPM clear."}},
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)}
	x.srv.AddPlan(p)
	return p
}

// A plan that waits for the owner shows in the Inbox with its text, its
// tasks and its consults; Approve, after a confirmation, approves the
// revision and the graph the card showed (ADR-0020).
func TestAPlanInTheInboxSnapshot(t *testing.T) {
	x := newTestApp(t)
	p := x.seedPlan()
	x.connect(t)
	x.hasAll(t, "1 plan waits for you", "P1 · Sign the ISO waits for your approval", "T7 · led by Duncan", "rev abababa · tasks version 2 · 3 tasks",
		"t1 · Make the signing key", "Vera (vera) in frostyard/snow", "t2 · Sign the release ISO", "Vera (vera) in frostyard/snow · after t1",
		"t3 · Write the runbook", "Duncan (duncan) in its home", "• It names every step",
		"K1 · Irulan: “Do TPM keys survive a re-image?”", "supports · They survive a re-image; not a TPM clear.", "Approve Plan")
	x.snapshot(t, "inbox-plan")
	x.click(t, "Approve Plan")
	x.hasAll(t, "Approve P1?")
	if len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathApprovePlan, 1))) != 0 {
		t.Fatal("the approval went before the owner confirmed it")
	}
	x.click(t, "Approve P1")
	x.until(t, func() bool {
		got, _ := x.a.st.Data.Plan(1)
		return got.State == proto.PlanApproved && x.a.st.InFlight() == 0
	})
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathApprovePlan, 1))
	var body proto.ApprovePlan
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.RequestID == "" || body.Rev != p.Rev || body.Graph != 2 {
		t.Fatalf("the approval sent %+v (%+v)", reqs, body)
	}
	x.hasAll(t, "Nothing waits for you")
}

// A plan that changed while the owner looked is not approved: pocketd
// refuses, the card says why, and the plan as it is now shows.
func TestAnApprovalOfAChangedPlanSaysWhy(t *testing.T) {
	x := newTestApp(t)
	p := x.seedPlan()
	x.connect(t)
	x.hasAll(t, "Approve Plan")
	x.click(t, "Approve Plan")
	// The lead writes again while the confirmation waits.
	changed := p
	changed.Rev = strings.Repeat("cd", 32)
	graph := *p.Graph
	graph.Rev = changed.Rev
	changed.Graph = &graph
	changed.UpdatedAt = at(0)
	x.srv.AddPlan(changed)
	x.click(t, "Approve P1")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 && x.a.st.Approving(1).Err != "" })
	x.hasAll(t, "plan P1 changed since you saw it", "rev cdcdcdc · tasks version 2 · 3 tasks")
	got, _ := x.a.st.Data.Plan(1)
	if got.State != proto.PlanReview {
		t.Fatalf("plan %s", got.State)
	}
}

// The board shows a plan on its thread: where it is, what its gate waits
// for, and its tasks; a task's thread names the plan it delivers. The
// plan's thread takes no place on the desk.
func TestPlansOnTheBoardSnapshot(t *testing.T) {
	x := newTestApp(t)
	p := x.seedPlan()
	p.State, p.Graph.Approved = proto.PlanBuilding, true
	p.Graph.Tasks[0].State, p.Graph.Tasks[0].Thread = proto.TaskDelegated, 8
	p.Graph.Tasks[2].State = proto.TaskReady
	p.Waiting = []string{"A3 open"}
	x.srv.AddPlan(p)
	x.srv.AddThread(proto.Thread{ID: 8, Title: "P1 t1 · Make the signing key", Agent: "vera", Supervisor: 1, Repository: "frostyard/snow",
		Branch: "vera/p1-t1-make-the-signing-key-t8", Task: "Do it.", State: proto.ThreadOpen, Plan: 1, PlanTask: "t1",
		CreatedAt: at(-time.Hour), UpdatedAt: at(-2 * time.Minute)})
	x.connect(t)
	x.click(t, "Work Board")
	// t3 is ready, but A3 holds every delivery: the plan's thread waits
	// for the owner (Codex review of #25, rounds 1 and 2).
	x.hasAll(t, "1 thread on the desk", "T7 · Sign the ISO", "Waits for you", "P1 waits for your answer: A3 open.", "Plan P1", "building", "Waiting: A3 open",
		"t1 · Make the signing key", "T8", "delegated", "ready", "T8 · P1 t1 · Make the signing key", "Task t1 of plan P1 · Sign the ISO")
	x.snapshot(t, "board-plan")
}

// Closing a plan's own thread drops its plan: the confirmation says so,
// and names the plan (Codex review of #25, round 1).
func TestClosingAPlanThreadSaysItDropsThePlan(t *testing.T) {
	x := newTestApp(t)
	x.seedPlan()
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Close Thread")
	x.hasAll(t, "Close T7 and drop P1 (Sign the ISO)?", "Close T7 and Drop the Plan")
	if len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathCloseThread, 7))) != 0 {
		t.Fatal("the close went before the owner confirmed it")
	}
}

// An approved plan with a ready task and nothing open is its supervisor's
// to move: the card says what it delivers (Codex review of #25, round 1).
func TestAReadyTaskGoesToTheSupervisor(t *testing.T) {
	x := newTestApp(t)
	p := x.seedPlan()
	p.State, p.Graph.Approved = proto.PlanApproved, true
	p.Graph.Tasks[0].State = proto.TaskReady
	p.Graph.Tasks[2].State = proto.TaskReady
	x.srv.AddPlan(p)
	x.connect(t)
	x.click(t, "Work Board")
	x.hasAll(t, "With the supervisors", "the supervisor delivers its ready tasks: t1 and t3.")
}

// A task's assignment on the approval card wraps: the owner reads every
// dependency of a task before approving it (Codex review of #25, round 4).
func TestATasksAssignmentWrapsOnTheApprovalCard(t *testing.T) {
	x := newTestApp(t)
	p := x.seedPlan()
	var deps []string
	for i := 1; i <= 29; i++ {
		deps = append(deps, "t"+itoa(i+100))
	}
	p.Graph.Tasks[1].Deps = deps
	x.srv.AddPlan(p)
	x.tt.SetSize(720, 1200)
	x.connect(t)
	long := "Vera (vera) in frostyard/snow · after " + strings.Join(deps[:28], ", ") + " and " + deps[28]
	whole, ok := x.tt.Find(long)
	line, ok2 := x.tt.Find("Vera (vera) in frostyard/snow")
	if !ok || !ok2 {
		t.Fatalf("assignment found %v, one line found %v (texts %q)", ok, ok2, x.tt.Texts())
	}
	if whole.H < 1.5*line.H {
		t.Errorf("the assignment is %.0f high, %.1f lines: cut to one", whole.H, whole.H/line.H)
	}
}
