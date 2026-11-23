package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/proto"
)

// openWorkerTestSession waits for a session's transcript before checking its composer.
func (x *testApp) openWorkerTestSession(t *testing.T, id int64) {
	t.Helper()
	x.a.openSession(id)
	x.until(t, func() bool {
		o := x.a.st.Data.Open()
		return o != nil && o.ID == id && o.Loaded
	})
}

// workerCardsShown counts cards by their accessible labels, not sidebar rows.
func (x *testApp) workerCardsShown() int {
	n := 0
	for _, node := range x.tt.AccessAll() {
		if node.Role == ui.RoleButton && strings.HasPrefix(node.Name, "Open worker ") {
			n++
		}
	}
	return n
}

func TestAWorkerCardOpensItsSessionAndBackOpensTheSpecialistSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.seedWorker()
	x.connect(t)
	x.untilProfiles(t)
	x.openWorkerTestSession(t, 2)
	x.hasAll(t, "T7 · worker", "running", "deepseek-flash")
	if n := x.workerCardsShown(); n != 1 {
		t.Fatalf("cards = %d, want 1", n)
	}
	x.snapshot(t, "worker-cards")
	x.click(t, "Open worker T7 · worker, running, deepseek-flash")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == 6 && o.Loaded })
	x.hasAll(t, "Back to specialist", "Worker:", "T7 · worker", "deepseek-flash")
	if n := x.workerCardsShown(); n != 0 {
		t.Errorf("worker has %d cards", n)
	}
	x.snapshot(t, "worker-back")
	x.click(t, "Back to the specialist's session, T7 · Fix the flaky CI in nsl")
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 2 })
	x.openWorkerTestSession(t, 4)
	if x.tt.HasText("Back to specialist") || x.workerCardsShown() != 0 {
		t.Error("unrelated session shows worker controls")
	}
}

func TestMoreThanThreeWorkersFoldBehindMore(t *testing.T) {
	x := newTestApp(t)
	x.seedAgents()
	x.seedWorker()
	for i := 7; i <= 10; i++ {
		x.srv.AddSession(proto.Session{ID: int64(i), Title: fmt.Sprintf("Worker %d", i), Route: "worker-default", StartedBy: 2,
			State: proto.SessionIdle, UpdatedAt: at(0)})
	}
	x.connect(t)
	x.untilProfiles(t)
	x.openWorkerTestSession(t, 2)
	if n := x.workerCardsShown(); n != 3 {
		t.Fatalf("collapsed cards = %d, want 3", n)
	}
	x.hasAll(t, "+2 more")
	x.click(t, "Show 2 more workers")
	if n := x.workerCardsShown(); n != 5 {
		t.Fatalf("expanded cards = %d, want 5", n)
	}
	x.hasAll(t, "Show fewer")
	x.click(t, "Show fewer workers")
	if n := x.workerCardsShown(); n != 3 {
		t.Fatalf("folded cards = %d, want 3", n)
	}
	x.hasAll(t, "+2 more")
}

func TestWorkerCardsOnThePhoneSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.phoneApp(t, true, nil)
	x.seedAgents()
	x.seedWorker()
	x.a.st.Refresh()
	x.untilLoaded(t)
	x.untilProfiles(t)
	x.openWorkerTestSession(t, 2)
	x.hasAll(t, "T7 · worker", "running", "deepseek-flash")
	x.within(t, ui.RoleButton, "Open worker T7 · worker, running, deepseek-flash")
	x.snapshot(t, "phone-worker-cards")
	x.tap(t, "Open worker T7 · worker, running, deepseek-flash")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == 6 && o.Loaded })
	x.hasAll(t, "Back to specialist", "Worker:", "T7 · worker", "deepseek-flash")
	x.within(t, ui.RoleButton, "Back to the specialist's session, T7 · Fix the flaky CI in nsl")
	x.snapshot(t, "phone-worker-back")
	x.tap(t, "Back to the specialist's session, T7 · Fix the flaky CI in nsl")
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 2 })
}

func TestWorkerCardsOnTheIPadSnapshot(t *testing.T) {
	x := newTestApp(t)
	loads := 0
	x.iPadApp(t, &loads)
	x.seedAgents()
	x.seedWorker()
	x.connect(t)
	x.untilProfiles(t)
	x.openWorkerTestSession(t, 2)
	x.hasAll(t, "T7 · worker", "running", "deepseek-flash")
	x.snapshot(t, "ipad-worker-cards")
	x.click(t, "Open worker T7 · worker, running, deepseek-flash")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == 6 && o.Loaded })
	x.hasAll(t, "Back to specialist", "Worker:", "T7 · worker", "deepseek-flash")
	x.click(t, "Back to the specialist's session, T7 · Fix the flaky CI in nsl")
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 2 })
}
