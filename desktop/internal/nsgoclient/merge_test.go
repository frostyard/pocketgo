package nsgoclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// The owner's approve and merge in the NSGo client (ADR-0018, ADR-0031):
// the views and texts of the MyGo client's merge.go and mergeall.go, over
// the same rules in state. GitHub is the fake below, a port of the MyGo
// client's (desktop/internal/app/merge_test.go): no test reaches the real
// GitHub or a real keyring.

// ghAuth is the owner's App sign-in, in memory, as the MyGo client's
// tests fake it.
type ghAuth struct {
	mu       sync.Mutex
	openErr  error
	signedIn bool
	unsaved  bool
	saved    int
	reason   string
}

func (f *ghAuth) Open() error { return f.openErr }
func (f *ghAuth) Start(context.Context) (github.DeviceCode, error) {
	return github.DeviceCode{}, errors.New("no device flow in this test")
}
func (f *ghAuth) Poll(ctx context.Context, _ github.DeviceCode) (github.AppToken, error) {
	<-ctx.Done()
	return github.AppToken{}, ctx.Err()
}
func (f *ghAuth) BeginSignIn() int                 { return 1 }
func (f *ghAuth) CancelSignIn(int)                 {}
func (f *ghAuth) Adopt(github.AppToken, int) error { return errors.New("no sign-in in this test") }
func (f *ghAuth) Invalidate(int)                   {}
func (f *ghAuth) DeleteAgain() error               { return nil }
func (f *ghAuth) BeginSaveAgain() (func() error, error) {
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unsaved, f.saved = false, f.saved+1
		return nil
	}, nil
}
func (f *ghAuth) BeginForget() (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signedIn, f.reason = false, ""
	return func() error { return nil }, nil
}
func (f *ghAuth) Lease(context.Context) (string, int, func(), error) {
	return "gho_x", 1, func() {}, nil
}
func (f *ghAuth) Status() github.AppStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.AppStatus{SignedIn: f.signedIn, Login: github.Login, Unsaved: f.unsaved, Reason: f.reason}
}

// ghAPI is GitHub as the owner's client sees it, as the MyGo client's
// tests fake it, with what these tests need besides: an owner the App
// cannot act on and the App's notes, a read that fails, a pull request of
// its own, and a first read or a merge held back.
type ghAPI struct {
	mu      sync.Mutex
	preview github.Preview
	// readErr fails every read of the gates.
	readErr error
	// owners are why the App cannot act on an owner, notes its warnings.
	owners, notes map[string]string
	// each changes the read of a reference, after preview.
	each    map[string]func(*github.Preview)
	intents []github.Intent
	// reads counts the reads of the gates, by reference; results answer an
	// approve and merge of a reference.
	reads   map[string]int
	results map[string]github.Result
	// bases, when set, are GitHub's base branches by "owner/name:branch":
	// a read takes its base commit there, a merge moves it, and Act refuses
	// a risk:high intent whose base moved since its check (ADR-0038).
	// strict are the references whose rules need them up to date: once
	// their base moved, the gates read them behind it.
	bases  map[string]string
	moves  int
	strict map[string]bool
	// first holds a reference's first read, again each read after it, and
	// acts each approve and merge of it, until closed.
	first, again, acts map[string]chan struct{}
}

func ghBase(ref github.Ref, base string) string { return ref.Owner + "/" + ref.Name + ":" + base }

func (f *ghAPI) CheckApp(_ context.Context, asked []string) (github.AppCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	check := github.AppCheck{Login: github.Login, Owners: map[string]string{}, Notes: map[string]string{}}
	for _, o := range asked {
		check.Owners[o] = f.owners[o]
		if note := f.notes[o]; note != "" {
			check.Notes[o] = note
		}
	}
	return check, nil
}

func (f *ghAPI) Preview(_ context.Context, ref github.Ref, _ github.CrossReview) (github.Preview, error) {
	f.mu.Lock()
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[ref.String()]++
	w := f.first[ref.String()]
	if f.reads[ref.String()] > 1 {
		w = f.again[ref.String()]
	}
	f.mu.Unlock()
	if w != nil {
		<-w
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return github.Preview{Ref: ref}, f.readErr
	}
	p := f.preview
	p.Ref = ref
	if each := f.each[ref.String()]; each != nil {
		each(&p)
	}
	if base, ok := f.bases[ghBase(ref, p.BaseRef)]; ok {
		if base != p.BaseOid && f.strict[ref.String()] {
			p.Eligible, p.Reason = false, "the PR is behind its base"
		}
		p.BaseOid = base
	}
	return p, nil
}

