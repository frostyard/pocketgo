package state

import (
	"context"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// signInHarness is a harness signed in, its sign-ins loaded.
func signInHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.SetSignIns(pocketdtest.DefaultSignIns())
	h.connect(t)
	h.store.LoadSignIns()
	h.settle(t)
	return h
}

// A start whose answer was lost keeps its request id, and once pocketd
// answers that the flow it started is gone, as after a restart, the id
// goes: the next Try Again starts another flow (Codex review of #123,
// round 1).
func TestAStartWhoseFlowIsGoneLetsItsRequestIDGo(t *testing.T) {
	h := signInHarness(t)
	start := proto.Path(proto.PathSignIn, CopilotProvider)
	h.srv.LoseAnswerOnce(http.MethodPost, start)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	h.srv.ForgetSignInFlows()
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetFailed {
		t.Fatalf("a start whose flow is gone: %+v", v)
	}
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetCode || v.Flow.ID != "flow-2" {
		t.Fatalf("Try Again after the flow was gone: %+v", v)
	}
	ids := sentIDs(t, h, start)
	if len(ids) != 3 || ids[0] != ids[1] || ids[2] == ids[1] {
		t.Fatalf("the starts' request ids %q", ids)
	}
}

// A sheet whose flow pocketd no longer has, as after a restart its
// sign-ins come back with no flow, ends with Try Again, instead of
// counting down a code that cannot work (Codex review of #123, round 1).
func TestASheetEndsWhenPocketdNoLongerHasItsFlow(t *testing.T) {
	h := signInHarness(t)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetCode {
		t.Fatalf("the sheet %+v", v)
	}
	// The start's event is in before pocketd restarts: one that came after
	// the fetch below began would win over its answer (frostyard/pocketgo#124).
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.ID == "flow-1"
	})
	h.settle(t)
	read := proto.Path(proto.PathSignInFlow, "flow-1")
	before := len(h.srv.RequestsTo(http.MethodGet, read))
	h.srv.ForgetSignInFlows()
	h.store.LoadSignIns()
	h.settle(t)
	h.loop.RunUntil(t, func() bool { return h.store.SignInSheet().Phase == SheetFailed })
	if n := len(h.srv.RequestsTo(http.MethodGet, read)) - before; n != 1 {
		t.Fatalf("the sheet read its flow %d times once it was gone", n)
	}
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetCode || v.Flow.ID != "flow-2" {
		t.Fatalf("Try Again %+v", v)
	}
}

// An ended sheet keeps its end when another device starts the provider's
// next flow: it never shows its earlier copy, waiting, again (Codex review
// of #123, round 1).
func TestAnEndedSheetKeepsItsEndWhenAnotherFlowStarts(t *testing.T) {
	h := signInHarness(t)
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	h.srv.EndSignIn(CopilotProvider, proto.FlowExpired, "", "expired")
	h.loop.RunUntil(t, func() bool { return h.store.SignInSheet().Phase == SheetFailed })
	other, err := proto.NewClient(h.srv.URL, pocketdtest.Token, &http.Client{Transport: h.srv.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.StartSignIn(context.Background(), CopilotProvider, proto.StartSignIn{RequestID: "other-device"}); err != nil {
		t.Fatal(err)
	}
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.ID == "flow-2"
	})
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetFailed || v.Flow.ID != "flow-1" || v.Text != "The code expired before it was approved." {
		t.Fatalf("the ended sheet after another flow started: %+v", v)
	}
}

// A flow approved while the provider was signed in already, as for
// another account, asks for the models again: its account offers its own
// (Codex review of #123, round 1).
func TestAnApprovedFlowAsksForTheModelsAgain(t *testing.T) {
	var d Data
	signedIn := proto.SignIn{Provider: CopilotProvider, State: proto.SignInSignedIn, Account: "octocat"}
	d.SetSignIns(d.Begin(), []proto.SignIn{signedIn})
	waiting := signedIn
	waiting.Flow = &proto.SignInFlow{ID: "f2", State: proto.FlowWaiting}
	if got := d.Apply(proto.Event{Type: proto.EventSignIn, SignIn: &waiting}); got != RefetchNone {
		t.Fatalf("a waiting flow asks %v", got)
	}
	approved := signedIn
	approved.Account = "hubot"
	approved.Flow = &proto.SignInFlow{ID: "f2", State: proto.FlowApproved, Account: "hubot"}
	if got := d.Apply(proto.Event{Type: proto.EventSignIn, SignIn: &approved}); got != RefetchModels {
		t.Fatalf("an approval while signed in asks %v", got)
	}
	if got := d.Apply(proto.Event{Type: proto.EventSignIn, SignIn: &approved}); got != RefetchNone {
		t.Fatalf("the same approval again asks %v", got)
	}
}
