package app

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// ghAuth is the owner's App sign-in, in memory; its poll waits for wait.
type ghAuth struct {
	mu       sync.Mutex
	unsaved  bool
	saved    int
	openErr  error
	signedIn bool
	wait     chan struct{}
	adopted  int
	reason   string
	forgot   int
	// forgetWait holds a sign-out's keyring delete until closed.
	forgetWait chan struct{}
}

func (f *ghAuth) Open() error { return f.openErr }
func (f *ghAuth) Start(context.Context) (github.DeviceCode, error) {
	return github.DeviceCode{DeviceCode: "dev", UserCode: "WDJB-MJHT", Expires: farFuture, Interval: time.Second}, nil
}
func (f *ghAuth) Poll(ctx context.Context, _ github.DeviceCode) (github.AppToken, error) {
	select {
	case <-f.wait:
	case <-ctx.Done():
		return github.AppToken{}, ctx.Err()
	}
	return github.AppToken{ClientID: github.AppClientID, Access: "gho_x", Refresh: "ghr_x"}, nil
}
func (f *ghAuth) BeginSignIn() int { return 1 }
func (f *ghAuth) CancelSignIn(int) {}
func (f *ghAuth) Adopt(github.AppToken, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signedIn, f.adopted = true, f.adopted+1
	return nil
}
func (f *ghAuth) Invalidate(int)     {}
func (f *ghAuth) DeleteAgain() error { return nil }
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
	w := f.forgetWait
	return func() error {
		if w != nil {
			<-w
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.forgot++
		return nil
	}, nil
}
func (f *ghAuth) Lease(context.Context) (string, int, func(), error) {
	return "gho_x", 1, func() {}, nil
}
func (f *ghAuth) Status() github.AppStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.AppStatus{SignedIn: f.signedIn, Login: github.Login, Unsaved: f.unsaved, Reason: f.reason}
}

// ghAPI is GitHub as the owner's client sees it.
type ghAPI struct {
	mu      sync.Mutex
	preview github.Preview
	intents []github.Intent
	// crs are the cross-reviews each preview was asked to count, by
	// reference; results answer an approve and merge of a reference.
	crs     map[string][]github.CrossReview
	results map[string]github.Result
	// titles are the titles GitHub reads, by reference.
	titles map[string]string
}

func (f *ghAPI) CheckApp(_ context.Context, asked []string) (github.AppCheck, error) {
	owners := map[string]string{}
	for _, o := range asked {
		owners[o] = "" // installed, with every permission PocketGo needs
	}
	return github.AppCheck{Login: github.Login, Owners: owners}, nil
}
func (f *ghAPI) Preview(_ context.Context, ref github.Ref, cr github.CrossReview) (github.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.crs == nil {
		f.crs = map[string][]github.CrossReview{}
	}
	f.crs[ref.String()] = append(f.crs[ref.String()], cr)
	p := f.preview
	p.Ref = ref
	if title, ok := f.titles[ref.String()]; ok {
		p.Title = title
	}
	return p, nil
}
func (f *ghAPI) Act(_ context.Context, in github.Intent, op string, _ func(string) error) github.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intents = append(f.intents, in)
	if r, ok := f.results[in.Preview.Ref.String()]; ok {
		r.Op = op
		return r
	}
	return github.Result{Op: op, Kind: "merged", Method: in.Preview.Method}
}

// farFuture is a device code's expiry no test reaches: no wall clock
// decides a test (Codex review of #24, round 6).
var farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	// mergeHead is the head of seedTeam's frostyard/nsl#31 and of its
	// ready verdict: the gates read it.
	mergeHead = "cccccccccccccccccccccccccccccccccccccccc"
	mergeBase = "3333333333333333333333333333333333333333"
)