func (f *ghAPI) Act(_ context.Context, in github.Intent, op string, _ func(string) error) github.Result {
	f.mu.Lock()
	w := f.acts[in.Preview.Ref.String()]
	f.mu.Unlock()
	if w != nil {
		<-w
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intents = append(f.intents, in)
	if r, ok := f.results[in.Preview.Ref.String()]; ok {
		r.Op = op
		return r
	}
	if f.bases != nil {
		key := ghBase(in.Preview.Ref, in.Preview.BaseRef)
		if now := f.bases[key]; in.Preview.RiskHigh && now != in.Preview.BaseOid {
			return github.Result{Op: op, Kind: "refused", Step: "read", Reason: "the base moved since it was checked (" + now[:12] + "): check it again"}
		}
		f.moves++
		f.bases[key] = fmt.Sprintf("b%039x", f.moves)
	}
	return github.Result{Op: op, Kind: "merged", Method: in.Preview.Method, MergedInto: in.Preview.BaseRef}
}

func (f *ghAPI) acted() []github.Intent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]github.Intent(nil), f.intents...)
}

// holdBack is a channel that holds a read or a merge back until release; the
// test's end releases it too, so nothing stays blocked.
func holdBack(t *testing.T) (ch chan struct{}, release func()) {
	ch = make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

const (
	// mergeHead is the head the fake gates read, and pocketd's ready
	// verdict and green CI are at.
	mergeHead = "cccccccccccccccccccccccccccccccccccccccc"
	mergeBase = "3333333333333333333333333333333333333333"
)

// readyPull is a pull request that waits for the owner's review, with
// pocketd's ready cross-review and green CI at mergeHead.
func readyPull(id int64, repo string, number int64, title, branch string) proto.PullRequest {
	return proto.PullRequest{ID: id, Repository: repo, Number: number, Title: title, State: proto.PullOpen, Head: branch, Base: "main",
		HeadSHA: mergeHead, Ready: true, Checks: proto.ChecksGreen,
		Review: &proto.PullReview{ID: 100 + id, Agent: "teg", Model: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"},
			Family: "openai", Head: mergeHead, Verdict: proto.ReviewReady, Summary: "Read the diff."},
		CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)}
}

// nsl31 and the porter bumps are the MyGo tests' pull requests: frostyard/
// nsl#31 by an agent, and Dependabot's on frostyard/porter.
var nsl31 = readyPull(1, "frostyard/nsl", 31, "Pin the runner image to ubuntu-24.04", "vera/pin-the-runner-image-t2")

func porterBump(n int64, title, branch string) proto.PullRequest {
	return readyPull(n, "frostyard/porter", n, title, branch)
}

var (
	porter53 = porterBump(53, "Bump golang.org/x/net from 0.46.0 to 0.47.0", "dependabot/go_modules/golang.org/x/net-0.47.0")
	porter54 = porterBump(54, "Bump golang.org/x/sys from 0.38.0 to 0.39.0", "dependabot/go_modules/golang.org/x/sys-0.39.0")
	porter55 = porterBump(55, "Bump actions/checkout from 6.0.0 to 6.0.1", "dependabot/github_actions/actions/checkout-6.0.1")
)

// newAPI is the fake GitHub, whose gates pass at mergeHead into main with
// a squash.
func newAPI() *ghAPI {
	return &ghAPI{preview: github.Preview{Head: mergeHead, BaseRef: "main", BaseOid: mergeBase, Method: "SQUASH", Eligible: true}}
}

// mergeApp is the client signed in to the fake pocketd with a kept token,
// with the owner's GitHub sign-in and client faked.
func mergeApp(t *testing.T, s *pocketdtest.Server, auth *ghAuth, api *ghAPI, w, h core.Points) *nsgotest.App {
	t.Helper()
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		cx.SetAppearance(core.AppearanceLight)
		return startClient(cx, s.Transport(), false, func(c *client) {
			c.creds = keptToken{pocketdtest.URL: pocketdtest.Token}
			c.initGitHubWith(auth, func(string) state.GitHubAPI { return api })
		})
	}, nsgotest.Size(w, h), nsgotest.Storage(map[string]string{"connected-server": pocketdtest.URL}))
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
	return app
}

// pullCard is the selector of pull request id's card in the Inbox.
func pullCard(id int64) string { return "id:" + state.PullCard(id) }

// checked presses the card's check and waits for what the gates read.
func checked(app *nsgotest.App, id int64, button, says string) {
	app.Press(pullCard(id) + " >> label:" + button)
	app.WaitFor(pullCard(id)+" >> text:"+says, 5*time.Second)
}

