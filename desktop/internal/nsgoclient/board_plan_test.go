package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// The board shows where a plan is, on its thread's card and on its own
// card in the Plans section: its state, its revision and what its gate
// waits for; the Plans section lists its tasks, each with its thread and
// its state; and a task's thread names the plan it delivers.
// TestPlansOnTheBoardSnapshot holds this for the MyGo client, which draws
// all of it on the plan's thread.
func TestPlansOnTheBoard(t *testing.T) {
	s := pocketdtest.New()
	seedPlan(s)
	p := plan1()
	p.State, p.Graph.Approved = proto.PlanBuilding, true
	p.Graph.Tasks[0].State, p.Graph.Tasks[0].Thread = proto.TaskDelegated, 8
	p.Graph.Tasks[2].State = proto.TaskReady
	p.Waiting = []string{"A3 open"}
	s.AddPlan(p)
	s.AddThread(proto.Thread{ID: 8, Title: "P1 t1 · Make the signing key", Agent: "vera", Supervisor: 1, Repository: "frostyard/snow",
		Branch: "vera/p1-t1-make-the-signing-key-t8", Task: "Do it.", State: proto.ThreadOpen, Plan: 1, PlanTask: "t1",
		CreatedAt: at(-time.Hour), UpdatedAt: at(-2 * time.Minute)})
	app := signedInApp(t, s, 1230, 1000)
	app.Press("sidebar-panel >> text:Work board")
	// The plan's own thread takes no place on the desk.
	app.WaitFor("text:1 thread on the desk", 5*time.Second)
	// t3 is ready, but A3 holds every delivery: the plan's thread waits
	// for the owner.
	for _, says := range []string{`"Your move"`, `"P1 waits for your answer: A3 open."`, `"Plan P1"`, `"building"`, `"rev abababa"`, `"Waiting: A3 open"`} {
		app.WaitFor(card(7)+" >> text:"+says, 5*time.Second)
	}
	app.Find(card(8) + ` >> text:"Task t1 of plan P1 · Sign the ISO"`)
	app.Find(card(8) + ` >> text:"T8 · Vera · frostyard/snow on vera/p1-t1-make-the-signing-key-t8"`)
	// The Plans section: the plan's card, with the same head, and its
	// tasks.
	plan := "id:plan-1"
	for _, says := range []string{`"Plan P1"`, `"building"`, `"rev abababa"`, `"Waiting: A3 open"`,
		`"t1 · Make the signing key"`, `"T8"`, `"delegated"`, `"Vera (vera) in frostyard/snow"`,
		`"t2 · Sign the release ISO"`, `"waiting"`, `"Vera (vera) in frostyard/snow · after t1"`,
		`"t3 · Write the runbook"`, `"ready"`, `"Duncan (duncan) in its home"`} {
		app.Find(plan + " >> text:" + says)
	}
	// 2 pictures: the plan's thread in its lane, then the plan's card,
	// under the lanes.
	app.Screenshot("work-board-plan-thread")
	app.Scroll(plan, 500)
	app.Screenshot("work-board-plan")
}

// A plan's state shows in the MyGo client's words: a plan in review waits
// for the owner, and one done or dropped says so.
func TestAPlansStateOnTheBoard(t *testing.T) {
	for st, says := range map[proto.PlanState]string{
		proto.PlanReview:   "waits for you",
		proto.PlanDone:     "done",
		proto.PlanDropped:  "dropped",
		proto.PlanDrafting: string(proto.PlanDrafting),
		proto.PlanApproved: string(proto.PlanApproved),
	} {
		s := pocketdtest.New()
		seedPlan(s)
		p := plan1()
		p.State = st
		s.AddPlan(p)
		app := signedInApp(t, s, 1230, 800)
		app.Press("sidebar-panel >> text:Work board")
		app.WaitFor(`id:plan-1 >> text:"`+says+`"`, 5*time.Second)
		app.Find(card(7) + ` >> text:"` + says + `"`)
		// A draft's tasks have no state yet: they wait for the approval.
		if app.Exists(`id:plan-1 >> text:"waiting"`) {
			t.Errorf("%s: a task of a graph not approved shows a state", st)
		}
	}
}

// A plan's thread whose plan the client does not hold names the plan.
func TestAPlanThreadWhosePlanIsNotLoadedNamesIt(t *testing.T) {
	s := pocketdtest.New()
	seedPlan(s)
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor(card(7)+` >> text:"Plan P1"`, 5*time.Second)
	app.Find(card(7) + ` >> text:"Duncan plans it."`)
}
