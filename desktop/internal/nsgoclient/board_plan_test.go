package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// The board shows a plan on its own thread's card, where the MyGo client
// shows it, and in no section of its own: its state, its revision and
// what its gate waits for, then its tasks, each with who does it, where
// and after which, its thread and its state; and a task's thread names
// the plan it delivers. TestPlansOnTheBoardSnapshot holds this for the
// MyGo client. The plan's text and each task's checks, which only this
// client shows, stay behind Show the Plan, closed at first.
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
	plan := groupOf("owner") + " >> " + card(7)
	for _, says := range []string{`"open"`, `"P1 waits for your answer: A3 open."`, `"Plan P1"`, `"building"`, `"rev abababa"`, `"Waiting: A3 open"`} {
		app.WaitFor(plan+" >> text:"+says, 5*time.Second)
	}
	// Its tasks, each a row of the card.
	for task, says := range map[string][]string{
		"t1": {`"t1 · Make the signing key"`, `"T8"`, `"delegated"`, `"Vera (vera) in frostyard/snow"`},
		"t2": {`"t2 · Sign the release ISO"`, `"waiting"`, `"Vera (vera) in frostyard/snow · after t1"`},
		"t3": {`"t3 · Write the runbook"`, `"ready"`, `"Duncan (duncan) in its home"`},
	} {
		for _, text := range says {
			app.Find(plan + " >> id:plan-1-task-" + task + " >> text:" + text)
		}
	}
	app.Find(groupOf("specialists") + " >> " + card(8) + ` >> text:"Task t1 of plan P1 · Sign the ISO"`)
	app.Find(card(8) + ` >> text:"T8 · Vera · frostyard/snow on vera/p1-t1-make-the-signing-key-t8"`)
	// No section of the board lists the plans, and no plan has a card of
	// its own while its thread's card shows it.
	if app.Exists("label:Plans") || app.Exists("id:plan-1") || app.Exists("label:Plans with no open thread") {
		t.Error("the board shows a plan outside its thread's card")
	}
	// Its text and its tasks' checks are folded.
	text, check := `text:"Sign each release with the TPM key; keep the public key in the repository."`, `text:"• The key is in the TPM"`
	if app.Exists(text) || app.Exists(check) || app.Exists("text:Approve plan") {
		t.Fatal("the plan's text or checks show before the owner opens Show the Plan")
	}
	app.Screenshot("work-board-plan-thread")
	app.Press(plan + " >> label:Show the Plan")
	app.WaitFor(plan+" >> "+text, 3*time.Second)
	for _, says := range []string{`"• The key is in the TPM"`, `"• The ISO verifies"`, `"• It names every step"`} {
		app.Find(plan + " >> text:" + says)
	}
	app.Screenshot("work-board-plan")
	// The fold stays open when the plan changes, and closes at a press.
	p.Graph.Tasks[1].State = proto.TaskReady
	s.Emit(proto.Event{Type: proto.EventPlan, Plan: &p})
	app.WaitFor(plan+` >> id:plan-1-task-t2 >> text:"ready"`, 5*time.Second)
	app.Find(plan + " >> " + check)
	app.Press(plan + " >> label:Show the Plan")
	app.WaitForGone(check, 3*time.Second)
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
		app.WaitFor(card(7)+` >> text:"`+says+`"`, 5*time.Second)
		app.Find(card(7) + ` >> text:"t1 · Make the signing key"`)
		// A draft's tasks have no state yet: they wait for the approval.
		if app.Exists(card(7) + ` >> text:"waiting"`) {
			t.Errorf("%s: a task of a graph not approved shows a state", st)
		}
	}
}

// A plan that waits for the owner's approval offers the Inbox on its
// thread's card, where its card has Approve: the board itself approves
// nothing.
func TestAPlanToApproveOffersTheInboxOnItsThread(t *testing.T) {
	s := pocketdtest.New()
	seedPlan(s)
	s.AddPlan(plan1())
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor(groupOf("owner")+" >> "+card(7)+` >> text:"P1 waits for your approval."`, 5*time.Second)
	if app.Exists("text:Approve plan") {
		t.Error("the board offers to approve a plan")
	}
	app.Press(card(7) + " >> text:Open the Inbox")
	app.WaitFor("text:Approve plan", 5*time.Second)
}

// A plan that waits for the owner keeps a card on the board when no open
// thread's card shows it (state.PlansOffTheBoard): its thread is closed,
// or is not among the threads loaded. The card says which, and goes to
// the Inbox.
func TestAPlanThatWaitsWithNoOpenThreadKeepsACard(t *testing.T) {
	for _, c := range []struct {
		name  string
		seed  func(s *pocketdtest.Server)
		where string
	}{
		{"its thread is closed", func(s *pocketdtest.Server) {
			s.AddThread(proto.Thread{ID: 7, Title: "Sign the ISO", Agent: "duncan", Supervisor: 1, State: proto.ThreadClosed, Plan: 1})
		}, "Its thread, T7, is closed."},
		{"its thread is not loaded", func(s *pocketdtest.Server) {}, "Its thread, T7, is not among the threads loaded."},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := pocketdtest.New()
			s.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan", Role: "agent"})
			s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "agent"})
			c.seed(s)
			s.AddPlan(plan1())
			app := signedInApp(t, s, 1230, 800)
			app.Press("sidebar-panel >> text:Work board")
			app.WaitFor("label:Plans with no open thread", 5*time.Second)
			for _, says := range []string{`"Sign the ISO"`, `"P1 waits for your approval. ` + c.where + `"`, `"Plan P1"`, `"waits for you"`, `"t1 · Make the signing key"`} {
				app.Find("id:plan-1 >> text:" + says)
			}
			app.Find("id:plan-1 >> label:Show the Plan")
			app.Press("id:plan-1 >> text:Open the Inbox")
			app.WaitFor("text:Approve plan", 5*time.Second)
		})
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
	if app.Exists("label:Show the Plan") {
		t.Error("a plan the client does not hold offers its text")
	}
}