// The owner checks a pull request's gates from its card, confirms, and it
// is approved and merged as the owner at the head shown (ADR-0018); the
// confirmation names the pull request, its head commit, its base, the
// method and who approves. TestApproveAndMergeFromTheInboxSnapshot holds
// this for the MyGo client.
func TestApproveAndMergeFromAPullRequestCard(t *testing.T) {
	s := pocketdtest.New()
	s.AddPull(nsl31)
	api := newAPI()
	app := mergeApp(t, s, &ghAuth{signedIn: true}, api, 1230, 800)
	app.WaitFor(pullCard(1)+" >> text:Its merge gates are read on GitHub, as bketelsen.", 5*time.Second)
	checked(app, 1, "Check merge gates", "Gates pass at cccccccccccc into main · squash")
	app.Find(pullCard(1) + " >> label:Check again")
	if !app.Enabled(pullCard(1) + " >> label:Approve and merge") {
		t.Fatal("Approve and merge is off where the gates pass")
	}
	app.Screenshot("inbox-merge")
	app.Press(pullCard(1) + " >> label:Approve and merge")
	app.WaitFor(`role:alertdialog >> text:"Approve and merge frostyard/nsl#31?"`, 3*time.Second)
	app.Find("role:alertdialog >> text:PocketGo approves it at commit " + mergeHead + " as bketelsen, then merges it into main with squash, if the gates still pass at that head.")
	app.Screenshot("inbox-merge-confirmation")
	if len(api.acted()) != 0 {
		t.Fatal("it merged before the owner confirmed")
	}
	app.Press(`role:alertdialog >> role:button "Approve and merge"`)
	app.WaitFor(pullCard(1)+" >> text:Merged as bketelsen.", 5*time.Second)
	app.Find(pullCard(1) + ` >> text:"merged"`)
	app.Find("text:frostyard/nsl#31: Merged as bketelsen.")
	intents := api.acted()
	if len(intents) != 1 || intents[0].Preview.Head != mergeHead || intents[0].Mode != "single" {
		t.Fatalf("intents %+v", intents)
	}
	// pocketd read it on GitHub at the confirmation.
	if got := len(s.RequestsTo("GET", proto.Path(proto.PathPull, 1))); got != 1 {
		t.Errorf("pocketd was read %d times at the confirmation, want 1", got)
	}
}

