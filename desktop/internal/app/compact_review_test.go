package app

import (
	"net/http"
	"os"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// phoneWindow is the test app's window as an iPhone's, not signed in to
// pocketd yet, with auth as its GitHub sign-in (nil for none).
func (x *testApp) phoneWindow(t *testing.T, auth *ghAuth) {
	t.Helper()
	mobile.SetLook(mobile.Cupertino)
	t.Cleanup(func() { mobile.SetLook(mobile.Automatic) })
	opts := x.options()
	opts.Platform = "ios"
	if auth != nil {
		api := &ghAPI{}
		opts.GitHubAuth, opts.GitHubClient = auth, func(string) state.GitHubAPI { return api }
	}
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(a.View, phoneW, phoneH, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
}

// A saved service that never answers keeps the first connection on the
// connecting screen: there a phone offers Sign Out, back to the address
// form, and Details opens the activity (Codex review of #85, round 1).
func TestAPhoneSignsOutOfAServiceThatNeverAnswers(t *testing.T) {
	x := newTestApp(t)
	x.phoneWindow(t, nil)
	x.srv.FailOnce(http.MethodGet, proto.PathEvents, http.StatusBadGateway)
	if err := x.a.st.Connect(x.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connecting && x.a.st.Conn.Reason != "" })
	x.click(t, "Details")
	if !x.a.shell.Console.Open || !x.tt.HasText("Close Activity") {
		t.Errorf("Details on the connecting screen shows no activity: %q", x.tt.Texts())
	}
	x.click(t, "Close Activity")
	x.click(t, "Sign Out")
	x.until(t, func() bool { return x.a.dialogs.Showing() })
	x.hasAll(t, "Sign out of pocketd?")
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return !x.a.st.SignedIn() })
	x.hasAll(t, "Connect to pocketd")
}

// A GitHub sign-in the keyring did not keep is said, with Save Again, on
// every view of a phone, as a desktop's status bar says it, whatever the
// Inbox holds (Codex review of #85, round 1).
func TestAPhoneSavesAnUnsavedGitHubSignInFromAnyView(t *testing.T) {
	x := newTestApp(t)
	auth := &ghAuth{signedIn: true, unsaved: true}
	x.phoneApp(t, false, auth)
	x.tap(t, "Sessions")
	x.hasAll(t, "GitHub sign-in not saved")
	x.tap(t, "Save Again")
	x.until(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return auth.saved == 1 })
	x.settle()
	if x.tt.HasText("GitHub sign-in not saved") {
		t.Error("the warning stays after the save")
	}
}

// The Activity sheet shows while the console would, so its own Close
// closes it (Codex review of #85, round 1).
func TestAPhonesActivityClosesWithItsOwnButton(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tap(t, "More")
	x.tap(t, "Activity")
	if !x.tt.HasText("Close Activity") {
		t.Fatalf("no activity in %q", x.tt.Texts())
	}
	x.tap(t, "Close Activity")
	if x.a.shell.Console.Open || x.tt.HasText("Close Activity") {
		t.Errorf("the activity stays open: %v, %q", x.a.shell.Console.Open, x.tt.Texts())
	}
}

// With no profile to choose, the folder's field is New Session's first
// control: on a phone it still takes no focus, which would bring the
// keyboard up (Codex review of #85, round 1).
func TestAPhonesNewSessionWithNoProfileFocusesNoField(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.srv.SetProfiles(nil)
	x.tap(t, "Sessions")
	x.tap(t, "New Session")
	x.until(t, func() bool {
		status, _ := x.a.st.Data.ProfilesStatus()
		return x.tt.HasText("Working directory") && status == state.ListLoaded && x.a.st.InFlight() == 0
	})
	if x.tt.HasText("Profile") {
		t.Fatalf("New Session offers profiles: %q", x.tt.Texts())
	}
	if x.tt.Focused("Working directory") {
		t.Error("New Session focused its folder's field")
	}
}

// A plan's card on a phone puts its lead's caption under its title, which
// takes the head row's room (Codex review of #85, round 1).
func TestAPhonesPlanCardPutsItsLeadUnderItsTitle(t *testing.T) {
	x := newTestApp(t)
	x.seedPlan()
	x.phoneWindow(t, nil)
	x.connect(t)
	title, ok := x.tt.Find("P1 · Sign the ISO waits for your approval")
	if !ok {
		t.Fatalf("no plan in %q", x.tt.Texts())
	}
	lead, ok := x.tt.Find("T7 · led by Duncan")
	if !ok || lead.Y < title.Y+title.H {
		t.Errorf("the lead's caption %+v is not under the title %+v", lead, title)
	}
}

// A session opened while an iPad's window is wide goes back to the tab of
// the place it was opened from once the window narrows (Codex review of
// #85, round 1).
func TestASessionOpenedWideGoesBackToItsTabNarrow(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tt.SetSize(820, 1180)
	x.settle()
	if x.a.compact {
		t.Fatal("the iPad's full window is compact")
	}
	x.a.goBoard()
	x.settle()
	x.a.openSession(11)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded })
	x.tt.SetSize(phoneW, phoneH)
	x.settle()
	x.hasAll(t, "Back to Board")
}

// The pane shell's palette, which a wide window brings to the command
// table, leaves it on a compact window, where it does not show, and comes
// back as the window widens (Codex review of #85, round 1).
func TestACompactWindowTakesThePalettesKeysAway(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tt.SetSize(820, 1180)
	x.settle()
	if _, ok := x.a.table.Command(frostui.PaletteCommandID); !ok {
		t.Fatal("the wide window has no palette command")
	}
	x.a.shell.Palette.Show()
	x.tt.SetSize(phoneW, phoneH)
	x.settle()
	if _, ok := x.a.table.Command(frostui.PaletteCommandID); ok || x.a.shell.Palette.Open {
		t.Errorf("the compact window keeps the palette: command %v, open %v", ok, x.a.shell.Palette.Open)
	}
	x.tt.SetSize(820, 1180)
	x.settle()
	if _, ok := x.a.table.Command(frostui.PaletteCommandID); !ok {
		t.Error("the palette's command does not come back as the window widens")
	}
}
