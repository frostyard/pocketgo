package nsgoclient

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// A phone's and a tablet's environments, by the sizes the MyGo client's
// phone tests draw at: an iPhone at 390 by 844 and an iPad at 820 by 1180.
var (
	phone  = nsgotest.Environment(core.Environment{OS: core.OSIOS, Idiom: core.IdiomPhone, PixelRatio: 3})
	tablet = nsgotest.Environment(core.Environment{OS: core.OSIOS, Idiom: core.IdiomTablet, PixelRatio: 2})
)

// patience is how long a phone's test waits: a simulator on a shared
// runner took more than 5 seconds to sign in and draw its first window.
func patience() time.Duration {
	if nsgotest.Target() != "" {
		return 40 * time.Second
	}
	return 5 * time.Second
}

// headlessOnly skips a test of a phone or a tablet on a target that is
// neither: a real window is its platform's, whatever environment the test
// names (seen on the Linux target: the window kept the desktop's layout).
// On an iOS target the tests run, in the device's own window.
func headlessOnly(t *testing.T) {
	t.Helper()
	if target := nsgotest.Target(); target != "" && !strings.HasPrefix(target, "ios") {
		t.Skip("a phone's and a tablet's layout show headless and on an iOS target")
	}
}

func phoneApp(t *testing.T, s *pocketdtest.Server) *nsgotest.App {
	t.Helper()
	headlessOnly(t)
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node { return newClient(cx, s.Transport()) }, nsgotest.Size(390, 844), phone)
	app.Clear("label:Service URL")
	app.Type("label:Service URL", pocketdtest.URL)
	app.Type("label:Device token", pocketdtest.Token)
	app.Press("text:Connect")
	// The bar of tabs has 2 views of that id on a device, the shell's and
	// the system's bar in it, where WaitFor wants 1: its presence is waited
	// for.
	app.WaitUntil(patience(), func() bool { return app.Exists("id:tab-bar") })
	return app
}

// The bar of tabs is the system's on a phone: the test host holds it as
// a native view, whose items, badges and choice it reads and sets.
const tabBar = "id:tab-bar >> id:tab-bar"

// tabs are the bar's titles, in its order.
func tabs(app *nsgotest.App) []string {
	var titles []string
	items, _ := app.FindMounted(tabBar).NativeProps["items"].([]any)
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			title, _ := m["title"].(string)
			titles = append(titles, title)
		}
	}
	return titles
}

// The tabs' places in the bar.
const (
	tabInbox = iota
	tabBoard
	tabSessions
	tabMore
)

// TestAPhoneGoesByABarOfTabs checks the compact window (ADR-0044): the
// Inbox, the Board, the Sessions and More as tabs, with what waits on the
// Inbox's; a session over its tab, with a way back to it and no bar; and
// no navigation beside the content.
func TestAPhoneGoesByABarOfTabs(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work"},
		proto.Entry{ID: 1, SessionID: 4, Kind: proto.EntryUser, Text: "Said in the editor's session.", At: time.Now().Add(-time.Minute)})
	s.AddThread(proto.Thread{ID: 11, Session: 9, Title: "Publish artifacts", State: proto.ThreadBlocked})
	s.AddAsk(proto.Ask{ID: 5, Thread: 11, SessionID: 9, State: proto.AskOpen, Question: "Approve the artifact?"})
	app := phoneApp(t, s)
	if got := tabs(app); !slices.Equal(got, []string{"Inbox", "Board", "Sessions", "More"}) {
		t.Fatalf("the bar's tabs are %q", got)
	}
	// What waits shows on the Inbox's tab.
	app.WaitUntil(patience(), func() bool {
		badges := fmt.Sprintf("%+v", app.FindMounted(tabBar).NativeProps["badges"])
		return strings.Contains(badges, "Text:1")
	})
	if app.Exists("id:sidebar-panel") || app.Exists("label:Filter sessions") {
		t.Error("a phone's window shows the navigation beside its content")
	}
	app.WaitFor("text:Approve the artifact?", patience())

	app.SelectTab(tabBar, tabBoard)
	app.WaitFor("text:1 thread on the desk, 1 waits for you", patience())

	app.SelectTab(tabBar, tabSessions)
	app.WaitFor("label:Filter sessions", patience())
	app.Find("label:New session")
	app.Press("text:Editor")
	app.WaitFor("text:Said in the editor's session.", patience())
	// The session shows over the Sessions tab, with a way back to it. That
	// the bar is hidden over it (state.TabBarShows) does not show headless:
	// the host gives the system's bar no height.
	if got := app.FindMounted(tabBar).NativeProps["selectedIndex"]; fmt.Sprint(got) != fmt.Sprint(tabSessions) {
		t.Errorf("with a session open the bar's tab is %v, want Sessions (%d)", got, tabSessions)
	}
	app.Press("label:Back to Sessions")
	app.WaitFor("label:Filter sessions", patience())
	app.WaitForGone("text:Said in the editor's session.", patience())
}

// TestAPhonesMoreHoldsWhatHasNoTab checks the More tab: the places with
// no tab, the service, and the way out; and that a phone has no Terminal
// and no Quit (state.HasTerminal, state.QuitsItself).
func TestAPhonesMoreHoldsWhatHasNoTab(t *testing.T) {
	s := pocketdtest.New()
	app := phoneApp(t, s)
	app.SelectTab(tabBar, tabMore)
	for _, row := range []string{"Files", "Activity", "Settings", "About PocketGo", "Sign out"} {
		app.WaitFor("role:button "+row, patience())
	}
	app.Find("text:pocketd test · laptop")
	if app.Exists("text:Terminal") {
		t.Error("a phone's More offers the Terminal")
	}
	app.Press("role:button Activity")
	app.WaitFor("label:Copy the activity", patience())
	app.Press("label:Back to More")
	app.WaitFor("role:button Files", patience())
	if app.Exists("text:Quit") {
		t.Error("a phone's More offers Quit")
	}
}

