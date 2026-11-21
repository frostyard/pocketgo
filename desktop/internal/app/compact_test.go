package app

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The iPhone's screen, portrait, in DIPs.
const phoneW, phoneH = 390, 844

// seedPhone gives the fake pocketd the team's threads (seedTeam), with a
// transcript in T1's session, and the owner's own sessions: one running,
// one idle and one waiting for an approval.
func (x *testApp) seedPhone() {
	x.seedTeam()
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 11, Title: "T1 · Fix the flaky arm64 job", Cwd: "/home/team/projects/nsl", Model: model, Thinking: "high",
		Profile: "vera", Route: "profile", UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 11, Kind: proto.EntryUser, Text: "The **arm64** job fails about once a day. Find out why and fix it.", At: at(-10 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 11, Kind: proto.EntryAssistant, Thinking: "Start with the job's recent logs.",
			Text:      "I'll look at the last failing runs first.",
			ToolCalls: []proto.ToolCall{{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"gh run list --workflow ci.yml --status failure --limit 5"}`)}},
			At:        at(-9 * time.Minute)},
		proto.Entry{ID: 3, SessionID: 11, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c1", Name: "bash",
			Text: "completed  failure  ci  main  arm64 (timeout)  12m\ncompleted  failure  ci  main  arm64 (timeout)  11m"}, At: at(-9 * time.Minute)},
		proto.Entry{ID: 4, SessionID: 11, Kind: proto.EntryAssistant,
			Text: "Both failures are timeouts on the arm64 runner, whose image is two years old. Pinning the runner to ubuntu-24.04 should fix it; " +
				"I asked whether to pin it.", At: at(-8 * time.Minute)},
	)
	x.srv.AddSession(proto.Session{ID: 2, Title: "Desk dashboard: weekly metrics", Cwd: "/home/team/projects/pi-pocket-team", Model: model,
		State: proto.SessionRunning, UpdatedAt: at(-2 * time.Minute)})
	x.srv.AddSession(proto.Session{ID: 3, Title: "Homewiki: garden plan", Cwd: "/home/team/projects/homewiki", Model: model,
		State: proto.SessionIdle, UpdatedAt: at(-time.Hour)})
	x.srv.AddSession(proto.Session{ID: 4, Title: "Snow image fails to boot", Cwd: "/home/team/projects/snow", Model: model,
		State: proto.SessionWaiting, UpdatedAt: at(-3 * time.Minute)})
	x.srv.AddApproval(proto.Approval{ID: "a2", SessionID: 4, CallID: "b9", Tool: "bash",
		Arguments: json.RawMessage(`{"command":"sudo bootc switch ghcr.io/frostyard/snow:testing"}`), State: proto.ApprovalPending, RequestedAt: at(-30 * time.Second)})
}

// phoneApp is the test app's window as an iPhone's, going between its
// places by nav, with seedPhone's data, signed in to pocketd and, as the
// owner, to GitHub. Its phone controls take iOS's look, as on an iPhone.
func (x *testApp) phoneApp(t *testing.T, nav string) {
	t.Helper()
	mobile.SetLook(mobile.Cupertino)
	t.Cleanup(func() { mobile.SetLook(mobile.Automatic) })
	x.seedPhone()
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	api := &ghAPI{preview: github.Preview{Head: mergeHead, BaseRef: "main", BaseOid: mergeBase, Method: "SQUASH", Eligible: true}}
	opts := x.options()
	opts.Platform, opts.CompactNav = "ios", nav
	opts.GitHubAuth, opts.GitHubClient = auth, func(string) state.GitHubAPI { return api }
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(a.View, phoneW, phoneH, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
	a.Start()
	x.settle()
	x.connect(t)
	x.until(t, a.gh.Opened)
	x.untilProfiles(t)
	a.openGitHubSignIn()
	x.until(t, func() bool { p, _, _ := a.gh.SignIn(); return p == state.SignedIn })
	// The sign-in's toast goes after a few seconds, and the owner pinned
	// a session.
	x.tt.Advance(10 * time.Second)
	a.togglePin(2)
	x.settle()
	if !a.compact {
		t.Fatal("the iPhone's window is not compact")
	}
}

// within fails unless the element of role labelled label lies within the
// phone's screen, across.
func (x *testApp) within(t *testing.T, role ui.Role, label string) {
	t.Helper()
	n, ok := x.tt.AccessByRole(role, label)
	if !ok {
		t.Errorf("no %v %q in %q", role, label, x.tt.Texts())
		return
	}
	if n.Bounds.X < 0 || n.Bounds.X+n.Bounds.W > phoneW+0.5 {
		t.Errorf("%q runs past the screen: %+v", label, n.Bounds)
	}
}

// tap clicks label, then takes the pointer off the content, as a finger
// lifts: a phone shows no hover.
func (x *testApp) tap(t *testing.T, label string) {
	t.Helper()
	x.click(t, label)
	x.tt.Move(1, phoneH/2)
	x.settle()
}

// checkToConfirm checks the gates of frostyard/nsl#31 from its Inbox card
// and opens its Approve and Merge confirmation, which fits the screen.
func (x *testApp) checkToConfirm(t *testing.T) {
	t.Helper()
	x.tap(t, "Check to Merge")
	ref := github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}
	x.until(t, func() bool { return x.a.gh.Merge(ref).Phase == state.MergeChecked })
	x.tap(t, "Approve + Merge")
	x.until(t, func() bool { return x.a.dialogs.Showing() })
	x.hasAll(t, "Approve and merge frostyard/nsl#31?")
	x.within(t, ui.RoleAlertDialog, "Approve and merge frostyard/nsl#31?")
	x.within(t, ui.RoleButton, "Cancel")
}

// NavTabs: the iPhone's places are tabs at the bottom; a session opened
// from the Sessions tab shows in its place, with a back button to it.
func TestThePhoneWithTabsSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, NavTabs)

	x.hasAll(t, "Inbox", "Pin the runner to ubuntu-24.04?", "Check to Merge")
	for _, tab := range []string{"Inbox", "Board", "Sessions", "More"} {
		if _, ok := x.tt.AccessByRole(ui.RoleTab, tab); !ok {
			t.Errorf("no tab %q", tab)
		}
	}
	x.within(t, ui.RoleGroup, "Question A1")
	x.snapshot(t, "a-tabs-1-home")

	x.tap(t, "Sessions")
	x.hasAll(t, "Sessions", "Team", "Recent", "Snow image fails to boot")
	if x.a.place != placeSessions {
		t.Fatalf("the Sessions tab shows place %v", x.a.place)
	}
	x.snapshot(t, "a-tabs-2-navigation")

	x.tap(t, "T1 · Fix the flaky arm64 job")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	x.hasAll(t, "Back to Sessions", "I'll look at the last failing runs first.")
	if _, ok := x.tt.AccessByRole(ui.RoleTab, "Inbox"); ok {
		t.Error("the tabs show over a session")
	}
	x.snapshot(t, "a-tabs-3-session")

	x.tap(t, "Back to Sessions")
	if x.a.place != placeSessions {
		t.Fatalf("back from the session shows place %v", x.a.place)
	}
	x.tap(t, "Board")
	x.hasAll(t, "Work Board", "Waits for you", "T1 · Fix the flaky arm64 job")
	x.snapshot(t, "a-tabs-4-board")

	x.tap(t, "Inbox")
	x.checkToConfirm(t)
	x.snapshot(t, "a-tabs-5-merge")
}

// NavSheet: one view at a time; the top bar's button opens the sidebar
// as a sheet, and choosing a place or a session there closes it.
func TestThePhoneWithASheetSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, NavSheet)

	x.hasAll(t, "Inbox", "Pin the runner to ubuntu-24.04?", "Check to Merge")
	if _, ok := x.tt.AccessByRole(ui.RoleTab, "Inbox"); ok {
		t.Error("the sheet's layout shows tabs")
	}
	x.within(t, ui.RoleGroup, "Question A1")
	x.snapshot(t, "b-sheet-1-home")

	x.tap(t, "Places and Sessions")
	x.hasAll(t, "Work Board", "Team", "Recent", "Sign Out")
	x.snapshot(t, "b-sheet-2-navigation")

	x.tap(t, "T1 · Fix the flaky arm64 job")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	if x.a.phone.sheet {
		t.Error("the sheet stays open over the session chosen")
	}
	x.hasAll(t, "I'll look at the last failing runs first.")
	x.snapshot(t, "b-sheet-3-session")

	x.tap(t, "Places and Sessions")
	x.tap(t, "Work Board")
	x.hasAll(t, "Waits for you", "T1 · Fix the flaky arm64 job")
	x.snapshot(t, "b-sheet-4-board")

	x.tap(t, "Places and Sessions")
	x.tap(t, "Inbox")
	x.checkToConfirm(t)
	x.snapshot(t, "b-sheet-5-merge")
}

// The iPad's full window keeps the desktop's layout, and so does a
// desktop's, however narrow (state.Compact).
func TestOnlyANarrowIOSWindowIsCompact(t *testing.T) {
	for _, tc := range []struct {
		goos string
		w, h int
		want bool
	}{
		{"ios", phoneW, phoneH, true},
		{"ios", 820, 1180, false},
		{"linux", phoneW, phoneH, false},
	} {
		x := newTestApp(t)
		opts := x.options()
		opts.Platform = tc.goos
		a, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		x.a = a
		x.tt = ui.NewTesterWith(a.View, tc.w, tc.h, ui.TesterOptions{})
		x.connect(t)
		if a.compact != tc.want {
			t.Errorf("%s at %d×%d: compact %v, want %v", tc.goos, tc.w, tc.h, a.compact, tc.want)
		}
	}
}
