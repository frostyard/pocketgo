package state

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/frostui/frostuitest"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/proto"
)

// fakeAuth is the owner's App sign-in, in memory. Its waits hold Open,
// Poll and Forget until the test closes them.
type fakeAuth struct {
	mu                     sync.Mutex
	openErr                error
	signedIn, deleteFailed bool
	gen                    int
	adopted                []github.AppToken
	invalid                []int
	tok                    github.AppToken
	pollErr, leaseErr      error
	openWait, pollWait     chan struct{}
	forgetWait             chan struct{}
	forgot, deletedAgain   int
}

func wait(ch chan struct{}) {
	if ch != nil {
		<-ch
	}
}

func (f *fakeAuth) Open() error {
	f.mu.Lock()
	w := f.openWait
	f.mu.Unlock()
	wait(w)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openErr
}
func (f *fakeAuth) Start(context.Context) (github.DeviceCode, error) {
	return github.DeviceCode{DeviceCode: "dev", UserCode: "ABCD-1234", Expires: time.Now().Add(time.Hour), Interval: time.Second}, nil
}
func (f *fakeAuth) Poll(ctx context.Context, _ github.DeviceCode) (github.AppToken, error) {
	f.mu.Lock()
	w, tok, err := f.pollWait, f.tok, f.pollErr
	f.mu.Unlock()
	if w != nil {
		select {
		case <-w:
		case <-ctx.Done():
			return github.AppToken{}, ctx.Err()
		}
	}
	return tok, err
}
func (f *fakeAuth) BeginSignIn() int { f.mu.Lock(); defer f.mu.Unlock(); f.gen++; return f.gen }
func (f *fakeAuth) CancelSignIn(int) {}
func (f *fakeAuth) Adopt(tok github.AppToken, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopted, f.signedIn = append(f.adopted, tok), true
	return nil
}
func (f *fakeAuth) Invalidate(gen int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalid = append(f.invalid, gen)
}
func (f *fakeAuth) Forget() error {
	f.mu.Lock()
	w := f.forgetWait
	f.mu.Unlock()
	wait(w)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signedIn = false
	f.forgot++
	return nil
}
func (f *fakeAuth) DeleteAgain() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedAgain++
	f.deleteFailed = false
	return nil
}
func (f *fakeAuth) Lease(context.Context) (string, int, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseErr != nil {
		return "", 0, nil, f.leaseErr
	}
	return "gho_token", 7, func() {}, nil
}
func (f *fakeAuth) Status() github.AppStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.AppStatus{SignedIn: f.signedIn, Login: github.Login, DeleteFailed: f.deleteFailed}
}

// fakeAPI is GitHub as the owner's client sees it.
type fakeAPI struct {
	mu          sync.Mutex
	login       string
	owners      map[string]string
	preview     github.Preview
	previewWait chan struct{}
	err         error
	intents     []github.Intent
	result      github.Result
}

func (f *fakeAPI) CheckApp(context.Context, []string) (github.AppCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	owners := f.owners
	if owners == nil {
		owners = map[string]string{github.Login: "", "frostyard": ""}
	}
	return github.AppCheck{Login: f.login, Owners: owners}, nil
}
func (f *fakeAPI) Preview(_ context.Context, ref github.Ref) (github.Preview, error) {
	f.mu.Lock()
	w := f.previewWait
	f.mu.Unlock()
	wait(w)
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.preview
	p.Ref = ref
	return p, f.err
}
func (f *fakeAPI) Act(_ context.Context, in github.Intent, op string, _ func(string) error) github.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intents = append(f.intents, in)
	r := f.result
	r.Op = op
	return r
}

const (
	headX = "1111111111111111111111111111111111111111"
	headY = "2222222222222222222222222222222222222222"
	baseX = "3333333333333333333333333333333333333333"
)

type ghHarness struct {
	loop *frostuitest.Loop
	auth *fakeAuth
	api  *fakeAPI
	gh   *GitHub
}

func newGHHarness(t *testing.T) *ghHarness {
	t.Helper()
	h := &ghHarness{loop: frostuitest.NewLoop(), auth: &fakeAuth{tok: github.AppToken{ClientID: github.AppClientID, Access: "gho_new", Refresh: "ghr_new"}},
		api: &fakeAPI{login: github.Login, preview: github.Preview{Head: headX, BaseOid: baseX, Method: "SQUASH", Eligible: true},
			result: github.Result{Kind: "merged", Method: "SQUASH"}}}
	h.gh = NewGitHub(GitHubOptions{Post: h.loop.Post, Auth: h.auth, NewClient: func(string) GitHubAPI { return h.api },
		Owners: []string{github.Login, "frostyard"}})
	opened := false
	h.gh.Open(func() { opened = true })
	h.loop.RunUntil(t, func() bool { return opened })
	return h
}

