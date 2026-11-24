package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestSettingsDialog(t *testing.T) {
	service := pocketdtest.New()
	service.AddSession(proto.Session{ID: 7, Title: "Release checklist", State: proto.SessionIdle})
	app := signedInApp(t, service, 1230, 800)
	app.Press("text:Release checklist")
	app.Type("label:Message", "Check this after changing settings")
	composer := app.Find("label:Message").ID
	app.Press("label:Settings")
	app.WaitFor("id:settings-sidebar", 3*time.Second)
	app.Press("text:Dark")
	app.Screenshot("settings")
	app.Type("label:Search settings", "approvals")
	app.Find("label:Tool approvals")
	app.Press("label:Tool approvals")
	app.Clear("label:Search settings")
	app.Press("id:settings-nav-notifications")
	app.Find("text:Request permission")
	app.Press("id:settings-nav-connection")
	app.Find("text:Reconnect")
	app.Press("label:Close settings")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("label:Close settings") })
	if draft := app.Find("label:Message"); draft.ID != composer || draft.Text != "Check this after changing settings" {
		t.Fatal("settings replaced the conversation or its draft")
	}
	app.Key("mod+,")
	app.WaitFor("label:Close settings", 3*time.Second)
	app.Key("Escape")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("label:Close settings") })
}

func TestCompactSettingsDialog(t *testing.T) {
	app := signedInApp(t, pocketdtest.New(), 390, 730)
	app.Press("label:Navigation")
	app.Press("label:Settings")
	app.WaitFor("label:Search settings", 3*time.Second)
	if app.Exists("id:settings-sidebar") {
		t.Fatal("compact settings rendered the desktop sidebar")
	}
	app.Press("id:settings-nav-accounts")
	app.Find("text:Provider accounts")
	app.Press("label:Close settings")
	app.Press("text:Work board")
	app.WaitFor("text:No thread is on the desk", 3*time.Second)
}
