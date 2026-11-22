package state

import (
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// A sheet whose start failed with no answer, closed, sends the start
// again with its request id and cancels the flow pocketd names: the code
// the start made waits for nobody (Codex review of #123, round 3).
func TestClosingAFailedStartCancelsTheFlowItMade(t *testing.T) {
	h := signInHarness(t)
	start := proto.Path(proto.PathSignIn, CopilotProvider)
	h.srv.LoseAnswerOnce(http.MethodPost, start)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetFailed {
		t.Fatalf("a start with no answer: %+v", v)
	}
	h.store.CancelSignIn()
	cancel := proto.Path(proto.PathCancelSignIn, "flow-1")
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo(http.MethodPost, cancel)) == 1 })
	h.settle(t)
	if ids := sentIDs(t, h, start); len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("the starts' request ids %q", ids)
	}
}

// The owner's Sign In… while a canceled start's replay is on its way wins:
// the sheet shows the flow, and the replay's answer cancels nothing
// (Codex review of #123, round 3).
func TestReopeningWhileACanceledStartReplaysKeepsTheFlow(t *testing.T) {
	h := signInHarness(t)
	start := proto.Path(proto.PathSignIn, CopilotProvider)
	h.srv.LoseAnswerOnce(http.MethodPost, start)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	release := h.srv.HoldAnswerOnce(http.MethodPost, start)
	h.store.CancelSignIn()
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, start) })
	h.store.StartSignIn(CopilotProvider)
	release()
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetCode || v.Flow.ID != "flow-1" {
		t.Fatalf("the reopened sheet %+v", v)
	}
	if n := len(h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathCancelSignIn, "flow-1"))); n != 0 {
		t.Fatalf("the replay canceled the reopened flow %d times", n)
	}
}

// A flow never goes back from its end to waiting: an ended copy, as a
// retried start answers it, wins over a waiting row the stream left
// (Codex review of #123, round 3).
func TestAnEndedFlowWinsOverAWaitingRow(t *testing.T) {
	h := signInHarness(t)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.ID == "flow-1"
	})
	ended := h.store.sheet.flow
	ended.State = proto.FlowExpired
	h.store.sheet.flow = ended
	h.store.settleSheet()
	if v := h.store.SignInSheet(); v.Phase != SheetFailed || v.Flow.State != proto.FlowExpired {
		t.Fatalf("a waiting row brought the code back: %+v", v)
	}
	if laterFlow(ended, proto.SignInFlow{ID: "flow-1", State: proto.FlowWaiting}) != ended {
		t.Fatal("laterFlow")
	}
}

// A fetch of the sign-ins the client had none of fetches the models and
// profiles: another device's sign-out may be what it brings first (Codex
// review of #123, round 3).
func TestTheFirstFetchOfTheSignInsFetchesTheModels(t *testing.T) {
	h := newHarness(t)
	h.srv.SetSignIns(pocketdtest.DefaultSignIns())
	h.connect(t)
	count := func(p string) int { return len(h.srv.RequestsTo(http.MethodGet, p)) }
	models, profiles := count(proto.PathModels), count(proto.PathProfiles)
	h.store.LoadSignIns()
	h.settle(t)
	if count(proto.PathModels) != models+1 || count(proto.PathProfiles) != profiles+1 {
		t.Fatalf("the first fetch fetched %d models and %d profiles", count(proto.PathModels)-models, count(proto.PathProfiles)-profiles)
	}
}

// A sign-out's answer and a fetch of the sign-ins go by when they began:
// a fetch begun before the sign-out never undoes it, and a sign-out's
// answer never undoes a fetch begun after it (Codex review of #123, round
// 3).
func TestASignOutAndAFetchGoByWhenTheyBegan(t *testing.T) {
	var d Data
	signedIn := proto.SignIn{Provider: CopilotProvider, State: proto.SignInSignedIn, DeviceFlow: true}
	none := signedIn
	none.State = proto.SignInNone
	d.SetSignIns(d.Begin(), []proto.SignIn{signedIn})
	fetch, out := d.Begin(), d.Begin()
	d.signedOut(out, none)
	d.SetSignIns(fetch, []proto.SignIn{signedIn})
	if row, _ := d.SignIn(CopilotProvider); row.State != proto.SignInNone {
		t.Fatalf("an older fetch undid the sign-out: %+v", row)
	}
	out, fetch = d.Begin(), d.Begin()
	d.SetSignIns(fetch, []proto.SignIn{signedIn})
	d.signedOut(out, none)
	if row, _ := d.SignIn(CopilotProvider); row.State != proto.SignInSignedIn {
		t.Fatalf("an older sign-out undid a newer fetch: %+v", row)
	}
}