// Where approve and merge cannot run, the card says why, where its button
// is, and the button stays off. The MyGo client's tests hold the same:
// TestApproveAndMergeOffSaysWhy (off), TestApproveAndMergeFromTheInboxSnapshot
// (the sign-in), TestTheSignInScreenSignsOutOfGitHubSnapshot (a kept
// sign-in not used), TestAnUnsavedSignInShowsInTheStatusBar (Save Again),
// TestARiskyPullRequestNeedsItsDiffOpened (risk:high) and
// TestTheConfirmationNamesTheBaseGitHubRead (a base GitHub reads
// otherwise); state's TestTheSignInNamesAnOwnerTheAppCannotActOn,
// TestAQueueCheckShowsTheAppsNote and TestAMergeNeedsGreenCIFromPocketd
// hold the rules under the rest.
func TestApproveAndMergeSaysWhyItIsOff(t *testing.T) {
	for _, c := range []struct {
		name    string
		auth    *ghAuth
		api     func(*ghAPI)
		says    []string
		checks  bool   // whether Check merge gates is offered
		check   string // what a check shows, when it runs
		approve string // "on", "off", or "" where Approve and merge is not offered
		save    bool   // whether the card offers Save again
		picture string
	}{
		{name: "the sign-in is held elsewhere", auth: &ghAuth{openErr: errors.New("another PocketGo holds the GitHub sign-in")},
			says: []string{"Approve and merge are off: another PocketGo holds the GitHub sign-in"}},
		{name: "signed out", auth: &ghAuth{reason: "the GitHub sign-in in the keyring is not one: a field is empty"},
			says: []string{"The kept GitHub sign-in is not used: the GitHub sign-in in the keyring is not one: a field is empty",
				"Sign in to GitHub in Settings, under Accounts, to approve and merge it as bketelsen."}, picture: "inbox-merge-sign-in"},
		{name: "the refreshed sign-in is not kept", auth: &ghAuth{signedIn: true, unsaved: true},
			says: []string{"The keyring did not keep the refreshed GitHub sign-in: nothing runs on GitHub until it does."}, checks: true,
			check: "Gates pass at", approve: "on", save: true},
		{name: "the App cannot act on the owner", auth: &ghAuth{signedIn: true}, api: func(a *ghAPI) { a.owners = map[string]string{"frostyard": "the App is suspended on frostyard"} },
			says: []string{"The App " + github.AppName + " cannot act on frostyard: the App is suspended on frostyard"}, checks: true,
			check: "the App " + github.AppName + " cannot act on frostyard: the App is suspended on frostyard", approve: "off"},
		{name: "the gates refuse", auth: &ghAuth{signedIn: true}, api: func(a *ghAPI) { a.preview.Eligible, a.preview.Reason = false, "a review requests changes" },
			checks: true, check: "a review requests changes"},
		{name: "the read fails", auth: &ghAuth{signedIn: true}, api: func(a *ghAPI) { a.readErr = errors.New("GitHub answered 502") },
			checks: true, check: "Reading its gates failed: GitHub answered 502"},
		{name: "GitHub reads another base", auth: &ghAuth{signedIn: true}, api: func(a *ghAPI) { a.preview.BaseRef = "release" },
			checks: true, check: "pocketd read it merging into main, the gates into release: wait for pocketd to read it again", approve: "off"},
		{name: "GitHub reads another head", auth: &ghAuth{signedIn: true}, api: func(a *ghAPI) { a.preview.Head = strings.Repeat("d", 40) },
			checks: true, check: "pocketd has no ready cross-review with green CI at dddddddddddd, the head the gates read", approve: "off"},
		{name: "risk:high", auth: &ghAuth{signedIn: true}, api: func(a *ghAPI) { a.preview.RiskHigh = true },
			checks: true, check: "it has risk:high: open its diff first", approve: "off", picture: "inbox-merge-risk-high"},
		{name: "the merge queue's note", auth: &ghAuth{signedIn: true}, api: func(a *ghAPI) {
			a.preview.Queue, a.preview.Method = true, ""
			a.notes = map[string]string{"frostyard": "an enqueue may fail: the App lacks merge_queues: write on frostyard"}
		}, checks: true, check: "an enqueue may fail: the App lacks merge_queues: write on frostyard", approve: "on"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := pocketdtest.New()
			s.AddPull(nsl31)
			api := newAPI()
			if c.api != nil {
				c.api(api)
			}
			app := mergeApp(t, s, c.auth, api, 1230, 800)
			card := pullCard(1)
			for _, says := range c.says {
				app.WaitFor(card+" >> text:"+says, 5*time.Second)
			}
			if c.checks {
				app.WaitFor(card+" >> label:Check merge gates", 5*time.Second)
			} else if app.Exists(card + " >> label:Check merge gates") {
				t.Fatal("Check merge gates is offered")
			}
			if c.check != "" {
				checked(app, 1, "Check merge gates", c.check)
			}
			switch approve := card + " >> label:Approve and merge"; c.approve {
			case "":
				if app.Exists(approve) {
					t.Error("Approve and merge is offered")
				}
			case "on":
				if !app.Enabled(approve) {
					t.Error("Approve and merge is off")
				}
			case "off":
				if app.Enabled(approve) {
					t.Error("Approve and merge is on")
				}
			}
			if c.picture != "" {
				app.Screenshot(c.picture)
			}
			if c.save {
				app.Press(card + " >> label:Save again")
				app.WaitUntil(5*time.Second, func() bool { c.auth.mu.Lock(); defer c.auth.mu.Unlock(); return c.auth.saved == 1 })
				app.WaitForGone(card+" >> text:The keyring did not keep the refreshed GitHub sign-in", 3*time.Second)
			}
			if len(api.acted()) != 0 {
				t.Fatalf("it merged: %+v", api.acted())
			}
		})
	}
}

// A risk:high pull request's Approve and merge turns on once its checked
// diff was opened, at the head the gates read. TestARiskyPullRequestNeedsItsDiffOpened
// holds this for the MyGo client.
func TestARiskyPullRequestMergesOnceItsDiffWasOpened(t *testing.T) {
	s := pocketdtest.New()
	s.AddPull(nsl31)
	api := newAPI()
	api.preview.RiskHigh = true
	app := mergeApp(t, s, &ghAuth{signedIn: true}, api, 1230, 800)
	app.WaitFor(pullCard(1)+" >> label:Check merge gates", 5*time.Second)
	checked(app, 1, "Check merge gates", "it has risk:high: open its diff first")
	app.Press(pullCard(1) + " >> label:Open checked diff")
	if got := app.OpenedURLs; len(got) != 1 || got[0] != "https://github.com/frostyard/nsl/compare/"+mergeBase+"..."+mergeHead {
		t.Fatalf("opened %q", got)
	}
	app.WaitUntil(5*time.Second, func() bool { return app.Enabled(pullCard(1) + " >> label:Approve and merge") })
	if app.Exists(pullCard(1) + " >> text:it has risk:high: open its diff first") {
		t.Error("the reason stays once the diff was opened")
	}
}

