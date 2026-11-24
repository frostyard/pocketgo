package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestFolderErrorToast(t *testing.T) {
	s := pocketdtest.New()
	s.SetFileRoots(proto.FileRoots{SessionFolders: true})
	s.AddSession(proto.Session{ID: 52349, Title: "Release worktree", Cwd: "/projects/release", State: proto.SessionIdle})
	app := signedInApp(t, s, 1230, 800)
	app.Press("label:Settings")
	app.Press("text:Dark")
	app.Press("label:Close settings")
	app.Press("text:Release worktree")
	app.Press("label:Session files")
	app.WaitFor("text:Couldn’t open folder", 5*time.Second)
	app.Press("text:Inbox")
	app.Find("text:not_found: session:52349: nothing is there")
	app.Screenshot("folder-error-toast")
	app.Resize(390, 730)
	close := app.Find("label:Dismiss message").Bounds()
	if close.Origin.X < 0 || close.Origin.X+close.Size.Width > 390 {
		t.Fatalf("dismiss button outside compact window: %v", close)
	}
	app.Press("label:Dismiss message")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Couldn’t open folder") })
}
