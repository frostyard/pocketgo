package nsgoclient

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
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

// A pull request's card says what the newest cross-review at its head
// found, with CI's state there, and shows the label GitHub has, with what
// pocketd last read behind it; it opens on GitHub at the link the client
// builds, never the URL pocketd sent. TestAPullRequestInTheInboxSnapshot
// holds this for the MyGo client.
func TestAPullRequestSaysItsCrossReview(t *testing.T) {
	s := pocketdtest.New()
	seedTeam(s)
	// A desktop's pointer rests on the pill for its tooltip; headless, the
	// idiom is a phone's unless the test says.
	app := signedInApp(t, s, 1230, 900, nsgotest.Idiom(core.IdiomDesktop))
	inbox := "id:" + state.PullCard(1)
	app.WaitFor(inbox+` >> text:"Cross-review R4 by teg (gpt-6-astra): ready at cccccccccccc · CI green"`, 5*time.Second)
	app.Find(inbox + ` >> text:"ready-for-human"`)
	app.Find(inbox + ` >> text:"vera/pin-the-runner-image-t2 → main"`)
	// A draft waits for nobody's review.
	if app.Exists("text:frostyard/frostread#12") {
		t.Error("a draft shows in the Inbox")
	}
	app.Screenshot("inbox-pull-request-review")
	tip := "text:It has ready-for-human on GitHub. The newest cross-review at its head said ready, and CI was green when pocketd last read it."
	if app.Exists(tip) {
		t.Fatal("the pill's tooltip shows before the pointer rests on it")
	}
	app.Hover(inbox + ` >> text:"ready-for-human"`)
	app.Advance(time.Second)
	app.WaitFor(tip, 3*time.Second)
	app.HoverOut()
	app.Press(inbox + " >> text:Open pull request")
	if got := app.OpenedURLs; !slices.Equal(got, []string{"https://github.com/frostyard/nsl/pull/31"}) {
		t.Errorf("opened %q", got)
	}
	// The same card stands in its thread's details on the board.
	app.Press("sidebar-panel >> text:Work board")
	app.Press("label:Details for T2")
	app.WaitFor(card(2)+` >> text:"Cross-review R4 by teg (gpt-6-astra): ready at cccccccccccc · CI green"`, 5*time.Second)
	app.Find(card(2) + ` >> text:"ready-for-human"`)
	// In a lane's narrow card, nothing of it stands outside the card.
	lane := app.Find(card(2)).Bounds()
	for _, part := range []string{`text:"ready-for-human"`, `text:"vera/pin-the-runner-image-t2 → main"`, "text:Open pull request"} {
		b := app.Find(card(2) + " >> " + part).Bounds()
		if b.Origin.X < lane.Origin.X || b.Origin.X+b.Size.Width > lane.Origin.X+lane.Size.Width {
			t.Errorf("%s stands outside its thread's card: %v in %v", part, b, lane)
		}
	}
	app.Screenshot("work-board-pull-request")
}

// A review that is not ready names its first finding, and its reviewer by
// the agent's name.
func TestAPullRequestSaysWhatANotReadyReviewFound(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "teg", Name: "Teg", Role: "specialist"})
	head := strings.Repeat("d", 40)
	s.AddPull(proto.PullRequest{ID: 1, Repository: "frostyard/nsl", Number: 31, Title: "Pin the runner image", State: proto.PullOpen,
		Head: "vera/pin", Base: "main", HeadSHA: head, Checks: proto.ChecksFailing,
		Review: &proto.PullReview{ID: 5, Agent: "teg", Model: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}, Head: head,
			Verdict: proto.ReviewNotReady, Findings: []string{"The timeout is still 10 minutes.", "The image is not pinned by digest."}}})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor(`text:"Cross-review R5 by Teg (gpt-6-astra): not-ready at dddddddddddd · CI failing · The timeout is still 10 minutes."`, 5*time.Second)
	if app.Exists("text:ready-for-human") {
		t.Error("a pull request without the label shows it")
	}
}

// A pull request the client can build no GitHub link for says so, and
// offers none.
func TestAPullRequestWithNoGitHubLinkSaysSo(t *testing.T) {
	s := pocketdtest.New()
	s.AddPull(proto.PullRequest{ID: 1, Repository: "not a repository", Number: 31, Title: "Pin the runner image",
		URL: "https://evil.example/pull/31", State: proto.PullOpen, Head: "vera/pin", Base: "main"})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor(`text:"no GitHub link"`, 5*time.Second)
	if app.Exists("text:Open pull request") {
		t.Error("a pull request with no GitHub link offers one")
	}
}

// The pill says what GitHub shows, and claims a ready review and green CI
// only when pocketd said both at the head, as
// TestTheReadyPillClaimsOnlyWhatPocketdSaw holds for the MyGo client.
func TestTheReadyPillClaimsOnlyWhatPocketdSaw(t *testing.T) {
	head := strings.Repeat("c", 40)
	ready := &proto.PullReview{Verdict: proto.ReviewReady, Head: head}
	for name, c := range map[string]struct {
		p      proto.PullRequest
		backed bool
	}{
		"a label by hand":      {proto.PullRequest{Ready: true, HeadSHA: head}, false},
		"a ready review, red":  {proto.PullRequest{Ready: true, HeadSHA: head, Review: ready, Checks: proto.ChecksFailing}, false},
		"a review of old head": {proto.PullRequest{Ready: true, HeadSHA: strings.Repeat("d", 40), Review: ready, Checks: proto.ChecksGreen}, false},
		"not ready, green": {proto.PullRequest{Ready: true, HeadSHA: head, Checks: proto.ChecksGreen,
			Review: &proto.PullReview{Verdict: proto.ReviewNotReady, Head: head}}, false},
		"ready and green": {proto.PullRequest{Ready: true, HeadSHA: head, Review: ready, Checks: proto.ChecksGreen}, true},
	} {
		got := readyTooltip(c.p)
		if !strings.HasPrefix(got, "It has ready-for-human on GitHub.") || strings.Contains(got, "said ready") != c.backed ||
			strings.Contains(got, "back it") {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got, want := readyTooltip(proto.PullRequest{Ready: true, HeadSHA: head}),
		"It has ready-for-human on GitHub. pocketd has not read a ready cross-review at its head with green CI."; got != want {
		t.Errorf("a label by hand: %q, want %q", got, want)
	}
}