// The merge queue's confirmation says it adds the pull request to the
// queue, and names the App's note. TestTheConfirmationNamesTheBaseGitHubRead
// and state's TestAQueueCheckShowsTheAppsNote hold the same for the MyGo
// client.
func TestTheQueuesConfirmationNamesTheAppsNote(t *testing.T) {
	s := pocketdtest.New()
	s.AddPull(nsl31)
	api := newAPI()
	api.preview.Queue, api.preview.Method = true, ""
	api.notes = map[string]string{"frostyard": "an enqueue may fail: the App lacks merge_queues: write on frostyard"}
	app := mergeApp(t, s, &ghAuth{signedIn: true}, api, 1230, 800)
	app.WaitFor(pullCard(1)+" >> label:Check merge gates", 5*time.Second)
	checked(app, 1, "Check merge gates", "Gates pass at cccccccccccc into main · the merge queue")
	app.Press(pullCard(1) + " >> label:Approve and merge")
	app.WaitFor("role:alertdialog >> text:PocketGo approves it at commit "+mergeHead+" as bketelsen, then adds it to the merge queue of main, if the gates still pass at that head.", 3*time.Second)
	app.Find("role:alertdialog >> text:an enqueue may fail: the App lacks merge_queues: write on frostyard")
}

// Check again reads the gates once more, and the card shows what they read
// now. The MyGo client's card holds the same (TestTheConfirmationNamesTheBaseGitHubRead
// checks again after a retarget).
func TestCheckAgainReadsTheGatesAgain(t *testing.T) {
	s := pocketdtest.New()
	s.AddPull(nsl31)
	api := newAPI()
	app := mergeApp(t, s, &ghAuth{signedIn: true}, api, 1230, 800)
	app.WaitFor(pullCard(1)+" >> label:Check merge gates", 5*time.Second)
	checked(app, 1, "Check merge gates", "Gates pass at cccccccccccc into main · squash")
	api.mu.Lock()
	api.preview.Eligible, api.preview.Reason = false, "the PR has conflicts"
	api.mu.Unlock()
	checked(app, 1, "Check again", "the PR has conflicts")
	app.Find(pullCard(1) + ` >> text:"not mergeable"`)
	if app.Exists(pullCard(1) + " >> text:Gates pass at") {
		t.Error("the card still says the gates pass")
	}
	api.mu.Lock()
	reads := api.reads["frostyard/nsl#31"]
	api.mu.Unlock()
	if reads != 2 {
		t.Errorf("the gates were read %d times, want 2", reads)
	}
}

// Each attempt's end shows on its card, with its badge, and in a notice,
// in the MyGo client's words (mergeResult): merged, refused, failed, the
// head moved since the check, added to the queue, approved and not merged,
// and an end not known. TestApproveAndMergeFromTheInboxSnapshot and
// TestMergeAllReadySaysWhatWasQueuedOrIsUnknown hold these texts for the
// MyGo client.
func TestEachResultSaysHowItEnded(t *testing.T) {
	s := pocketdtest.New()
	s.AddPull(nsl31)
	api := newAPI()
	app := mergeApp(t, s, &ghAuth{signedIn: true}, api, 1230, 800)
	app.WaitFor(pullCard(1)+" >> label:Check merge gates", 5*time.Second)
	button := "Check merge gates"
	for _, c := range []struct {
		result      github.Result
		badge, says string
		notice      string
	}{
		{github.Result{Kind: "refused", Step: "read", Reason: "a review requests changes"}, "refused", "Refused. a review requests changes", "Couldn’t merge pull request"},
		{github.Result{Kind: "failed", Step: "approve", Reason: "GitHub answered 502"}, "failed", "Failed. GitHub answered 502", "Couldn’t merge pull request"},
		{github.Result{Kind: "refused", Step: "read", Reason: "new commit since you confirmed (dddddddddddd)", Head: strings.Repeat("d", 40), Base: "main"},
			"refused", "Refused. new commit since you confirmed (dddddddddddd)", "Couldn’t merge pull request"},
		{github.Result{Kind: "queued", Step: "merge", Method: "queue"}, "queued", "Added to the merge queue.", "Pull request queued"},
		{github.Result{Kind: "approved", Step: "read 2", Reason: "the PR has conflicts"}, "approved", "Approved, not merged. the PR has conflicts", "Approved, not merged"},
		{github.Result{Kind: "unknown", Step: "reconcile merge", Reason: "merge may be pending"}, "unknown", "Its end is unknown: check it again. merge may be pending", "Merge status unknown"},
		{github.Result{Kind: "merged", Step: "merge", Method: "SQUASH", MergedInto: "main"}, "merged", "Merged as bketelsen.", "Pull request merged"},
	} {
		api.mu.Lock()
		api.results = map[string]github.Result{"frostyard/nsl#31": c.result}
		api.mu.Unlock()
		checked(app, 1, button, "Gates pass at cccccccccccc into main · squash")
		button = "Check again"
		app.Press(pullCard(1) + " >> label:Approve and merge")
		app.WaitFor(`role:alertdialog >> text:"Approve and merge frostyard/nsl#31?"`, 3*time.Second)
		app.Press(`role:alertdialog >> role:button "Approve and merge"`)
		app.WaitFor(pullCard(1)+" >> text:"+c.says, 5*time.Second)
		app.Find(pullCard(1) + ` >> text:"` + c.badge + `"`)
		app.WaitFor(`role:alert >> text:"`+c.notice+`"`, 3*time.Second)
		app.Find("role:alert >> text:frostyard/nsl#31: " + c.says)
		app.Press("label:Dismiss message")
		app.WaitForGone("role:alert", 3*time.Second)
		if strings.HasPrefix(c.says, "Refused. new commit") {
			app.Screenshot("inbox-merge-head-moved")
		}
		app.Find(pullCard(1) + " >> label:Check again")
	}
	if got := len(api.acted()); got != 7 {
		t.Errorf("%d attempts, want 7", got)
	}
}

