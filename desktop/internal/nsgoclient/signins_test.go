package nsgoclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// TestTheSignInsAndTheDeviceFlow checks pocketd's sign-ins in Settings:
// each provider with what it is signed in as, Sign in… for the one pocketd
// signs in by a device flow, the flow's steps (the code, the device page,
// the wait with the code's time left, how it ended), Sign out behind its
// question, and a denied code with Try Again (ADR-0062). It holds for the
// NSGo client what TestTheSignInsPageAndItsSheetSnapshot holds for MyGo's.
func TestTheSignInsAndTheDeviceFlow(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 900)
	app.Press("label:Settings")
	app.Press("text:Accounts")
	app.WaitFor("text:GitHub Copilot", 5*time.Second)
	app.Find("id:sign-in-github-copilot >> text:Not signed in")
	app.WaitFor("text:Sign in on the service host: pocketd login anthropic", 5*time.Second)

	app.Press("label:Sign in to GitHub Copilot")
	app.WaitFor("text:"+pocketdtest.UserCode, 5*time.Second)
	app.Find("text:Open github.com/login/device")
	app.Find("text:On another computer, go to https://github.com/login/device and enter the code.")
	app.WaitFor("text:Waiting for approval… ", 5*time.Second)
	app.Screenshot("settings-sign-in-code")
	app.Press("text:Copy code")
	if app.Clipboard != pocketdtest.UserCode {
		t.Errorf("Copy code put %q on the clipboard", app.Clipboard)
	}

	gpt := proto.Model{ModelRef: proto.ModelRef{Provider: "github-copilot", ID: "gpt-5"}, Name: "GPT-5 (Copilot)"}
	s.EndSignIn("github-copilot", proto.FlowApproved, "octocat", "", gpt)
	app.WaitFor("text:Signed in as @octocat. GitHub Copilot models are now available.", 5*time.Second)
	app.Press("text:Done")
	app.WaitFor("id:sign-in-github-copilot >> text:Signed in as @octocat", 5*time.Second)
	if n := len(s.RequestsTo(http.MethodPost, "/v1/signins/flows/flow-1/cancel")); n != 0 {
		t.Fatalf("Done canceled an approved flow: %d", n)
	}

	app.Press("label:Sign out of GitHub Copilot")
	app.WaitFor("text:Sign pocketd out of GitHub Copilot?", 5*time.Second)
	app.Press("role:alertdialog >> role:button Sign out of GitHub Copilot")
	app.WaitFor("id:sign-in-github-copilot >> text:Not signed in", 5*time.Second)
	if n := len(s.RequestsTo(http.MethodDelete, proto.Path(proto.PathSignIn, "github-copilot"))); n != 1 {
		t.Fatalf("%d sign-outs went", n)
	}

	app.Press("label:Sign in to GitHub Copilot")
	app.WaitFor("text:"+pocketdtest.UserCode, 5*time.Second)
	s.EndSignIn("github-copilot", proto.FlowDenied, "", "The code was denied at https://github.com/login/device.")
	app.WaitFor("text:The code was denied on GitHub.", 5*time.Second)
	app.Find("role:button Close")
	app.Press("role:button Try Again")
	app.WaitFor("text:Waiting for approval… ", 5*time.Second)
	app.Press("role:button Cancel")
	app.WaitForGone("text:Waiting for approval… ", 5*time.Second)
}

// TestSignInsThatFailedToLoadSaySo checks the failed list, with Try Again.
func TestSignInsThatFailedToLoadSaySo(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 900)
	s.FailOnce(http.MethodGet, proto.PathSignIns, http.StatusBadGateway)
	app.Press("label:Settings")
	app.Press("text:Accounts")
	app.WaitFor("text:Loading the sign-ins failed", 5*time.Second)
	app.Press("role:button Try Again")
	app.WaitFor("text:GitHub Copilot", 5*time.Second)
}
