package state

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// The sign-ins follow their events: an event's row wins over the copy of
// a fetch begun before it, a failed fetch keeps the rows, and a provider
// whose state changed asks for the models again (ADR-0062).
func TestSignInsFollowTheirEvents(t *testing.T) {
	var d Data
	if status, _ := d.SignInsStatus(); status != ListLoading {
		t.Fatalf("a new Data's sign-ins are %v", status)
	}
	f := d.Begin()
	copilot := proto.SignIn{Provider: CopilotProvider, Name: "GitHub Copilot", State: proto.SignInNone, DeviceFlow: true}
	signedIn := copilot
	signedIn.State, signedIn.Account = proto.SignInSignedIn, "octocat"
	if got := d.Apply(proto.Event{Type: proto.EventSignIn, SignIn: &signedIn}); got != RefetchModels {
		t.Fatalf("a provider the client did not know asks %v", got)
	}
	d.SetSignIns(f, []proto.SignIn{copilot, {Provider: "anthropic", State: proto.SignInSignedIn}})
	if row, _ := d.SignIn(CopilotProvider); row.State != proto.SignInSignedIn || len(d.SignIns()) != 2 || d.SignIns()[0].Provider != CopilotProvider {
		t.Fatalf("the fetch begun before the event undid it: %+v", d.SignIns())
	}
	flowing := signedIn
	flowing.Flow = &proto.SignInFlow{ID: "f1", State: proto.FlowWaiting}
	if got := d.Apply(proto.Event{Type: proto.EventSignIn, SignIn: &flowing}); got != RefetchNone {
		t.Fatalf("a flow's start asks %v", got)
	}
	if got := d.Apply(proto.Event{Type: proto.EventSignIn, SignIn: &copilot}); got != RefetchModels {
		t.Fatalf("a sign-out asks %v", got)
	}
	g := d.Begin()
	d.SetSignInsError(g, "down")
	if status, err := d.SignInsStatus(); status != ListFailed || err != "down" || len(d.SignIns()) != 2 {
		t.Fatalf("a failed fetch: %v %q %+v", status, err, d.SignIns())
	}
	d.SetSignIns(g, []proto.SignIn{signedIn})
	if row, _ := d.SignIn(CopilotProvider); row.State != proto.SignInSignedIn || len(d.SignIns()) != 1 {
		t.Fatalf("a fetch begun after the event: %+v", d.SignIns())
	}

	if SignInCaption(signedIn) != "Signed in as @octocat" || SignInCaption(copilot) != "Not signed in" ||
		SignInCaption(proto.SignIn{Provider: "anthropic", State: proto.SignInSignedIn}) != "Signed in. Sign in on the service host: pocketd login anthropic" ||
		SignInCaption(proto.SignIn{Provider: CopilotProvider, State: proto.SignInExpired, DeviceFlow: true}) != "The sign-in expired: sign in again" {
		t.Fatal("SignInCaption")
	}
	if url, shown, ok := DevicePage(CopilotProvider); !ok || url != "https://github.com/login/device" || shown != "github.com/login/device" {
		t.Fatalf("Copilot's device page %q %q %v", url, shown, ok)
	}
	if _, _, ok := DevicePage("anthropic"); ok {
		t.Fatal("anthropic has a device page")
	}
	if FormatLeft(4*time.Minute+5*time.Second) != "4:05" || FormatLeft(0) != "0:00" || FormatLeft(15*time.Minute) != "15:00" {
		t.Fatal("FormatLeft")
	}
}