func (h *ghHarness) signIn(t *testing.T) {
	t.Helper()
	h.gh.BeginSignIn()
	h.loop.RunUntil(t, func() bool { p, _, fail := h.gh.SignIn(); return p == SignedIn || fail != "" })
	if p, _, fail := h.gh.SignIn(); p != SignedIn {
		t.Fatalf("sign-in: phase %v, %q", p, fail)
	}
}

var ref31 = github.Ref{Owner: "frostyard", Name: "nsl", Number: 31}

// pull31 is ref31 as pocketd links it, with a ready verdict at headX.
var pull31 = proto.PullRequest{ID: 1, Repository: "frostyard/nsl", Number: 31, HeadSHA: headX,
	Review: &proto.PullReview{Verdict: proto.ReviewReady, Head: headX}}

// at is pull31 with its verdict at head.
func at(head string) proto.PullRequest {
	p := pull31
	r := *p.Review
	r.Head = head
	p.Review, p.HeadSHA = &r, head
	return p
}

// The device flow keeps a pair only when GitHub names the owner and the
// App (ADR-0018).
func TestTheSignInKeepsOnlyTheOwnersPair(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	if len(h.auth.adopted) != 1 || h.auth.adopted[0].Login != github.Login {
		t.Fatalf("adopted %+v", h.auth.adopted)
	}
	for name, c := range map[string]struct {
		login, client, want string
	}{
		"the bot's login": {"frostyard-machines", github.AppClientID, "acts only as the owner"},
		"another App":     {github.Login, "Iv1.other", "another App"},
	} {
		h := newGHHarness(t)
		h.api.login, h.auth.tok.ClientID = c.login, c.client
		h.gh.BeginSignIn()
		h.loop.RunUntil(t, func() bool { _, _, fail := h.gh.SignIn(); return fail != "" })
		if p, _, fail := h.gh.SignIn(); p != SignedOut || !strings.Contains(fail, c.want) || len(h.auth.adopted) != 0 {
			t.Errorf("%s: phase %v, %q, adopted %+v", name, p, fail, h.auth.adopted)
		}
	}
}

// A sign-in cancelled while its code shows keeps nothing, whatever the
// poll brings after.
func TestACancelledSignInKeepsNothing(t *testing.T) {
	h := newGHHarness(t)
	h.auth.pollWait = make(chan struct{})
	h.gh.BeginSignIn()
	h.loop.RunUntil(t, func() bool { p, _, _ := h.gh.SignIn(); return p == SignInCode })
	if _, code, _ := h.gh.SignIn(); code.UserCode != "ABCD-1234" {
		t.Fatalf("code %+v", code)
	}
	h.gh.CancelSignIn()
	close(h.auth.pollWait)
	h.loop.Flush()
	if p, _, _ := h.gh.SignIn(); p != SignedOut || len(h.auth.adopted) != 0 {
		t.Fatalf("phase %v, adopted %+v", p, h.auth.adopted)
	}
}

// A sign-in that cannot open (another PocketGo holds it) turns approve and
// merge off.
func TestASignInThatCannotOpenIsOff(t *testing.T) {
	loop := frostuitest.NewLoop()
	auth := &fakeAuth{openErr: errors.New("another PocketGo holds the GitHub sign-in")}
	g := NewGitHub(GitHubOptions{Post: loop.Post, Auth: auth, NewClient: func(string) GitHubAPI { return &fakeAPI{} }})
	opened := false
	g.Open(func() { opened = true })
	loop.RunUntil(t, func() bool { return opened })
	if g.On() || !strings.Contains(g.Off(), "another PocketGo") {
		t.Fatalf("on %v, off %q", g.On(), g.Off())
	}
	g.Check(ref31)
	if g.Merge(ref31).Phase != MergeNone {
		t.Fatal("a check ran with the sign-in off")
	}
	if off := NewGitHub(GitHubOptions{Post: loop.Post}); off.On() || off.Off() == "" {
		t.Fatal("no keyring is on")
	}
}

