package app

import (
	"net/http"
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// Settings' Sign-ins page lists pocketd's sign-ins, Copilot first with
// Sign In…, the others with the command that signs them in on the service
// host. Sign In… opens the sheet: GitHub is asked for a code, then the
// code shows, large, with Copy, the device page the desktop builds, and
// the wait with the code's time left; once approved, the account and the
// models, which New Session offers then; denied, why, with Try Again.
// Cancel cancels the flow on pocketd, and Sign Out asks first (ADR-0062).
func TestTheSignInsPageAndItsSheetSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.connect(t)
	opened := make(chan string, 1)
	x.a.opts.OpenURL = func(u string) error { opened <- u; return nil }
	x.loop.Post(func() { x.a.table.Run("settings") })
	x.until(t, func() bool {
		status, _ := x.a.st.Data.SignInsStatus()
		return x.a.prefs.layout.Open && status == state.ListLoaded
	})
	x.click(t, "Sign-ins")
	x.hasAll(t, "GitHub Copilot", "Not signed in", "Sign In…", "Anthropic (Claude)", "Signed in. Sign in on the service host: pocketd login anthropic",
		"OpenAI Codex (ChatGPT)", "PocketGo never sees a token")
	x.snapshot(t, "settings-signins")

	start := proto.Path(proto.PathSignIn, "github-copilot")
	release := x.srv.HoldAnswerOnce(http.MethodPost, start)
	x.click(t, "Sign In…")
	x.until(t, func() bool { return x.srv.Holding(http.MethodPost, start) })
	x.hasAll(t, "Sign in to GitHub Copilot", "Asking GitHub for a code…", "Cancel")
	x.snapshot(t, "signin-sheet-starting")
	release()
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetCode })
	x.hasAll(t, pocketdtest.UserCode, "Copy", "Open github.com/login/device", "On another computer, go to https://github.com/login/device and enter the code.",
		"Waiting for approval… 15:00 left")
	x.snapshot(t, "signin-sheet-waiting")
	x.click(t, "Open github.com/login/device")
	if got := <-opened; got != "https://github.com/login/device" {
		t.Fatalf("the sheet opened %q", got)
	}

	gpt := proto.Model{ModelRef: proto.ModelRef{Provider: "github-copilot", ID: "gpt-5"}, Name: "GPT-5 (Copilot)"}
	x.srv.EndSignIn("github-copilot", proto.FlowApproved, "octocat", "", gpt)
	x.until(t, func() bool {
		models, _ := x.a.st.Models()
		return x.a.st.SignInSheet().Phase == state.SheetSignedIn && slices.ContainsFunc(models, func(m proto.Model) bool { return m.ModelRef == gpt.ModelRef })
	})
	x.hasAll(t, "Signed in as @octocat. GitHub Copilot models are now available.", "Done")
	x.snapshot(t, "signin-sheet-signed-in")
	x.click(t, "Done")
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetClosed })
	x.hasAll(t, "Signed in as @octocat", "Sign Out")
	x.snapshot(t, "settings-signins-signed-in")
	if n := len(x.srv.RequestsTo(http.MethodPost, "/v1/signins/flows/flow-1/cancel")); n != 0 {
		t.Fatalf("Done canceled an approved flow: %d", n)
	}

	x.click(t, "Sign Out")
	x.hasAll(t, "Sign pocketd out of GitHub Copilot?", "Sign Out of GitHub Copilot")
	x.snapshot(t, "signin-signout-confirm")
	x.click(t, "Sign Out of GitHub Copilot")
	x.until(t, func() bool {
		row, _ := x.a.st.Data.SignIn("github-copilot")
		return row.State == proto.SignInNone && !x.a.st.SigningOut("github-copilot").Busy
	})
	if n := len(x.srv.RequestsTo(http.MethodDelete, start)); n != 1 {
		t.Fatalf("%d sign-outs went", n)
	}
	x.hasAll(t, "Not signed in", "Sign In…")

	x.click(t, "Sign In…")
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetCode })
	x.srv.EndSignIn("github-copilot", proto.FlowDenied, "", "The code was denied at https://github.com/login/device.")
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetFailed })
	x.hasAll(t, "The code was denied on GitHub.", "Try Again", "Close")
	x.snapshot(t, "signin-sheet-failed")
	x.click(t, "Try Again")
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetCode })
	flow := x.a.st.SignInSheet().Flow.ID
	x.click(t, "Cancel")
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetClosed && x.a.st.InFlight() == 0 })
	if n := len(x.srv.RequestsTo(http.MethodPost, "/v1/signins/flows/"+flow+"/cancel")); n != 1 {
		t.Fatalf("Cancel sent %d cancels of %s", n, flow)
	}
}