// TestATabletsFullWindowKeepsTheDesktopsLayout checks an iPad's full
// window: the navigation beside the content, as a desktop's, with no
// Terminal place (TestTheIPadWindowHasNoTerminalSnapshot holds this for
// the MyGo client).
func TestATabletsFullWindowKeepsTheDesktopsLayout(t *testing.T) {
	headlessOnly(t)
	s := pocketdtest.New()
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node { return newClient(cx, s.Transport()) }, nsgotest.Size(820, 1180), tablet)
	app.Clear("label:Service URL")
	app.Type("label:Service URL", pocketdtest.URL)
	app.Type("label:Device token", pocketdtest.Token)
	app.Press("text:Connect")
	app.WaitFor("label:Filter sessions", patience())
	if app.Exists("id:tab-bar") {
		t.Error("an iPad's full window goes by a bar of tabs")
	}
	app.Find("text:Work board")
	if app.Exists("text:Terminal") {
		t.Error("an iPad's window has the Terminal place")
	}
	// Narrowed, as in Split View, it goes by the bar of tabs.
	app.Resize(500, 1180)
	app.WaitUntil(patience(), func() bool { return app.Exists("id:tab-bar") })
}

// TestADesktopsNarrowWindowHasNoBarOfTabs checks that the compact layout
// is a phone's and a tablet's: a desktop's narrow window keeps the kit's
// one-pane shell.
func TestADesktopsNarrowWindowHasNoBarOfTabs(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 620, 800)
	if app.Exists("id:tab-bar") {
		t.Error("a desktop's narrow window goes by a bar of tabs")
	}
}

// held counts a sign-in's suspends and resumes.
type held struct{ suspends, resumes int }

func (h *held) Suspend() { h.suspends++ }
func (h *held) Resume()  { h.resumes++ }

// TestAPhoneHoldsTheGitHubRefreshInTheBackground checks that the GitHub
// sign-in's refresh is held while iOS has the app in the background, and
// let go as it comes back (ADR-0041); a desktop holds nothing.
func TestAPhoneHoldsTheGitHubRefreshInTheBackground(t *testing.T) {
	// The test moves the app to the background through its environment,
	// which only the headless host takes from a test.
	if nsgotest.Target() != "" {
		t.Skip("the app's lifecycle is the platform's on a target")
	}
	for _, tc := range []struct {
		name string
		env  core.Environment
		want held
	}{
		{"a phone", core.Environment{OS: core.OSIOS, Idiom: core.IdiomPhone, PixelRatio: 3}, held{1, 1}},
		{"a desktop", core.Environment{OS: core.OSLinux, Idiom: core.IdiomDesktop, PixelRatio: 1}, held{}},
	} {
		got := &held{}
		app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
			return startClient(cx, pocketdtest.New().Transport(), false, func(c *client) { c.holdWhileSuspended(got) })
		}, nsgotest.Size(390, 844), nsgotest.Environment(tc.env))
		app.WaitFor("label:Device token", patience())
		away := tc.env
		away.Lifecycle = core.LifecycleBackground
		app.SetEnvironment(away)
		app.SetEnvironment(tc.env)
		if *got != tc.want {
			t.Errorf("%s: %d suspends and %d resumes, want %d and %d", tc.name, got.suspends, got.resumes, tc.want.suspends, tc.want.resumes)
		}
	}
}

// TestAPhonesBoardKeepsWhatHoldsSomethingAboveTheThreads checks the
// board's order on a compact window (state.BoardOrder): a night shift's
// section that holds something stays above the threads, and one that
// holds nothing goes after them. A desktop keeps its own order.
func TestAPhonesBoardKeepsWhatHoldsSomethingAboveTheThreads(t *testing.T) {
	seed := func() *pocketdtest.Server {
		s := pocketdtest.New()
		s.AddThread(proto.Thread{ID: 11, Session: 9, Agent: "editor", Title: "Publish artifacts", State: proto.ThreadOpen})
		s.SetSchedules(proto.Schedules{Schedules: []proto.Schedule{{ID: "morning", When: "07:00", Message: "Start the day."}}})
		return s
	}
	top := func(app *nsgotest.App, selector string) core.Points { return app.Find(selector).Bounds().Origin.Y }

	app := phoneApp(t, seed())
	app.SelectTab(tabBar, tabBoard)
	app.WaitFor(groupOf("specialists"), patience())
	schedules, threads, night := top(app, "id:board >> text:Schedules"), top(app, groupOf("specialists")), top(app, "id:board >> text:Night shift")
	if !(schedules < threads && threads < night) {
		t.Errorf("a phone's board has the schedules at %v, the threads at %v and the empty night shift at %v: want them in that order", schedules, threads, night)
	}

	desk := signedInApp(t, seed(), 1230, 900)
	desk.Press("sidebar-panel >> text:Work board")
	desk.WaitFor(groupOf("specialists"), patience())
	if threads, schedules := top(desk, groupOf("specialists")), top(desk, "id:board >> text:Schedules"); threads >= schedules {
		t.Errorf("a desktop's board has the threads at %v and the schedules at %v: want the threads first", threads, schedules)
	}
}