// mergeAllApp is mergeApp with the pull requests given added, signed in
// to GitHub, once the Inbox shows Merge all ready.
func mergeAllApp(t *testing.T, api *ghAPI, pulls ...proto.PullRequest) (*pocketdtest.Server, *nsgotest.App) {
	t.Helper()
	s := pocketdtest.New()
	for _, p := range pulls {
		s.AddPull(p)
	}
	app := mergeApp(t, s, &ghAuth{signedIn: true}, api, 1230, 900)
	app.WaitFor("text:Approve and merge every pull request whose gates pass now, one at a time, as bketelsen.", 5*time.Second)
	return s, app
}

// confirmAll starts Merge all ready and opens its confirmation once its
// checks ended.
func confirmAll(app *nsgotest.App, n int) {
	app.Press(`role:button "Merge all ready"`)
	app.WaitFor("text:Merge all ready waits for your confirmation of "+pullCount(n)+".", 5*time.Second)
	app.Press("label:Review and confirm")
	app.WaitFor("role:alertdialog", 3*time.Second)
}

// The owner merges every ready pull request from the Inbox at once: Merge
// all ready reads each one's gates, one confirmation lists them, by the
// title GitHub read, at their head commits, and each is approved and
// merged in turn; one that fails stays with its reason (ADR-0031).
// TestMergeAllReadyFromTheInboxSnapshot and
// TestTheMergeAllConfirmationShowsTheTitleGitHubRead hold this for the
// MyGo client.
func TestMergeAllReadyFromTheInbox(t *testing.T) {
	api := newAPI()
	api.each = map[string]func(*github.Preview){"frostyard/nsl#31": func(p *github.Preview) { p.Title = "Pin the runner image to ubuntu-26.04" }}
	s, app := mergeAllApp(t, api, nsl31, porter53)
	confirmAll(app, 2)
	app.Find(`role:alertdialog >> text:"Approve and merge 2 pull requests?"`)
	for _, says := range []string{
		"PocketGo approves each at the head shown as bketelsen, then merges it with the method shown, one at a time.",
		"frostyard/nsl#31 · Pin the runner image to ubuntu-26.04\nat commit " + mergeHead + " into main · squash",
		"frostyard/porter#53 · Bump golang.org/x/net from 0.46.0 to 0.47.0\nat commit " + mergeHead + " into main · squash",
	} {
		app.Find("role:alertdialog >> text:" + says)
	}
	app.Screenshot("inbox-merge-all")
	if len(api.acted()) != 0 {
		t.Fatal("it merged before the owner confirmed")
	}
	// frostyard/nsl#31 is refused at its turn; porter#53 still merges.
	api.mu.Lock()
	api.results = map[string]github.Result{"frostyard/nsl#31": {Kind: "refused", Step: "read", Reason: "a review requests changes"}}
	api.mu.Unlock()
	app.Press(`role:alertdialog >> role:button "Approve and merge 2"`)
	app.WaitFor("text:1 merged, 1 stayed, 0 not tried", 5*time.Second)
	app.Find("text:frostyard/nsl#31: Refused. a review requests changes")
	app.Find(pullCard(53) + " >> text:Merged as bketelsen.")
	app.Screenshot("inbox-merge-all-ended")
	intents := api.acted()
	if len(intents) != 2 || intents[0].Preview.Ref.String() != "frostyard/nsl#31" || intents[1].Preview.Ref.String() != "frostyard/porter#53" {
		t.Fatalf("intents %+v", intents)
	}
	for _, id := range []int64{1, 53} {
		if got := len(s.RequestsTo("GET", proto.Path(proto.PathPull, id))); got != 1 {
			t.Errorf("pull %d read %d times from pocketd at its turn, want 1", id, got)
		}
	}
	app.Press(`role:button "Dismiss"`)
	app.WaitForGone("text:1 merged, 1 stayed, 0 not tried", 3*time.Second)
}

