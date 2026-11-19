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
	openErr  error
	signedIn bool
	wait     chan struct{}
	adopted  int
}

func (f *ghAuth) Open() error { return f.openErr }
func (f *ghAuth) Start(context.Context) (github.DeviceCode, error) {
	return github.DeviceCode{DeviceCode: "dev", UserCode: "WDJB-MJHT", Expires: time.Now().Add(time.Hour), Interval: time.Second}, nil
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
func (f *ghAuth) SaveAgain() error   { return nil }
func (f *ghAuth) Forget() error      { f.mu.Lock(); defer f.mu.Unlock(); f.signedIn = false; return nil }
func (f *ghAuth) Lease(context.Context) (string, int, func(), error) {
	return "gho_x", 1, func() {}, nil
}
func (f *ghAuth) Status() github.AppStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.AppStatus{SignedIn: f.signedIn, Login: github.Login}
}

// ghAPI is GitHub as the owner's client sees it.
type ghAPI struct {
	mu      sync.Mutex
	preview github.Preview
	intents []github.Intent
}

func (f *ghAPI) CheckApp(_ context.Context, asked []string) (github.AppCheck, error) {
	owners := map[string]string{}
	for _, o := range asked {
		owners[o] = "" // installed, with every permission PocketGo needs
	}
	return github.AppCheck{Login: github.Login, Owners: owners}, nil
}
func (f *ghAPI) Preview(_ context.Context, ref github.Ref) (github.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.preview
	p.Ref = ref
	return p, nil
}
func (f *ghAPI) Act(_ context.Context, in github.Intent, op string, _ func(string) error) github.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intents = append(f.intents, in)
	return github.Result{Op: op, Kind: "merged", Method: in.Preview.Method}
}

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
	api := &ghAPI{preview: github.Preview{Head: mergeHead, BaseOid: mergeBase, Method: "SQUASH", Eligible: true}}
	a, err := New(Options{Version: "test", Post: x.loop.Post, Store: x.store, Creds: x.creds, After: never, Transport: x.srv.Transport(),
		GitHubAuth: auth, GitHubClient: func(string) state.GitHubAPI { return api }})
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
	x.hasAll(t, "Gates pass at cccccccccccc · squash", "Approve + Merge", "Check Again")
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

// A verdict pocketd files while the confirmation is open counts: the
// merge reads the pull request as the desktop holds it at the click
// (Codex review of #24, round 2).
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
	p, _ := x.a.currentPull(1)
	review := *p.Review
	review.Verdict, review.Findings = proto.ReviewNotReady, []string{"A test is missing."}
	p.Review = &review
	x.srv.Emit(proto.Event{Type: proto.EventPull, Pull: &p})
	x.until(t, func() bool {
		cur, _ := x.a.currentPull(1)
		return cur.Review != nil && cur.Review.Verdict == proto.ReviewNotReady
	})
	x.click(t, "Approve + Merge")
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.intents) != 0 {
		t.Fatalf("it merged on a verdict pocketd replaced: %+v", api.intents)
	}
}
