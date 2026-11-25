//go:build !js

package nsgoclient

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// mygoSettings is what the MyGo client kept for the test's service.
func mygoSettings() config.Settings {
	return config.Settings{
		Server: pocketdtest.URL, LastServer: pocketdtest.URL, LastSession: 3, Zoom: 1.25,
		Pinned:   map[string][]int64{pocketdtest.URL: {3}},
		Notify:   map[string]config.Notify{pocketdtest.URL: {AsksOff: true}},
		Notified: map[string][]string{pocketdtest.URL: {"ask:5"}},
		Widths:   map[string]float32{"sidebar": 300},
	}
}

// TestADeviceOfTheMyGoClientStartsSignedIn checks the move of the MyGo
// client's settings: a first start of this client on them signs in to the
// service they name with the keyring's token and no form, opens the
// session shown last, and has the zoom, the pins, the notification
// switches and what has notified.
func TestADeviceOfTheMyGoClientStartsSignedIn(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 3, Title: "Shown last", Cwd: "/work"},
		proto.Entry{ID: 1, SessionID: 3, Kind: proto.EntryUser, Text: "Said in the session shown last.", At: time.Now().Add(-time.Minute)})
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		return startClient(cx, s.Transport(), false, func(c *client) {
			c.moveSettings(mygoSettings())
			c.creds = keptToken{pocketdtest.URL: pocketdtest.Token}
		})
	}, nsgotest.Size(1230, 800))
	app.WaitFor("text:Said in the session shown last.", 5*time.Second)
	if app.Exists("label:Device token") {
		t.Error("the sign-in form shows on a device the MyGo client was signed in on")
	}
	want := map[string]string{
		"connected-server": pocketdtest.URL, "last-server": pocketdtest.URL, "zoom": "1.25", movedKey: "1",
		"notified/" + pocketdtest.URL: `["ask:5"]`,
	}
	for key, value := range want {
		if got, _ := app.Storage().GetItem(key); got != value {
			t.Errorf("%s is %q, want %q", key, got, value)
		}
	}
	var prefs preferences
	text, _ := app.Storage().GetItem("preferences/" + pocketdtest.URL)
	if err := json.Unmarshal([]byte(text), &prefs); err != nil || len(prefs.Pinned) != 1 || prefs.Pinned[0] != 3 || !prefs.Notify.AsksOff || prefs.Notify.Off {
		t.Errorf("the moved preferences are %s (%v)", text, err)
	}
	if _, moved := app.Storage().GetItem("pane-widths"); moved {
		t.Error("the MyGo window's pane widths were moved")
	}
}

// TestTheMoveIsMadeOnceAndLeavesAClientOfItsOwn checks that the move
// leaves a storage that has a sign-in of its own, and is not made again.
func TestTheMoveIsMadeOnceAndLeavesAClientOfItsOwn(t *testing.T) {
	for name, storage := range map[string]map[string]string{
		"a sign-in of its own": {"last-server": "https://other.example"},
		"moved before":         {movedKey: "1"},
	} {
		app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
			return startClient(cx, pocketdtest.New().Transport(), false, func(c *client) { c.moveSettings(mygoSettings()) })
		}, nsgotest.Size(1230, 800), nsgotest.Storage(storage))
		app.WaitFor("label:Device token", 5*time.Second)
		if got, _ := app.Storage().GetItem("connected-server"); got != "" {
			t.Errorf("%s: the move set the service to sign in to: %q", name, got)
		}
		if got, _ := app.Storage().GetItem("zoom"); got != "" {
			t.Errorf("%s: the move set the zoom: %q", name, got)
		}
	}
}