// The sheet follows the flow its start answered: the code and its time
// left by the desktop's clock while it waits, the account and the models
// once approved, which the store fetches again, and why it ended
// otherwise. A start that failed keeps its request id for Try Again; one
// that succeeded lets it go. Cancel sends the flow's cancel while it
// waits, and nothing once it ended (ADR-0062).
func TestTheSheetFollowsItsFlow(t *testing.T) {
	h := newHarness(t)
	h.srv.SetSignIns(pocketdtest.DefaultSignIns())
	h.connect(t)
	h.store.LoadSignIns()
	h.settle(t)
	start := proto.Path(proto.PathSignIn, CopilotProvider)
	if v := h.store.SignInSheet(); v.Phase != SheetClosed {
		t.Fatalf("a sheet before a start: %+v", v)
	}

	h.srv.FailOnce(http.MethodPost, start, http.StatusBadGateway)
	h.store.StartSignIn(CopilotProvider)
	if v := h.store.SignInSheet(); v.Phase != SheetStarting {
		t.Fatalf("the sheet while the start goes: %+v", v)
	}
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetFailed || !strings.Contains(v.Text, "did not start") {
		t.Fatalf("a failed start: %+v", v)
	}
	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	v := h.store.SignInSheet()
	if v.Phase != SheetCode || v.Flow.UserCode != pocketdtest.UserCode || v.Left != 15*time.Minute || v.Provider != "GitHub Copilot" || v.ProviderID != CopilotProvider {
		t.Fatalf("the waiting sheet %+v", v)
	}
	h.now = h.now.Add(time.Minute)
	if v := h.store.SignInSheet(); v.Left != 14*time.Minute {
		t.Fatalf("the time left after a minute: %s", v.Left)
	}
	copilot := proto.Model{ModelRef: proto.ModelRef{Provider: CopilotProvider, ID: "gpt-5"}, Name: "GPT-5"}
	h.srv.EndSignIn(CopilotProvider, proto.FlowApproved, "octocat", "", copilot)
	h.loop.RunUntil(t, func() bool { return h.store.SignInSheet().Phase == SheetSignedIn })
	h.settle(t)
	if v := h.store.SignInSheet(); v.Text != "Signed in as @octocat. GitHub Copilot models are now available." {
		t.Fatalf("the approved sheet says %q", v.Text)
	}
	if models, _ := h.store.Models(); !slices.ContainsFunc(models, func(m proto.Model) bool { return m.ModelRef == copilot.ModelRef }) {
		t.Fatalf("the models after the sign-in: %+v", models)
	}
	if row, _ := h.store.Data.SignIn(CopilotProvider); row.State != proto.SignInSignedIn || row.Account != "octocat" {
		t.Fatalf("the row after the sign-in %+v", row)
	}
	h.store.CancelSignIn()
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetClosed || len(h.srv.RequestsTo(http.MethodPost, "/v1/signins/flows/flow-1/cancel")) != 0 {
		t.Fatalf("closing an approved sheet: %+v", v)
	}

	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	ids := sentIDs(t, h, start)
	if len(ids) != 3 || ids[0] != ids[1] || ids[2] == ids[1] {
		t.Fatalf("the starts' request ids %q", ids)
	}
	v = h.store.SignInSheet()
	if v.Phase != SheetCode || v.Flow.ID != "flow-2" {
		t.Fatalf("the second flow %+v", v)
	}
	h.store.CancelSignIn()
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetClosed || len(h.srv.RequestsTo(http.MethodPost, "/v1/signins/flows/flow-2/cancel")) != 1 {
		t.Fatalf("the cancel of a waiting flow: %+v", v)
	}
	// The cancel's event brings the canceled flow.
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.ID == "flow-2" && row.Flow.State == proto.FlowCanceled
	})

	h.store.StartSignIn(CopilotProvider)
	h.settle(t)
	h.srv.EndSignIn(CopilotProvider, proto.FlowDenied, "", "The code was denied at https://github.com/login/device.")
	h.loop.RunUntil(t, func() bool { return h.store.SignInSheet().Phase == SheetFailed })
	if v := h.store.SignInSheet(); v.Text != "The code was denied on GitHub." {
		t.Fatalf("the denied sheet says %q", v.Text)
	}
	h.store.CancelSignIn()

	h.store.SignOutOf(CopilotProvider)
	if !h.store.SigningOut(CopilotProvider).Busy || h.store.MaySignIn(proto.SignIn{Provider: CopilotProvider, DeviceFlow: true}) {
		t.Fatal("the sign-out is not on its way")
	}
	h.settle(t)
	if row, _ := h.store.Data.SignIn(CopilotProvider); row.State != proto.SignInNone || h.store.SigningOut(CopilotProvider).Err != "" {
		t.Fatalf("the row after the sign-out %+v", row)
	}
	if len(h.srv.RequestsTo(http.MethodDelete, start)) != 1 {
		t.Fatal("no sign-out went")
	}
	if h.store.MaySignIn(proto.SignIn{Provider: "anthropic"}) {
		t.Fatal("a provider the desktop does not sign in to offers Sign In…")
	}
}

// Cancel at any step cancels the flow on pocketd: a sheet closed while its
// start is on its way shows no more, and the flow the start answers is
// canceled; one opened again before the answer shows that flow, which
// stays.
func TestASheetClosedWhileItStartsCancelsItsFlow(t *testing.T) {
	h := newHarness(t)
	h.srv.SetSignIns(pocketdtest.DefaultSignIns())
	h.connect(t)
	h.store.LoadSignIns()
	h.settle(t)
	start := proto.Path(proto.PathSignIn, CopilotProvider)

	release := h.srv.HoldAnswerOnce(http.MethodPost, start)
	h.store.StartSignIn(CopilotProvider)
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, start) })
	h.store.CancelSignIn()
	if v := h.store.SignInSheet(); v.Phase != SheetClosed {
		t.Fatalf("a sheet closed while it starts shows %+v", v)
	}
	release()
	h.settle(t)
	if n := len(h.srv.RequestsTo(http.MethodPost, "/v1/signins/flows/flow-1/cancel")); n != 1 {
		t.Fatalf("the started flow got %d cancels", n)
	}
	h.loop.RunUntil(t, func() bool {
		row, _ := h.store.Data.SignIn(CopilotProvider)
		return row.Flow != nil && row.Flow.State == proto.FlowCanceled
	})
	if v := h.store.SignInSheet(); v.Phase != SheetClosed {
		t.Fatalf("the sheet came back %+v", v)
	}

	release = h.srv.HoldAnswerOnce(http.MethodPost, start)
	h.store.StartSignIn(CopilotProvider)
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, start) })
	h.store.CancelSignIn()
	h.store.StartSignIn(CopilotProvider)
	if v := h.store.SignInSheet(); v.Phase != SheetStarting {
		t.Fatalf("a sheet opened again while it starts shows %+v", v)
	}
	release()
	h.settle(t)
	if v := h.store.SignInSheet(); v.Phase != SheetCode || v.Flow.ID != "flow-2" {
		t.Fatalf("the sheet opened again %+v", v)
	}
	if n := len(h.srv.RequestsTo(http.MethodPost, "/v1/signins/flows/flow-2/cancel")); n != 0 {
		t.Fatalf("the flow of a sheet opened again got %d cancels", n)
	}
	if n := len(h.srv.RequestsTo(http.MethodPost, start)); n != 2 {
		t.Fatalf("%d starts went, want one each", n)
	}
}