// The confirmation says what it leaves out: a dependency bot's risk:high
// pull request goes without its diff opened, and it names how many that
// pocketd backs do not pass now, whose cards say why.
// TestTheMergeAllConfirmationSaysWhereItWaivesOpenDiff holds the first for
// the MyGo client, and its mergeAllConfirmView says the second.
func TestTheMergeAllConfirmationSaysWhatItLeavesOut(t *testing.T) {
	api := newAPI()
	api.each = map[string]func(*github.Preview){
		"frostyard/porter#53": func(p *github.Preview) { p.RiskHigh, p.DependencyBot, p.Bot = true, true, "dependabot" },
		"frostyard/porter#54": func(p *github.Preview) { p.Eligible, p.Reason = false, "a required check failed" },
		"frostyard/porter#55": func(p *github.Preview) { p.Eligible, p.Reason = false, "the PR has conflicts" },
	}
	_, app := mergeAllApp(t, api, nsl31, porter53, porter54, porter55)
	confirmAll(app, 2)
	app.Find("role:alertdialog >> text:frostyard/porter#53 · Bump golang.org/x/net from 0.46.0 to 0.47.0\nat commit " + mergeHead +
		" into main · squash · risk:high, by a dependency bot: no diff opened")
	app.Find("role:alertdialog >> text:2 other pull requests pocketd backs do not pass now: each card says why.")
	if app.Exists("role:alertdialog >> text:frostyard/porter#54") || app.Exists("role:alertdialog >> text:frostyard/porter#55") {
		t.Error("the confirmation lists a pull request whose gates refuse")
	}
	app.Screenshot("inbox-merge-all-left-out")
	app.Press("role:alertdialog >> role:button Cancel")
	app.Press("label:Drop the list")
	app.WaitFor(`role:button "Merge all ready"`, 3*time.Second)
	// Each card says why it was left out, and the waived one's own
	// Approve and merge waits for its diff.
	app.Find(pullCard(54) + " >> text:a required check failed")
	app.Find(pullCard(55) + " >> text:the PR has conflicts")
	app.Find(pullCard(53) + " >> text:it has risk:high: open its diff first")
	if app.Enabled(pullCard(53) + " >> label:Approve and merge") {
		t.Error("the card's Approve and merge is on before the diff")
	}
	if len(api.acted()) != 0 {
		t.Fatalf("it merged: %+v", api.acted())
	}
}

// Merge all ready is off, and says why, while pocketd backs none of the
// pull requests that wait (ADR-0031). TestMergeAllReadyIsOffWithNothingBacked
// holds this for the MyGo client.
func TestMergeAllReadyIsOffWithNothingBacked(t *testing.T) {
	p := nsl31
	review := *p.Review
	review.Verdict = proto.ReviewNotReady
	p.Review = &review
	s := pocketdtest.New()
	s.AddPull(p)
	app := mergeApp(t, s, &ghAuth{signedIn: true}, newAPI(), 1230, 800)
	app.WaitFor("text:Merge all ready is off: pocketd backs no pull request here now: none has a ready cross-review with green CI at its head.", 5*time.Second)
	if app.Enabled(`role:button "Merge all ready"`) {
		t.Error("Merge all ready is on with nothing backed")
	}
}

// While the run reads the gates, it says how many it read, and Stop ends
// it with nothing merged; while it merges, it says which turn and whose,
// and Stop after this one lets the merge under way end and tries no more.
// TestMergeAllStopEndsAfterTheMergeUnderWay holds the rule in state; the
// MyGo client's bar draws the same texts (mergeAllBar).
func TestMergeAllReadySaysWhereItStands(t *testing.T) {
	api := newAPI()
	first, readOn := holdBack(t)
	api.first = map[string]chan struct{}{"frostyard/porter#53": first}
	_, app := mergeAllApp(t, api, nsl31, porter53)
	app.Press(`role:button "Merge all ready"`)
	app.WaitFor("text:Reading the merge gates on GitHub: 1 of 2 read…", 5*time.Second)
	app.Press(`role:button "Stop"`)
	app.WaitFor("text:0 merged, 0 stayed, 0 not tried", 3*time.Second)
	app.Find("text:You stopped it: nothing was merged.")
	app.Press(`role:button "Dismiss"`)
	readOn()

	acts, actOn := holdBack(t)
	api.mu.Lock()
	api.acts = map[string]chan struct{}{"frostyard/nsl#31": acts}
	api.mu.Unlock()
	app.WaitFor(`role:button "Merge all ready"`, 3*time.Second)
	confirmAll(app, 2)
	app.Press(`role:alertdialog >> role:button "Approve and merge 2"`)
	app.WaitFor("text:Approving and merging 1 of 2 as bketelsen: frostyard/nsl#31…", 5*time.Second)
	app.Press(`role:button "Stop after this one"`)
	app.WaitFor("text:Approving and merging 1 of 2 as bketelsen: frostyard/nsl#31 · stopping after this one…", 3*time.Second)
	if app.Enabled(`role:button "Stop after this one"`) {
		t.Error("Stop after this one is on once pressed")
	}
	app.Screenshot("inbox-merge-all-progress")
	actOn()
	app.WaitFor("text:1 merged, 0 stayed, 1 not tried", 5*time.Second)
	app.Find("text:You stopped it: the rest were not tried.")
	app.Find(`text:"not tried"`)
	if got := api.acted(); len(got) != 1 || got[0].Preview.Ref.String() != "frostyard/nsl#31" {
		t.Fatalf("intents %+v", got)
	}
}

