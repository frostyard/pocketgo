package state

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestASessionsWorkersAreItsCardsActiveFirstNewestFirst(t *testing.T) {
	parent := agentSession(2, "duncan", "Specialist", 0)
	running := workerSession(11, 2, "Running", -time.Hour)
	running.State = proto.SessionRunning
	waiting := workerSession(13, 2, "Waiting", 0)
	waiting.UpdatedAt = pocketdtest.Now.Add(-2 * time.Hour)
	waiting.State = proto.SessionWaiting
	notWorker := session(6, "Not a worker", 0)
	notWorker.StartedBy = 2
	d := sidebarData(team, parent, session(5, "Other starter", 0),
		workerSession(10, 2, "Idle", time.Hour), running,
		workerSession(12, 2, "", 0), waiting,
		workerSession(20, 5, "Other worker", 0), notWorker)
	want := []WorkerCard{
		{ID: 13, Title: "Waiting", State: proto.SessionWaiting, Model: running.Model},
		{ID: 11, Title: "Running", State: proto.SessionRunning, Model: running.Model},
		{ID: 12, Title: UntitledSession, State: proto.SessionIdle, Model: running.Model},
		{ID: 10, Title: "Idle", State: proto.SessionIdle, Model: running.Model},
	}
	if got := d.WorkersOf(2); !reflect.DeepEqual(got, want) {
		t.Fatalf("cards = %+v, want %+v", got, want)
	}
	d.SetApprovals(d.Begin(), []proto.Approval{approval("a1", 12, "c1", -time.Minute)})
	want = []WorkerCard{want[0], {ID: 12, Title: UntitledSession, State: proto.SessionWaiting, Model: running.Model}, want[1], want[3]}
	if got := d.WorkersOf(2); !reflect.DeepEqual(got, want) {
		t.Fatalf("cards with approval = %+v, want %+v", got, want)
	}
}

func TestNoCardsWithoutWorkersOrForAWorkerOrAMissingStarter(t *testing.T) {
	d := sidebarData(team, session(2, "No workers", 0),
		workerSession(3, 2, "Worker", 0), workerSession(4, 3, "Nested", 0),
		workerSession(5, 99, "Missing starter", 0), workerSession(6, 0, "No starter", 0),
		session(7, "No workers", 0))
	for _, id := range []int64{0, 3, 7, 99} {
		if got := d.WorkersOf(id); got != nil {
			t.Errorf("WorkersOf(%d) = %+v, want nil", id, got)
		}
	}
}

func TestWorkerCardsCapAtThreeWithMore(t *testing.T) {
	for _, n := range []int{0, 1, 3, 4, 6} {
		for _, expanded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/expanded=%t", n, expanded), func(t *testing.T) {
				rows := []proto.Session{session(2, "Starter", 0)}
				var all []WorkerCard
				for i := 0; i < n; i++ {
					id := int64(10 + i)
					rows = append(rows, workerSession(id, 2, "Worker", 0))
					all = append([]WorkerCard{{ID: id, Title: "Worker", State: proto.SessionIdle, Model: rows[len(rows)-1].Model}}, all...)
				}
				d := sidebarData(team, rows...)
				want := WorkerCards{Cards: all}
				if n > 3 {
					if expanded {
						want.Fewer = true
					} else {
						want.Cards = all[:3]
						want.More = n - 3
					}
				}
				if got := d.WorkerCards(2, expanded); !reflect.DeepEqual(got, want) {
					t.Fatalf("cards = %+v, want %+v", got, want)
				}
			})
		}
	}
}

func TestMoreWorkersLabel(t *testing.T) {
	for n, want := range map[int]string{1: "+1 more", 5: "+5 more"} {
		if got := MoreWorkersLabel(n); got != want {
			t.Errorf("MoreWorkersLabel(%d) = %q, want %q", n, got, want)
		}
	}
	if got := WorkerCardLabel(WorkerCard{Title: "T7 · worker", State: proto.SessionRunning}); got != "Open worker T7 · worker, running" {
		t.Errorf("WorkerCardLabel = %q", got)
	}
	card := WorkerCard{Title: "T7 · worker", State: proto.SessionRunning, Model: proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}}
	if got := WorkerCardLabel(card); got != "Open worker T7 · worker, running, deepseek-flash" {
		t.Errorf("WorkerCardLabel with model = %q", got)
	}
}

