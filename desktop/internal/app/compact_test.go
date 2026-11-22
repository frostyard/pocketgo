package app

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The iPhone's screen, portrait, in DIPs, and what its status bar and
// home indicator cover of it (an iPhone 12 to 14's safe area).
const (
	phoneW, phoneH        = 390, 844
	phoneTop, phoneBottom = 47, 34
)

// phoneProcessEnv names, in the test process a phone test starts, the
// test that process runs as an iPhone's (inPhoneProcess).
const phoneProcessEnv = "POCKETGO_PHONE_PROCESS"

// inPhoneProcess runs the calling test again in a test process of its
// own and reports whether this is that process. The phone's safe areas
// (Tester.SetInsets) turn MyGo's touch handling on for the whole process,
// for good: the other tests' views must not draw so. The calling test
// does its work only where it reports true.
func inPhoneProcess(t *testing.T) bool {
	t.Helper()
	if os.Getenv(phoneProcessEnv) == t.Name() {
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), phoneProcessEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("%s as an iPhone: %v\n%s", t.Name(), err, out)
	}
	return false
}

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

// phoneApp is the test app's window as an iPhone's, with seedPhone's
// data, signed in to pocketd and, as the owner, to GitHub, a session
// pinned. Its phone controls take iOS's look, as on an iPhone. With
// safeAreas, its status bar and home indicator cover the screen's edges,
// as on an iPhone, which only a phone process may draw (inPhoneProcess).
// auth is the GitHub sign-in, nil for one that signs in at once.
func (x *testApp) phoneApp(t *testing.T, safeAreas bool, auth *ghAuth) {
	t.Helper()
	if safeAreas && os.Getenv(phoneProcessEnv) != t.Name() {
		t.Fatal("the phone's safe areas outside a phone process would turn touch on for the other tests")
	}
	mobile.SetLook(mobile.Cupertino)
	t.Cleanup(func() { mobile.SetLook(mobile.Automatic) })
	x.seedPhone()
	if auth == nil {
		auth = &ghAuth{wait: make(chan struct{})}
		close(auth.wait)
	}
	api := &ghAPI{preview: github.Preview{Head: mergeHead, BaseRef: "main", BaseOid: mergeBase, Method: "SQUASH", Eligible: true}}
	opts := x.options()
	opts.Platform = "ios"
	opts.GitHubAuth, opts.GitHubClient = auth, func(string) state.GitHubAPI { return api }
	// The program always gives the native chooser (Codex review of #85,
	// round 2).
	opts.PickFiles = func(context.Context) ([]string, error) { return nil, nil }
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(a.View, phoneW, phoneH, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
	if safeAreas {
		x.tt.SetInsets(phoneTop, 0, phoneBottom, 0)
	}
	a.Start()
	x.settle()
	x.connect(t)
	x.until(t, a.gh.Opened)
	x.untilProfiles(t)
	if p, _, _ := a.gh.SignIn(); p != state.SignedIn {
		a.openGitHubSignIn()
		x.until(t, func() bool { p, _, _ := a.gh.SignIn(); return p == state.SignedIn })
	}
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

// hasTabs reports whether the bar of tabs shows.
func (x *testApp) hasTabs() bool {
	_, ok := x.tt.AccessByRole(ui.RoleTab, "Inbox")
	return ok
}

// The iPhone's window, as the owner goes through it (ADR-0044): the Inbox
// under the bar of tabs, the Sessions tab, a session with its back button
// and no tabs, the work board, an Approve and Merge confirmation within
// the screen, and More. The bars go under the status bar and the home
// indicator; nothing else does.
func TestThePhoneSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.phoneApp(t, true, nil)
	x.hasAll(t, "Inbox", "Pin the runner to ubuntu-24.04?", "Check to Merge")
	for _, tab := range state.Tabs {
		if _, ok := x.tt.AccessByRole(ui.RoleTab, tab.Title()); !ok {
			t.Errorf("no tab %q", tab.Title())
		}
	}
	if n, ok := x.tt.AccessByRole(ui.RoleTab, "Inbox"); !ok || n.Bounds.Y+n.Bounds.H > phoneH-phoneBottom+0.5 {
		t.Errorf("the Inbox's tab is under the home indicator: %+v", n.Bounds)
	}
	if r, ok := x.tt.Find("1 question, 1 pull request and 1 tool call wait for you"); !ok || r.Y < phoneTop {
		t.Errorf("the Inbox's note is under the status bar: %+v, %v", r, ok)
	}
	// The top bar's color goes up under the status bar.
	img := x.tt.Image()
	scale := float32(img.Bounds().Dx()) / phoneW
	under, beside := img.At(int(phoneW/2*scale), int(phoneTop/2*scale)), img.At(int(phoneW/2*scale), int((phoneTop+4)*scale))
	if under != beside {
		t.Errorf("under the status bar is %v, the top bar %v", under, beside)
	}
	x.within(t, ui.RoleGroup, "Question A1")
	x.snapshot(t, "phone-1-inbox")

	x.tap(t, "Sessions")
	x.hasAll(t, "Pinned", "Team", "Recent", "Snow image fails to boot")
	x.snapshot(t, "phone-2-sessions")

	x.tap(t, "T1 · Fix the flaky arm64 job")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	x.hasAll(t, "Back to Sessions", "I'll look at the last failing runs first.")
	if x.hasTabs() {
		t.Error("the tabs show over a session")
	}
	if n, ok := x.tt.AccessByRole(ui.RoleTextField, "Message"); !ok || n.Bounds.Y+n.Bounds.H > phoneH-phoneBottom {
		t.Errorf("the composer is under the home indicator: %+v, %v", n.Bounds, ok)
	}
	x.snapshot(t, "phone-3-session")

	x.tap(t, "Back to Sessions")
	x.tap(t, "Board")
	x.hasAll(t, "Work Board", "Waits for you", "T1 · Fix the flaky arm64 job")
	x.snapshot(t, "phone-4-board")

	x.tap(t, "Inbox")
	x.checkToConfirm(t)
	x.snapshot(t, "phone-5-merge")
	x.tap(t, "Cancel")

	x.tap(t, "More")
	x.hasAll(t, "Appearance", "Zoom", "Activity", "Sign Out of GitHub", "About PocketGo", "Sign Out")
	x.snapshot(t, "phone-6-more")
}

// A session opened from a tab goes back to that tab's view
// (state.BackTo): from the Inbox's card to the Inbox, from the board's
// card to the board; going to a place by its key chooses its tab.
func TestAPhonesBackGoesToTheTabItCameFrom(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	if x.a.phone.tab != state.TabInbox || !x.hasTabs() {
		t.Fatalf("the window starts on tab %v, tabs %v", x.a.phone.tab, x.hasTabs())
	}
	x.click(t, "Open Session")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	x.hasAll(t, "Back to Inbox")
	if x.hasTabs() {
		t.Error("the tabs show over a session")
	}
	x.click(t, "Back to Inbox")
	if x.a.place != placeInbox || x.a.phone.tab != state.TabInbox || !x.hasTabs() {
		t.Fatalf("back from the session shows place %v, tab %v", x.a.place, x.a.phone.tab)
	}

	// The work board's key chooses its tab.
	x.a.table.Run("board")
	x.settle()
	if x.a.phone.tab != state.TabBoard {
		t.Fatalf("the board shows under tab %v", x.a.phone.tab)
	}
	x.click(t, "Open Vera's Session")
	x.until(t, func() bool { return x.a.st.Data.OpenID() != 0 })
	x.hasAll(t, "Back to Board")
	x.click(t, "Back to Board")
	if x.a.place != placeBoard {
		t.Fatalf("back from the board's session shows place %v", x.a.place)
	}
}

// On a phone a session opens with its keyboard down, and its composer
// says no keys, which a phone's keyboard does not show; New Session opens
// with no field focused, and Settings are More's rows, not the desktop's
// window.
func TestAPhoneBringsNoKeyboardUpUnasked(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.click(t, "Sessions")
	x.click(t, "T1 · Fix the flaky arm64 job")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	field, ok := x.tt.AccessByRole(ui.RoleTextField, "Message")
	if !ok || field.Placeholder != "Message the agent" {
		t.Errorf("the composer says %q (found %v)", field.Placeholder, ok)
	}
	if x.tt.Focused("Message") {
		t.Error("the composer took the focus as the session opened")
	}
	x.click(t, "Back to Sessions")
	x.click(t, "New Session")
	x.until(t, func() bool { return x.tt.HasText("Working directory") })
	if x.tt.Focused("Working directory") {
		t.Error("New Session focused its folder's field")
	}
	x.click(t, "Cancel")
	x.settle()

	x.a.table.Run("settings")
	x.settle()
	if x.a.place != placeMore || x.a.prefs.layout.Open {
		t.Fatalf("Settings on a phone shows place %v, the window open %v", x.a.place, x.a.prefs.layout.Open)
	}
	if _, ok := x.tt.AccessByRole(ui.RolePopUpButton, "Zoom"); !ok {
		t.Errorf("no Zoom in More: %q", x.tt.Texts())
	}
}

// A card's title wraps on a phone rather than being cut short beside its
// Open Session button, and its thread's caption goes under it.
func TestAPhonesCardTitlesAreWhole(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	line, ok := x.tt.Find("Pin the runner to ubuntu-24.04?")
	if !ok {
		t.Fatalf("no question in %q", x.tt.Texts())
	}
	title, ok := x.tt.Find("frostyard/nsl#31 waits for your review")
	if !ok {
		t.Fatalf("no pull request's title in %q", x.tt.Texts())
	}
	if title.H < 1.5*line.H {
		t.Errorf("the pull request's title is one line, %+v, beside a line of %+v: it is cut short", title, line)
	}
	caption, ok := x.tt.Find("T2 · Pin the runner image")
	if !ok || caption.Y < title.Y+title.H {
		t.Errorf("the thread's caption %+v is not under the title %+v", caption, title)
	}
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

// Touch stays out of the other tests' views: what turns it on for the
// whole process (Tester.SetInsets, and the fingers and touches) is called
// only in phoneApp, which runs it only in a phone process.
func TestTouchStaysInThePhoneProcess(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	touch := map[string]bool{"SetInsets": true, "FingerDown": true, "FingerMove": true, "FingerUp": true, "FingerCancel": true,
		"TouchDown": true, "TouchMove": true, "TouchUp": true, "TouchCancel": true}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && touch[sel.Sel.Name] && fn.Name.Name != "phoneApp" && fn.Name.Name != "keyboardUp" {
					t.Errorf("%s: %s calls %s, which turns touch on for every test of the process; use phoneApp in a phone process",
						fset.Position(sel.Pos()), fn.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}