// newMergeApp is a test app with the team seeded and the owner's GitHub
// sign-in and client faked.
func newMergeApp(t *testing.T, auth *ghAuth) (*testApp, *ghAPI) {
	t.Helper()
	x := newTestApp(t)
	x.seedTeam()
	api := &ghAPI{preview: github.Preview{Head: mergeHead, BaseRef: "main", BaseOid: mergeBase, Method: "SQUASH", Eligible: true}}
	opts := x.options()
	opts.GitHubAuth, opts.GitHubClient = auth, func(string) state.GitHubAPI { return api }
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(a.View, 1280, 900, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
	a.Start()
	x.settle()
	x.connect(t)
	// The GitHub sign-in opens on its own goroutine (Codex review of #24,
	// round 2).
	x.until(t, a.gh.Opened)
	return x, api
}

// The owner signs in to GitHub from a pull request's card, checks its
// gates, confirms, and it is approved and merged as the owner at the head
// shown (ADR-0018).
func TestApproveAndMergeFromTheInboxSnapshot(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	x, api := newMergeApp(t, auth)
	x.hasAll(t, "frostyard/nsl#31 waits for your review", "Sign in to GitHub to approve and merge it as bketelsen.")
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignInCode })
	x.hasAll(t, "Sign in to GitHub", "WDJB-MJHT", "Open GitHub", "Copy Code")
	x.snapshot(t, "github-sign-in")
	x.click(t, "Open GitHub")
	if got := x.tt.OpenedURLs(); !slices.Equal(got, []string{github.DeviceURL}) {
		t.Fatalf("opened %q", got)
	}
	close(auth.wait)
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	if x.a.ghSignIn || x.tt.HasText("WDJB-MJHT") {
		t.Error("the sign-in window stays after the sign-in")
	}
	x.click(t, "Check to Merge")
	x.until(t, func() bool {
		return x.a.gh.Merge(github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}).Phase == state.MergeChecked
	})
	x.hasAll(t, "Gates pass at cccccccccccc into main · squash", "Approve + Merge", "Check Again")
	x.snapshot(t, "inbox-merge")
	x.click(t, "Approve + Merge")
	x.hasAll(t, "Approve and merge frostyard/nsl#31?")
	if len(api.intents) != 0 {
		t.Fatal("it merged before the owner confirmed")
	}
	x.click(t, "Approve + Merge")
	x.until(t, func() bool {
		return x.a.gh.Merge(github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}).Phase == state.MergeDone
	})
	x.hasAll(t, "Merged as bketelsen.")
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.intents) != 1 || api.intents[0].Preview.Head != mergeHead || api.intents[0].Mode != "single" {
		t.Fatalf("intents %+v", api.intents)
	}
}

// A risk:high pull request merges only once its diff was opened, at the
// head the gates read.
func TestARiskyPullRequestNeedsItsDiffOpened(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	api.preview.RiskHigh = true
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.click(t, "Check to Merge")
	ref := github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}
	x.until(t, func() bool { return x.a.gh.Merge(ref).Phase == state.MergeChecked })
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Approve + Merge"); !ok || !b.Disabled {
		t.Fatalf("Approve + Merge before the diff: %+v, %v", b, ok)
	}
	x.click(t, "Open Diff")
	if got := x.tt.OpenedURLs(); len(got) != 1 || got[0] != "https://github.com/frostyard/nsl/compare/"+mergeBase+"..."+mergeHead {
		t.Fatalf("opened %q", got)
	}
	x.settle()
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Approve + Merge"); !ok || b.Disabled {
		t.Fatalf("Approve + Merge after the diff: %+v, %v", b, ok)
	}
}

// A sign-in another PocketGo holds turns approve and merge off, and the
// card says why.
func TestApproveAndMergeOffSaysWhy(t *testing.T) {
	x, _ := newMergeApp(t, &ghAuth{openErr: errors.New("another PocketGo holds the GitHub sign-in")})
	x.until(t, func() bool { return x.a.gh.Off() != "" && !strings.Contains(x.a.gh.Off(), "not open yet") })
	x.hasAll(t, "Approve and merge are off: another PocketGo holds the GitHub sign-in")
	if x.tt.HasText("Sign In to GitHub") {
		t.Error("an off sign-in offers Sign In to GitHub")
	}
}