// A pull request merges only after a check whose gates passed, at the
// head shown; risk:high needs its diff opened at that head (ADR-0018).
func TestApproveAndMergeFollowsTheGates(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	if ok, why := h.gh.CanMerge(pull31); ok || why == "" {
		t.Fatalf("before a check: %v %q", ok, why)
	}
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); !ok {
		t.Fatalf("an eligible check: %q", why)
	}
	done := false
	h.gh.ApproveAndMerge(pull31, func(github.Result) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if m := h.gh.Merge(ref31); m.Phase != MergeDone || m.Result.Kind != "merged" {
		t.Fatalf("merge %+v", m)
	}
	if len(h.api.intents) != 1 || h.api.intents[0].Preview.Head != headX || h.api.intents[0].Mode != "single" || h.api.intents[0].Preview.Ref != ref31 {
		t.Fatalf("intents %+v", h.api.intents)
	}
	if ok, _ := h.gh.CanMerge(pull31); ok {
		t.Fatal("a done merge can run again without a check")
	}

	// Refused by the gates: no button, and why.
	h.api.preview = github.Preview{Head: headX, Eligible: false, Reason: "the label ready-for-human is missing"}
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); ok || why != "the label ready-for-human is missing" {
		t.Fatalf("refused: %v %q", ok, why)
	}

	// risk:high: the diff first, and only for the head it showed.
	h.api.preview = github.Preview{Head: headX, BaseOid: baseX, Method: "SQUASH", Eligible: true, RiskHigh: true}
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); ok || !strings.Contains(why, "risk:high") {
		t.Fatalf("risk:high before the diff: %v %q", ok, why)
	}
	h.gh.OpenedDiff(ref31, h.gh.Merge(ref31).Preview.Head)
	if ok, _ := h.gh.CanMerge(pull31); !ok {
		t.Fatal("risk:high after the diff cannot merge")
	}
	h.api.preview.Head = headY
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, _ := h.gh.CanMerge(pull31); ok {
		t.Fatal("a new head merges on the old head's diff")
	}
	h.gh.OpenedDiff(ref31, h.gh.Merge(ref31).Preview.Head)
	// pocketd's verdict is at headX: GitHub's label names the pull request,
	// not the head the gates read (Codex review of #24, round 1).
	if ok, why := h.gh.CanMerge(pull31); ok || !strings.Contains(why, "no ready cross-review at 222222222222") {
		t.Fatalf("a verdict of another head: %v %q", ok, why)
	}
	done = false
	h.gh.ApproveAndMerge(at(headY), func(github.Result) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if last := h.api.intents[len(h.api.intents)-1]; last.AckHead != headY || last.Preview.Head != headY {
		t.Fatalf("the risk:high intent %+v", last)
	}
}

// A read GitHub refuses as unauthorized marks the token spent.
func TestA401SpendsTheToken(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.api.err = &github.APIError{Status: 401, Message: "Bad credentials"}
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if m := h.gh.Merge(ref31); m.Err == "" {
		t.Fatalf("merge %+v", m)
	}
	if len(h.auth.invalid) != 1 || h.auth.invalid[0] != 7 {
		t.Fatalf("invalidated %v", h.auth.invalid)
	}
}

