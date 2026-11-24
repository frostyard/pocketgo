package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestInboxPullCard(t *testing.T) {
	s := pocketdtest.New()
	p := proto.PullRequest{ID: 7, Repository: "example/mail", Number: 377, Title: "Keep the release guide portable across deployments", Base: "main", HeadSHA: "c1dab5e3a220128498a7608a8366fa3bb2f29ba4", State: proto.PullOpen, Checks: proto.ChecksPending}
	s.AddPull(p)
	app := signedInApp(t, s, 1230, 800)
	app.Press("label:Settings")
	app.Press("text:Dark")
	app.Press("label:Close settings")
	app.WaitFor("text:Checks pending", 3*time.Second)
	app.Find("text:example/mail#377")
	app.Find("text:" + p.Title)
	app.Find("text:Into main")
	app.Find("text:c1dab5e")
	app.Find("label:Copy full commit SHA")
	app.Find("text:Open pull request")
	app.Screenshot("inbox-pull-request")
	p.Checks = proto.ChecksFailing
	s.Emit(proto.Event{Type: proto.EventPull, Pull: &p})
	app.WaitFor("text:Checks failing", 3*time.Second)
	app.Resize(390, 730)
	app.WaitFor("text:"+p.Title, 3*time.Second)
	button := app.Find("text:Open pull request").Bounds()
	if button.Origin.X < 0 || button.Origin.X+button.Size.Width > 390 {
		t.Fatal("pull request action is outside the compact card")
	}
}
