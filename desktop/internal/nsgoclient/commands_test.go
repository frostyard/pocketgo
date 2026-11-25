package nsgoclient

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// TestTheCommandsTheMyGoClientHad checks the commands and keys the NSGo
// client lacked: Search sessions on its key, the palette's second key,
// Stop run listed with why it is off, About with the protocol's version,
// and Sign out behind its confirmation.
func TestTheCommandsTheMyGoClientHad(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 800)
	app.Key("mod+shift+f")
	app.WaitFor("text:Search every session", 5*time.Second)
	app.Key("mod+shift+p")
	app.WaitFor("text:Stop run", 5*time.Second)
	app.Find("text:Sign out")
	app.Find("text:Zoom in")
	app.Press("text:About PocketGo")
	app.WaitFor("text:The client of pocketd, protocol version "+strconv.Itoa(proto.Version)+".", 5*time.Second)
	app.Press("role:button Close")
	app.Key("mod+k")
	app.WaitFor("text:Sign out", 5*time.Second)
	app.Press("text:Sign out")
	app.WaitFor("role:alertdialog", 5*time.Second)
	app.Press("role:button Sign out")
	app.WaitFor("label:Device token", 5*time.Second)
}

// TestStopRunDoesNothingWithNoRun checks that Stop run, listed while no
// run works, sends nothing.
func TestStopRunDoesNothingWithNoRun(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work"})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Editor")
	app.WaitFor("label:Message", 5*time.Second)
	app.Key("mod+.")
	app.Advance(time.Second)
	if n := len(s.RequestsTo("POST", "/v1/sessions/4/abort")); n != 0 {
		t.Errorf("Stop run with no run sent %d aborts", n)
	}
}

// TestZoomNeverLeavesTheWindowTooNarrow checks the zoom that fits: a zoom
// saved larger than the window has room for is shown as the largest that
// leaves the window its 2 panes, and Settings says so
// (TestZoomFitsAndItsKeysWorkEverywhereSnapshot holds this for MyGo).
func TestZoomNeverLeavesTheWindowTooNarrow(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 800, nsgotest.Storage(map[string]string{"zoom": "2"}))
	// At 200% the window would show 615 points, under the 720 its 2 panes
	// need; at 150% it shows 820.
	app.WaitUntil(5*time.Second, func() bool { return app.Find("id:zoom").Bounds().Size.Width == 820 })
	app.Press("label:Settings")
	app.WaitFor("text:Shown at 150%: the window is too narrow for more.", 5*time.Second)
}

// TestTheZoomKeysWorkOnTheSignInScreen checks that the zoom's keys work
// before a sign-in.
func TestTheZoomKeysWorkOnTheSignInScreen(t *testing.T) {
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node { return newClient(cx, pocketdtest.New().Transport()) }, nsgotest.Size(1230, 800))
	app.WaitFor("label:Device token", 5*time.Second)
	before := app.Find("id:zoom").Bounds().Size.Width
	app.Key("mod+=")
	app.WaitUntil(5*time.Second, func() bool { return app.Find("id:zoom").Bounds().Size.Width < before })
	if kept, _ := app.Storage().GetItem("zoom"); kept == "" || kept == "1" {
		t.Errorf("the zoom kept after Ctrl+= is %q", kept)
	}
}

// keptToken is the device token a keyring holds for a service.
type keptToken map[string]string

func (k keptToken) Token(server string) (string, error) { return k[server], nil }
func (k keptToken) SetToken(server, token string) error { k[server] = token; return nil }
func (k keptToken) Forget(server string) error          { delete(k, server); return nil }

// TestAClickAtStartOpensOnceConnected checks a notification clicked while
// the kept sign-in still starts: what it is about opens once the client
// is connected, in place of the session shown last (ADR-0049). The test of
// the same name holds this for the MyGo client.
func TestAClickAtStartOpensOnceConnected(t *testing.T) {
	// In a real window the test's press of a notification did not reach
	// the client (seen on the Linux target, 2026-11-25: the session shown
	// last opened), so this runs headless only.
	if nsgotest.Target() != "" {
		t.Skip("a notification's press is not delivered on a target")
	}
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 1, Title: "The notification's", Cwd: "/work"},
		proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "Said in the notification's session.", At: time.Now().Add(-time.Minute)})
	s.AddSession(proto.Session{ID: 3, Title: "Shown last", Cwd: "/work"},
		proto.Entry{ID: 1, SessionID: 3, Kind: proto.EntryUser, Text: "Said in the session shown last.", At: time.Now().Add(-time.Minute)})
	held := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(held) }) })
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		return startClient(cx, s.Transport(), false, func(c *client) { c.creds = slowToken{keptToken{pocketdtest.URL: pocketdtest.Token}, held} })
	}, nsgotest.Size(1230, 800), nsgotest.Storage(map[string]string{
		"connected-server":               pocketdtest.URL,
		"preferences/" + pocketdtest.URL: `{"LastSession":3}`,
	}))
	// The keyring has not answered yet: the client is not signed in.
	app.WaitFor("label:Device token", 5*time.Second)
	// The system still shows a notification of an earlier run.
	id := state.NoticeID(pocketdtest.URL, state.NoticeTarget{Session: 1})
	app.Notifications = append(app.Notifications, core.Notification{ID: id, Title: "The notification's"})
	app.PressNotification(id)
	once.Do(func() { close(held) })
	app.WaitFor("text:Said in the notification's session.", 5*time.Second)
	if app.Exists("text:Said in the session shown last.") {
		t.Error("the session shown last opened, not the notification's")
	}
}

// slowToken holds a keyring's answer until held closes.
type slowToken struct {
	keptToken
	held chan struct{}
}

func (s slowToken) Token(server string) (string, error) {
	<-s.held
	return s.keptToken.Token(server)
}