// Dependabot's pull requests on one base all go in one run: once the first
// merged and moved the base, each other is checked again at the new base
// at its turn, which the card shows, and merges there, or waits for
// Dependabot to rebase it once it is behind (ADR-0038, ADR-0039).
// TestMergeAllReadyChecksAgainAtTheNewBaseSnapshot holds this for the MyGo
// client.
func TestMergeAllReadyChecksAgainAtTheNewBase(t *testing.T) {
	api := newAPI()
	again, readAgain := holdBack(t)
	api.preview.RiskHigh, api.preview.DependencyBot, api.preview.Bot = true, true, "dependabot"
	api.bases = map[string]string{"frostyard/nsl:main": mergeBase, "frostyard/porter:main": mergeBase}
	api.strict = map[string]bool{"frostyard/porter#55": true}
	api.again = map[string]chan struct{}{"frostyard/porter#54": again}
	_, app := mergeAllApp(t, api, nsl31, porter53, porter54, porter55)
	confirmAll(app, 4)
	app.Find("role:alertdialog >> text:One listed without its diff opened whose base an earlier merge moved is checked again at the new base first.")
	app.Press(`role:alertdialog >> role:button "Approve and merge 4"`)
	app.WaitFor("text:Approving and merging 3 of 4 as bketelsen: frostyard/porter#54 · checking it again at its new base…", 5*time.Second)
	app.Screenshot("inbox-merge-all-recheck")
	readAgain()
	app.WaitFor("text:3 merged, 1 waits for Dependabot to rebase, 0 stayed, 0 not tried; 2 checked again at a new base", 5*time.Second)
	app.Find("text:frostyard/porter#55: waits for Dependabot to rebase it (the PR is behind its base)")
	app.Screenshot("inbox-merge-all-recheck-ended")
	intents := api.acted()
	if len(intents) != 3 {
		t.Fatalf("intents %+v", intents)
	}
	if in := intents[2]; in.Preview.Ref != (github.Ref{Owner: "frostyard", Name: "porter", Number: 54}) || in.Preview.BaseOid == mergeBase || !in.WaiveDiff {
		t.Fatalf("porter#54's intent %+v", in)
	}
}

// A Dependabot pull request the run's own merge left behind its base, or
// in conflict with it, waits for Dependabot to rebase it: the summary
// counts it apart from the failures, and lists it after them, by the bot's
// name; one whose approval went in before the merge was refused says so
// (ADR-0039). TestMergeAllReadyCountsRebaseWaitsApartSnapshot holds this
// for the MyGo client.
func TestMergeAllReadyCountsRebaseWaitsApart(t *testing.T) {
	api := newAPI()
	api.preview.RiskHigh, api.preview.DependencyBot, api.preview.Bot = true, true, "dependabot"
	api.bases = map[string]string{"frostyard/nsl:main": mergeBase, "frostyard/porter:main": mergeBase}
	api.strict = map[string]bool{"frostyard/porter#54": true}
	api.results = map[string]github.Result{
		"frostyard/nsl#31":    {Kind: "refused", Step: "read", Reason: "a review requests changes"},
		"frostyard/porter#55": {Kind: "approved", Step: "read 2", Reason: "the PR has conflicts", Head: mergeHead, Base: "main"},
	}
	_, app := mergeAllApp(t, api, nsl31, porter53, porter54, porter55)
	confirmAll(app, 4)
	app.Press(`role:alertdialog >> role:button "Approve and merge 4"`)
	app.WaitFor("text:1 merged, 2 wait for Dependabot to rebase, 1 stayed, 0 not tried; 2 checked again at a new base", 5*time.Second)
	inOrder(t, app, "frostyard/nsl#31: Refused. a review requests changes",
		"frostyard/porter#54: waits for Dependabot to rebase it (the PR is behind its base)",
		"frostyard/porter#55: waits for Dependabot to rebase it (approved as bketelsen, not merged: the PR has conflicts)")
	app.Screenshot("inbox-merge-all-rebase-wait")
	if app.Exists("text:frostyard/porter#54 (checked again at its new base): Refused.") {
		t.Error("the wait is listed among the failures")
	}
}