func TestACardGoesWhenItsWorkerIsGone(t *testing.T) {
	parent := agentSession(2, "duncan", "Specialist", 0)
	worker := workerSession(3, 2, "Worker", 0)
	d := sidebarData(team, parent, worker)
	if got := d.WorkersOf(2); len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("initial cards = %+v", got)
	}
	d.SetSessions(d.Begin(), []proto.Session{parent})
	if got := d.WorkersOf(2); got != nil {
		t.Fatalf("cards after worker gone = %+v", got)
	}
	d.SetSessions(d.Begin(), []proto.Session{worker})
	if got, ok := d.ParentOf(3); ok || !reflect.DeepEqual(got, proto.Session{}) {
		t.Fatalf("parent after starter gone = %+v, %t", got, ok)
	}
}

func TestOnlyAWorkerSessionHasAParentLink(t *testing.T) {
	parent := agentSession(2, "duncan", "Specialist", 0)
	parent.StartedBy = 1
	owner := session(1, "Owner", 0)
	owner.StartedBy = 2
	review := session(8, "Review", 0)
	review.Route = "review-default"
	review.StartedBy = 2
	d := sidebarData(team, owner, parent, workerSession(3, 2, "Worker", 0),
		workerSession(4, 99, "Missing starter", 0), workerSession(5, 3, "Nested", 0),
		workerSession(6, 0, "No starter", 0), review)
	if got, ok := d.ParentOf(3); !ok || !reflect.DeepEqual(got, parent) {
		t.Fatalf("ParentOf(3) = %+v, %t, want %+v, true", got, ok, parent)
	}
	for _, id := range []int64{0, 1, 2, 4, 5, 6, 8, 99} {
		if got, ok := d.ParentOf(id); ok || !reflect.DeepEqual(got, proto.Session{}) {
			t.Errorf("ParentOf(%d) = %+v, %t, want zero, false", id, got, ok)
		}
	}
}

func TestAWorkersModelShowsAndANormalSessionsDoesNot(t *testing.T) {
	model := proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}
	parent := agentSession(2, "duncan", "Specialist", 0)
	worker := workerSession(3, 2, "T7 · worker", 0)
	worker.Model = model
	orphan := workerSession(4, 99, "Orphan", 0)
	orphan.Model = model
	review := session(5, "Review", 0)
	review.Route = "cross-review"
	empty := workerSession(6, 2, "", 0)
	zero := workerSession(0, 2, "Invalid zero ID", 0)
	d := sidebarData(team, session(1, "Owner", 0), parent, worker, orphan, review, empty, zero)
	want := WorkerHeader{Parent: 2, ParentTitle: "Specialist", Title: "T7 · worker", Model: model}
	if got, ok := d.WorkerHeaderOf(3); !ok || got != want {
		t.Errorf("WorkerHeaderOf(3) = %+v, %t, want %+v, true", got, ok, want)
	}
	if got := ModelName(model); got != "deepseek-flash" {
		t.Errorf("ModelName = %q", got)
	}
	if got := ModelName(proto.ModelRef{Provider: "deepseek"}); got != "" {
		t.Errorf("ModelName without ID = %q", got)
	}
	found := false
	for _, card := range d.WorkersOf(2) {
		if card.ID == 3 {
			found = true
			if card.Model != model {
				t.Errorf("card model = %+v, want %+v", card.Model, model)
			}
		}
	}
	if !found {
		t.Error("worker card missing")
	}
	want = WorkerHeader{Title: "Orphan", Model: model}
	if got, ok := d.WorkerHeaderOf(4); !ok || got != want {
		t.Errorf("orphan header = %+v, %t, want %+v, true", got, ok, want)
	}
	if got, ok := d.WorkerHeaderOf(6); !ok || got.Title != UntitledSession {
		t.Errorf("untitled worker header = %+v, %t", got, ok)
	}
	for _, id := range []int64{0, 1, 2, 5, 99} {
		if got, ok := d.WorkerHeaderOf(id); ok || got != (WorkerHeader{}) {
			t.Errorf("WorkerHeaderOf(%d) = %+v, %t, want zero, false", id, got, ok)
		}
	}
	parent.Title = ""
	d.SetSessions(d.Begin(), []proto.Session{worker, parent})
	if got, ok := d.WorkerHeaderOf(3); !ok || got.ParentTitle != UntitledSession {
		t.Errorf("untitled parent header = %+v, %t", got, ok)
	}
}
