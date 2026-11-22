package state

import (
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// A sign-in that changes the models pocketd runs fetches the profiles too,
// whose model and problem pocketd decides from them; so does a fetch of
// the sign-ins that brings the change before its event (Codex review of
// #123, round 2).
func TestASignInChangeFetchesTheModelsAndProfiles(t *testing.T) {
	h := signInHarness(t)
	count := func(p string) int { return len(h.srv.RequestsTo(http.MethodGet, p)) }
	models, profiles := count(proto.PathModels), count(proto.PathProfiles)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	h.srv.EndSignIn(CopilotProvider, proto.FlowApproved, "octocat", "")
	h.loop.RunUntil(t, func() bool { return count(proto.PathProfiles) > profiles })
	h.settle(t)
	if count(proto.PathModels) <= models {
		t.Fatal("the sign-in's event fetched no models")
	}

	// pocketd signs out with no event this client sees; the fetch brings it.
	rows := pocketdtest.DefaultSignIns()
	h.srv.SetSignIns(rows)
	models, profiles = count(proto.PathModels), count(proto.PathProfiles)
	h.store.LoadSignIns()
	h.settle(t)
	if count(proto.PathModels) != models+1 || count(proto.PathProfiles) != profiles+1 {
		t.Fatalf("a fetch that brought a sign-out fetched %d models and %d profiles", count(proto.PathModels)-models, count(proto.PathProfiles)-profiles)
	}
	h.store.LoadSignIns()
	h.settle(t)
	if count(proto.PathModels) != models+1 {
		t.Fatal("a fetch that changed nothing fetched the models")
	}
}

// A start the owner canceled while it went, whose answer was lost, goes
// again with its request id once pocketd is heard from, and the flow it
// made is canceled: no code the owner gave up waits to be approved (Codex
// review of #123, round 2).
func TestACanceledStartWhoseAnswerWasLostIsCanceled(t *testing.T) {
	h := signInHarness(t)
	start := proto.Path(proto.PathSignIn, CopilotProvider)
	release := h.srv.HoldAndLoseAnswerOnce(http.MethodPost, start)
	h.store.StartSignIn(CopilotProvider)
	// The start made its flow, whose event comes before the lost answer.
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.ID == "flow-1"
	})
	h.store.CancelSignIn()
	release()
	cancel := proto.Path(proto.PathCancelSignIn, "flow-1")
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo(http.MethodPost, cancel)) == 1 })
	h.settle(t)
	ids := sentIDs(t, h, start)
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("the starts' request ids %q", ids)
	}
	if v := h.store.SignInSheet(); v.Phase != SheetClosed {
		t.Fatalf("the canceled sheet came back %+v", v)
	}
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.ID == "flow-1" && row.Flow.State == proto.FlowCanceled
	})
}

// A read of the sheet's flow answered after an event ended it, and after
// another device's flow took the provider's row, does not bring back the
// code: the sheet keeps the end the event brought (Codex review of #123,
// round 2).
func TestALateFlowReadDoesNotUndoAnEnd(t *testing.T) {
	h := signInHarness(t)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.ID == "flow-1"
	})
	h.settle(t)
	row, _ := h.store.Data.SignIn(CopilotProvider)
	waiting := *row.Flow
	read := proto.Path(proto.PathSignInFlow, "flow-1")
	release := h.srv.HoldAnswerOnce(http.MethodGet, read)
	none := row
	none.Flow = nil
	h.store.apply(proto.Event{Type: proto.EventSignIn, SignIn: &none})
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, read) })
	expired := row
	ended := waiting
	ended.State, ended.EndedAt = proto.FlowExpired, pocketdtest.Now
	expired.Flow = &ended
	h.store.apply(proto.Event{Type: proto.EventSignIn, SignIn: &expired})
	next := row
	next.Flow = &proto.SignInFlow{ID: "flow-9", Provider: CopilotProvider, State: proto.FlowWaiting}
	h.store.apply(proto.Event{Type: proto.EventSignIn, SignIn: &next})
	release()
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetFailed || v.Flow.ID != "flow-1" || v.Flow.State != proto.FlowExpired {
		t.Fatalf("the late read undid the end: %+v", v)
	}
}