// A verdict pocketd files while the confirmation is open counts, even one
// whose event never came: the merge reads the pull request from pocketd
// at the click (Codex review of #24, rounds 2 and 4).
func TestAVerdictFiledDuringTheConfirmationStopsTheMerge(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.click(t, "Check to Merge")
	ref := github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}
	x.until(t, func() bool { return x.a.gh.Merge(ref).Phase == state.MergeChecked })
	x.click(t, "Approve + Merge")
	x.hasAll(t, "Approve and merge frostyard/nsl#31?")
	var p proto.PullRequest
	for _, q := range x.a.st.Data.Pulls() {
		if q.ID == 1 {
			p = q
		}
	}
	review := *p.Review
	review.Verdict, review.Findings = proto.ReviewNotReady, []string{"A test is missing."}
	p.Review = &review
	x.srv.AddPull(p) // no event: the desktop missed it
	x.click(t, "Approve + Merge")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.intents) != 0 {
		t.Fatalf("it merged on a verdict pocketd replaced: %+v", api.intents)
	}
}

// A refreshed sign-in the keyring did not keep shows in the status bar,
// whatever the Inbox holds, with Save Again (Codex review of #24, round 4).
func TestAnUnsavedSignInShowsInTheStatusBar(t *testing.T) {
	auth := &ghAuth{signedIn: true, unsaved: true}
	x, _ := newMergeApp(t, auth)
	x.hasAll(t, "GitHub sign-in not saved")
	x.click(t, "GitHub sign-in not saved")
	x.until(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return auth.saved == 1 })
	x.settle()
	if x.tt.HasText("GitHub sign-in not saved") {
		t.Error("the warning stays after the save")
	}
}

// The sign-in screen of pocketd offers Save Again too: the GitHub sign-in
// opens without pocketd (Codex review of #24, round 5).
func TestTheSignInScreenSavesTheGitHubSignInAgain(t *testing.T) {
	x := newTestApp(t)
	auth := &ghAuth{signedIn: true, unsaved: true}
	opts := x.options()
	opts.GitHubAuth, opts.GitHubClient = auth, func(string) state.GitHubAPI { return &ghAPI{} }
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(a.View, 1280, 900, ui.TesterOptions{})
	a.Start()
	x.until(t, a.gh.Opened)
	x.hasAll(t, "Connect to pocketd", "The keyring did not keep the refreshed GitHub sign-in.")
	x.click(t, "Save Again")
	x.until(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return auth.saved == 1 })
}

// pocketd's own answer at the click must back the head too: an event that
// came after the read began wins the merge of the rows, though it may be a
// replay older than the answer (Codex review of #24, round 7).
func TestPocketdsAnswerMustBackTheHead(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.click(t, "Check to Merge")
	ref := github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}
	x.until(t, func() bool { return x.a.gh.Merge(ref).Phase == state.MergeChecked })
	x.click(t, "Approve + Merge")
	x.hasAll(t, "Approve and merge frostyard/nsl#31?")
	var backed proto.PullRequest
	for _, q := range x.a.st.Data.Pulls() {
		if q.ID == 1 {
			backed = q
		}
	}
	notBacked := backed
	review := *backed.Review
	review.Verdict = proto.ReviewNotReady
	notBacked.Review = &review
	x.srv.AddPull(notBacked) // pocketd's answer, with no event
	before := len(x.srv.RequestsTo("GET", proto.PathPulls))
	release := x.srv.HoldAnswerOnce("GET", proto.PathPulls)
	x.click(t, "Approve + Merge")
	x.until(t, func() bool { return len(x.srv.RequestsTo("GET", proto.PathPulls)) == before+1 })
	// A replayed event of the backed row reaches the desktop after the
	// read began, and before its answer.
	replay := backed
	replayReview := *backed.Review
	replayReview.ID = 99
	replay.Review = &replayReview
	x.srv.EmitOnly(proto.Event{Type: proto.EventPull, Pull: &replay})
	x.until(t, func() bool {
		for _, q := range x.a.st.Data.Pulls() {
			if q.ID == 1 && q.Review != nil && q.Review.ID == 99 {
				return true
			}
		}
		return false
	})
	release()
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	x.settle()
	api.mu.Lock()
	n := len(api.intents)
	api.mu.Unlock()
	if n != 0 {
		t.Fatalf("it merged on a row pocketd's answer did not back: %+v", api.intents)
	}
	x.hasAll(t, "pocketd's answer does not back")
}

