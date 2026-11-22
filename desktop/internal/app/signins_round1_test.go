package app

import (
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// A compact window, whose Settings are More's rows, signs pocketd in too:
// More lists the sign-ins, GitHub Copilot's row taps to the sheet, and
// Cancel cancels the flow on pocketd (Codex review of #123, round 1).
func TestAPhoneSignsPocketdInFromMoreSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tap(t, "More")
	x.until(t, func() bool { status, _ := x.a.st.Data.SignInsStatus(); return status == state.ListLoaded })
	x.hasAll(t, "Sign-ins", "GitHub Copilot", "Not signed in", "Sign In…", "Anthropic (Claude)")
	x.snapshot(t, "phone-more-signins")
	x.tap(t, "Sign In…")
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetCode })
	x.hasAll(t, pocketdtest.UserCode, "Open github.com/login/device")
	x.snapshot(t, "phone-signin-sheet")
	flow := x.a.st.SignInSheet().Flow.ID
	x.tap(t, "Cancel")
	x.until(t, func() bool { return x.a.st.SignInSheet().Phase == state.SheetClosed && x.a.st.InFlight() == 0 })
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathCancelSignIn, flow))); n != 1 {
		t.Fatalf("Cancel sent %d cancels", n)
	}
}