// Signing out forgets the sign-in and every check.
func TestSigningOutForgetsTheChecks(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	done := false
	h.gh.SignOut(func(error) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if p, _, _ := h.gh.SignIn(); p != SignedOut || h.gh.Merge(ref31).Phase != MergeNone || h.auth.forgot != 1 {
		t.Fatalf("phase %v, merge %+v, forgot %d", p, h.gh.Merge(ref31), h.auth.forgot)
	}
}

// A GitHub call names a pull request by its repository and number, and
// the diff by two commit ids GitHub gave.
func TestMergeRefAndDiffURL(t *testing.T) {
	p := proto.PullRequest{Repository: "frostyard/nsl", Number: 31, URL: "https://evil.example/x"}
	if ref, ok := MergeRef(p); !ok || ref != ref31 {
		t.Fatalf("MergeRef %+v %v", ref, ok)
	}
	for _, repo := range []string{"frostyard", "frostyard/nsl/x", "../nsl", "frostyard/.."} {
		if ref, ok := MergeRef(proto.PullRequest{Repository: repo, Number: 31}); ok {
			t.Errorf("repository %q is %+v", repo, ref)
		}
	}
	if u, ok := DiffURL(github.Preview{Ref: ref31, BaseOid: baseX, Head: headX}); !ok || u != "https://github.com/frostyard/nsl/compare/"+baseX+"..."+headX {
		t.Fatalf("DiffURL %q %v", u, ok)
	}
	if _, ok := DiffURL(github.Preview{Ref: ref31, BaseOid: "main", Head: headX}); ok {
		t.Fatal("a base that is no commit id makes a link")
	}
}

// Approve and merge are off until Open ended: the lock held and the
// keyring read (Codex review of #24, round 1).
func TestNothingRunsBeforeTheSignInOpened(t *testing.T) {
	loop := frostuitest.NewLoop()
	auth := &fakeAuth{openWait: make(chan struct{})}
	g := NewGitHub(GitHubOptions{Post: loop.Post, Auth: auth, NewClient: func(string) GitHubAPI { return &fakeAPI{login: github.Login} }})
	opened := false
	g.Open(func() { opened = true })
	if g.On() {
		t.Fatal("on before Open ended")
	}
	g.BeginSignIn()
	if p, _, _ := g.SignIn(); p != SignedOut {
		t.Fatalf("a sign-in began before Open ended: %v", p)
	}
	close(auth.openWait)
	loop.RunUntil(t, func() bool { return opened })
	if !g.On() {
		t.Fatal("off after Open ended well")
	}
}

// Open Diff counts for the head it showed: a check that read another head
// meanwhile makes it count for nothing.
func TestOpenDiffCountsForItsHead(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.api.preview.RiskHigh = true
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.api.preview.Head = headY
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.gh.OpenedDiff(ref31, headX) // the diff of the head read before
	if ok, why := h.gh.CanMerge(at(headY)); ok || !strings.Contains(why, "risk:high") {
		t.Fatalf("an old head's diff counts: %v %q", ok, why)
	}
}

// A sign-out whose keyring deletion failed is tried again with DeleteAgain.
func TestASignOutThatFailedIsTriedAgain(t *testing.T) {
	h := newGHHarness(t)
	h.auth.deleteFailed = true
	if !h.gh.CanSignOut() {
		t.Fatal("no Sign Out for a deletion that failed")
	}
	done := false
	h.gh.SignOut(func(error) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if h.auth.deletedAgain != 1 || h.auth.forgot != 0 || h.gh.CanSignOut() {
		t.Fatalf("deleted again %d, forgot %d, can sign out %v", h.auth.deletedAgain, h.auth.forgot, h.gh.CanSignOut())
	}
}

// A sign-in cancelled while a check runs leaves the check to finish: the
// credential did not change.
func TestACancelledSignInStrandsNoCheck(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.api.previewWait = make(chan struct{})
	h.gh.Check(ref31)
	h.auth.pollWait = make(chan struct{})
	h.gh.BeginSignIn()
	h.loop.RunUntil(t, func() bool { p, _, _ := h.gh.SignIn(); return p == SignInCode })
	h.gh.CancelSignIn()
	close(h.api.previewWait)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	close(h.auth.pollWait)
}

// No sign-in starts while a sign-out deletes the keyring entry.
func TestNoSignInDuringASignOut(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.auth.forgetWait = make(chan struct{})
	done := false
	h.gh.SignOut(func(error) { done = true })
	h.gh.BeginSignIn()
	if p, _, _ := h.gh.SignIn(); p != SigningOut {
		t.Fatalf("a sign-in began during the sign-out: %v", p)
	}
	close(h.auth.forgetWait)
	h.loop.RunUntil(t, func() bool { return done })
	if p, _, _ := h.gh.SignIn(); p != SignedOut {
		t.Fatalf("after the sign-out: %v", p)
	}
}

// A sign-in that ended under a lease (its refresh expired or revoked)
// signs the window out, and its checks go.
func TestAnEndedSignInSignsOut(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.auth.mu.Lock()
	h.auth.signedIn, h.auth.leaseErr = false, github.ErrNotSignedIn
	h.auth.mu.Unlock()
	h.gh.Check(ref31)
	h.loop.RunUntil(t, func() bool { p, _, _ := h.gh.SignIn(); return p == SignedOut })
	if m := h.gh.Merge(ref31); m.Phase != MergeNone {
		t.Fatalf("a check of the ended sign-in stays: %+v", m)
	}
}

// The sign-in names an owner where the App cannot act.
func TestTheSignInNamesAnOwnerTheAppCannotActOn(t *testing.T) {
	h := newGHHarness(t)
	h.api.owners = map[string]string{github.Login: "", "frostyard": "the App is not installed here"}
	h.signIn(t)
	if got := h.gh.OwnerProblem("frostyard"); got != "the App is not installed here" || h.gh.OwnerProblem(github.Login) != "" {
		t.Fatalf("owner problems %q", got)
	}
}