// The sign-in screen of pocketd offers GitHub's sign-out too, and says why
// a kept sign-in is not used: the GitHub sign-in opens without pocketd
// (Codex review of #24, round 7).
func TestTheSignInScreenSignsOutOfGitHubSnapshot(t *testing.T) {
	x := newTestApp(t)
	auth := &ghAuth{reason: "the GitHub sign-in in the keyring is not one: a field is empty"}
	opts := x.options()
	opts.GitHubAuth, opts.GitHubClient = auth, func(string) state.GitHubAPI { return &ghAPI{} }
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	x.a = a
	x.tt = ui.NewTesterWith(a.View, 1280, 900, ui.TesterOptions{})
	a.Start()
	x.until(t, a.gh.Opened)
	x.hasAll(t, "Connect to pocketd", "The kept GitHub sign-in is not used: the GitHub sign-in in the keyring is not one: a field is empty")
	x.snapshot(t, "sign-in-github-rejected")
	x.click(t, "Sign Out of GitHub")
	x.hasAll(t, "Sign out of GitHub?")
	x.click(t, "Sign Out")
	x.until(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return auth.forgot == 1 })
	x.settle()
	if x.tt.HasText("The kept GitHub sign-in is not used") {
		t.Error("the problem stays after the sign-out")
	}
}

// The confirmation names the base GitHub read, which the merge holds to;
// a base GitHub reads otherwise than pocketd backs no merge at all (Codex
// review of #24, round 9; of #26, round 5).
func TestTheConfirmationNamesTheBaseGitHubRead(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.click(t, "Check to Merge")
	ref := github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}
	x.until(t, func() bool { return x.a.gh.Merge(ref).Phase == state.MergeChecked })
	x.click(t, "Approve + Merge")
	x.until(t, func() bool { return x.a.dialogs.Showing() })
	if !x.tt.HasText("PocketGo approves it at cccccccccccc as bketelsen, then merges it into main with squash, if the gates still pass at that head.") {
		t.Fatal("the confirmation does not name the base GitHub read")
	}
	x.click(t, "Cancel")
	// Retargeted on GitHub, at the same head: pocketd's verdict is of main.
	api.mu.Lock()
	api.preview.BaseRef = "release"
	api.mu.Unlock()
	x.click(t, "Check Again")
	x.until(t, func() bool { return x.a.gh.Merge(ref).Phase == state.MergeChecked })
	x.settle()
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Approve + Merge"); !ok || !b.Disabled {
		t.Fatalf("Approve + Merge on a retargeted pull request: %+v, %v", b, ok)
	}
	if ok, why := x.a.gh.CanMerge(x.a.st.Data.Pulls()[0]); ok || !strings.Contains(why, "merging into main, the gates into release") {
		t.Fatalf("CanMerge %v %q", ok, why)
	}
}

// A GitHub device flow begun before pocketd signed the window out stays in
// view on pocketd's sign-in screen (Codex review of #24, round 9).
func TestTheGitHubCodeStaysWhenPocketdSignsOut(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	x, _ := newMergeApp(t, auth)
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignInCode })
	x.srv.Revoke()
	x.a.st.Refresh()
	x.until(t, func() bool { return !x.a.st.SignedIn() })
	x.hasAll(t, "Connect to pocketd", "WDJB-MJHT")
}

// While signing out of GitHub, the card's Sign In to GitHub is off: the
// window would wait on a flow that cannot begin (Codex review of #24,
// round 11).
func TestSignInToGitHubIsOffWhileSigningOut(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, _ := newMergeApp(t, auth)
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	release := make(chan struct{})
	auth.mu.Lock()
	auth.forgetWait = release
	auth.mu.Unlock()
	x.a.confirmGitHubSignOut()
	x.settle()
	x.hasAll(t, "Sign out of GitHub?")
	x.click(t, "Sign Out")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SigningOut })
	x.settle()
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Sign In to GitHub"); !ok || !b.Disabled {
		t.Fatalf("Sign In to GitHub while signing out: %+v, %v", b, ok)
	}
	close(release)
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedOut })
}
