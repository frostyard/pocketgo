package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
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
	unsaved                bool
	savedAgain             int
	adoptErr               error
	leaseGen               int
	// begun counts sign-outs begun, before their finish runs; beginErr
	// refuses one, as a closing TokenSource does; reason is why a kept
	// entry is not used.
	begun    int
	beginErr error
	reason   string
	// ui is the goroutine the test drives the UI from; begunOff counts
	// sign-outs begun on any other.
	ui       uint64
	begunOff int
	// saveBegun counts saves begun, before their finish runs.
	saveBegun int
}

// goid is the id of the calling goroutine.
func goid() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	id, _ := strconv.ParseUint(string(buf[:bytes.IndexByte(buf, ' ')]), 10, 64)
	return id
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
	return github.DeviceCode{DeviceCode: "dev", UserCode: "ABCD-1234", Expires: farFuture, Interval: time.Second}, nil
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
	if f.adoptErr != nil {
		return f.adoptErr
	}
	f.adopted, f.signedIn = append(f.adopted, tok), true
	return nil
}
func (f *fakeAuth) Invalidate(gen int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalid = append(f.invalid, gen)
}
func (f *fakeAuth) BeginForget() (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	f.begun++
	if goid() != f.ui {
		f.begunOff++
	}
	f.signedIn, f.reason = false, ""
	return func() error {
		f.mu.Lock()
		w := f.forgetWait
		f.mu.Unlock()
		wait(w)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleteFailed = false
		f.forgot++
		return nil
	}, nil
}
func (f *fakeAuth) BeginSaveAgain() (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	f.saveBegun++
	if goid() != f.ui {
		f.begunOff++
	}
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.savedAgain++
		f.unsaved = false
		return nil
	}, nil
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
	return "gho_token", 7 + f.leaseGen, func() {}, nil
}
func (f *fakeAuth) Status() github.AppStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.AppStatus{SignedIn: f.signedIn, Login: github.Login, DeleteFailed: f.deleteFailed, Unsaved: f.unsaved, Reason: f.reason}
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
	deadlines   []bool
	checks      int
	checkErr    error
	// checkWait holds the nth installation read (1-based) until closed;
	// ownersAt answers it with these owners instead of owners.
	checkWait map[int]chan struct{}
	ownersAt  map[int]map[string]string
	// actWait holds Act until closed.
	actWait chan struct{}
	// notes are CheckApp's warnings per owner.
	notes map[string]string
	// crs are the cross-reviews each preview was asked to count, by
	// reference; previews and results answer a reference with their own
	// preview and result instead of preview and result.
	crs      map[string][]github.CrossReview
	previews map[string]github.Preview
	results  map[string]github.Result
	// needsVerdict refuses a preview asked to count no ready cross-review,
	// as a bot's pull request under the org ruleset is.
	needsVerdict bool
	// bases, when set, are GitHub's base branches by baseKey: a preview
	// reads its base commit there as its read begins, a merge moves the
	// branch to movedBase(n) for the nth move, and Act refuses a risk:high
	// intent whose base moved since its check, as CheckIntent does
	// (ADR-0038). onMove runs under mu after each move.
	bases  map[string]string
	moves  int
	onMove func(key string)
	// holds hold the previews of a reference until closed, in place of
	// previewWait.
	holds map[string]chan struct{}
}

// previewOf is what a preview of ref reads, under mu.
func (f *fakeAPI) previewOf(ref github.Ref) github.Preview {
	if own, ok := f.previews[ref.String()]; ok {
		return own
	}
	return f.preview
}

// baseKey names ref's branch base of its repository.
func baseKey(ref github.Ref, base string) string { return ref.Owner + "/" + ref.Name + ":" + base }

// movedBase is the commit the nth merge moved a base to.
func movedBase(n int) string { return fmt.Sprintf("%040x", n) }

func (f *fakeAPI) CheckApp(_ context.Context, asked []string) (github.AppCheck, error) {
	f.mu.Lock()
	f.checks++
	n, w := f.checks, f.checkWait[f.checks]
	f.mu.Unlock()
	wait(w)
	f.mu.Lock()
	defer f.mu.Unlock()
	owners := f.owners
	if o, ok := f.ownersAt[n]; ok {
		owners = o
	}
	if f.checkErr != nil {
		err := f.checkErr
		f.checkErr = nil
		return github.AppCheck{}, err
	}
	out := map[string]string{}
	for _, o := range asked {
		why, ok := owners[o]
		switch {
		case ok:
			out[o] = why
		case owners == nil && (o == github.Login || o == "frostyard"):
			out[o] = ""
		default:
			out[o] = "the App is not installed on " + o
		}
	}
	return github.AppCheck{Login: f.login, Owners: out, Notes: f.notes}, nil
}
func (f *fakeAPI) Preview(_ context.Context, ref github.Ref, cr github.CrossReview) (github.Preview, error) {
	f.mu.Lock()
	w := f.previewWait
	if hold, ok := f.holds[ref.String()]; ok {
		w = hold
	}
	if f.crs == nil {
		f.crs = map[string][]github.CrossReview{}
	}
	f.crs[ref.String()] = append(f.crs[ref.String()], cr)
	// The base as the read began: one that moves while it waits is read
	// as it was.
	base, onBase := f.bases[baseKey(ref, f.previewOf(ref).BaseRef)]
	f.mu.Unlock()
	wait(w)
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.previewOf(ref)
	if onBase {
		p.BaseOid = base
	}
	if f.needsVerdict && cr.Verdict != github.VerdictReady {
		p.Eligible, p.Reason = false, "the rules need 2 approvals at the head; PocketGo counts 1: no ready cross-review by pocketd at 1111111"
	}
	p.Ref = ref
	return p, f.err
}
func (f *fakeAPI) Act(ctx context.Context, in github.Intent, op string, _ func(string) error) github.Result {
	f.mu.Lock()
	w := f.actWait
	f.mu.Unlock()
	wait(w)
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := ctx.Deadline()
	f.deadlines = append(f.deadlines, ok)
	f.intents = append(f.intents, in)
	r := f.result
	if own, ok := f.results[in.Preview.Ref.String()]; ok {
		r = own
	}
	r.Op = op
	if f.bases == nil {
		return r
	}
	key := baseKey(in.Preview.Ref, in.Preview.BaseRef)
	if now := f.bases[key]; in.Preview.RiskHigh && now != in.Preview.BaseOid {
		since := "since you opened the diff"
		if in.WaiveDiff {
			since = "since it was checked"
		}
		return github.Result{Op: op, Kind: "refused", Step: "read", Reason: "the base moved " + since + " (" + now[:12] + "): check it again"}
	}
	if r.Kind == "merged" {
		f.moves++
		f.bases[key] = movedBase(f.moves)
		if f.onMove != nil {
			f.onMove(key)
		}
	}
	return r
}

// farFuture is a device code's expiry no test reaches: no wall clock
// decides a test (Codex review of #24, round 6).
var farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

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
	// logs are the lines GitHub logged, on the UI thread.
	logs []string
}

func newGHHarness(t *testing.T) *ghHarness {
	t.Helper()
	h := &ghHarness{loop: frostuitest.NewLoop(), auth: &fakeAuth{ui: goid(), tok: github.AppToken{ClientID: github.AppClientID, Access: "gho_new", Refresh: "ghr_new"}},
		api: &fakeAPI{login: github.Login, preview: github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true},
			result: github.Result{Kind: "merged", Method: "SQUASH"}}}
	h.gh = NewGitHub(GitHubOptions{Post: h.loop.Post, Auth: h.auth, NewClient: func(string) GitHubAPI { return h.api },
		Owners: []string{github.Login, "frostyard"}, Log: func(_ Level, text string) { h.logs = append(h.logs, text) }})
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
var pull31 = proto.PullRequest{ID: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX, Base: "main", Checks: proto.ChecksGreen,
	Review: &proto.PullReview{Verdict: proto.ReviewReady, Head: headX}}

// pullOf is ref as pocketd links it, as pull31 is: open, with a ready
// verdict and green CI at headX on main.
func pullOf(ref github.Ref) proto.PullRequest {
	p := pull31
	p.Repository, p.Number = ref.Owner+"/"+ref.Name, int64(ref.Number)
	return p
}

// fresh is p as Store.ReadPull gives it when pocketd's answer holds p.
func fresh(p proto.PullRequest) FreshPull {
	return FreshPull{merged: p, direct: p, listed: p, read: true}
}

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
	g.Check(pull31)
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
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); !ok {
		t.Fatalf("an eligible check: %q", why)
	}
	done := false
	h.gh.ApproveAndMerge(fresh(pull31), h.gh.Merge(ref31), func(github.Result) { done = true })
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
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); ok || why != "the label ready-for-human is missing" {
		t.Fatalf("refused: %v %q", ok, why)
	}

	// risk:high: the diff first, and only for the head it showed.
	h.api.preview = github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true, RiskHigh: true}
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); ok || !strings.Contains(why, "risk:high") {
		t.Fatalf("risk:high before the diff: %v %q", ok, why)
	}
	h.gh.OpenedDiff(ref31, h.gh.Merge(ref31).Preview.BaseOid, h.gh.Merge(ref31).Preview.Head)
	if ok, _ := h.gh.CanMerge(pull31); !ok {
		t.Fatal("risk:high after the diff cannot merge")
	}
	h.api.preview.Head = headY
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, _ := h.gh.CanMerge(pull31); ok {
		t.Fatal("a new head merges on the old head's diff")
	}
	h.gh.OpenedDiff(ref31, h.gh.Merge(ref31).Preview.BaseOid, h.gh.Merge(ref31).Preview.Head)
	// pocketd's verdict is at headX: GitHub's label names the pull request,
	// not the head the gates read (Codex review of #24, round 1).
	if ok, why := h.gh.CanMerge(pull31); ok || !strings.Contains(why, "no ready cross-review with green CI at 222222222222") {
		t.Fatalf("a verdict of another head: %v %q", ok, why)
	}
	done = false
	h.gh.ApproveAndMerge(fresh(at(headY)), h.gh.Merge(ref31), func(github.Result) { done = true })
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
	h.gh.Check(pull31)
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
	h.gh.Check(pull31)
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
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.api.preview.Head = headY
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.gh.OpenedDiff(ref31, baseX, headX) // the diff of the head read before
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
	// Forget deletes it, and ends a sign-in being kept meanwhile (Codex
	// review of #24, round 5).
	if h.auth.forgot != 1 || h.gh.CanSignOut() {
		t.Fatalf("forgot %d, can sign out %v", h.auth.forgot, h.gh.CanSignOut())
	}
}

// A sign-in cancelled while a check runs leaves the check to finish: the
// credential did not change.
func TestACancelledSignInStrandsNoCheck(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.api.previewWait = make(chan struct{})
	h.gh.Check(pull31)
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
	h.gh.Check(pull31)
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

// pocketd backs a merge only with green CI at the head: GitHub's gates
// may pass a base with no required check (Codex review of #24, round 2).
func TestAMergeNeedsGreenCIFromPocketd(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	for name, checks := range map[string]proto.CheckState{"none": proto.ChecksNone, "pending": proto.ChecksPending, "not read": ""} {
		p := pull31
		p.Checks = checks
		if ok, why := h.gh.CanMerge(p); ok || !strings.Contains(why, "green CI") {
			t.Errorf("CI %s: %v %q", name, ok, why)
		}
	}
	if ok, why := h.gh.CanMerge(pull31); !ok {
		t.Fatalf("green CI: %q", why)
	}
	done := false
	h.gh.ApproveAndMerge(fresh(pull31), h.gh.Merge(ref31), func(github.Result) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	// The whole attempt has a deadline.
	if len(h.api.deadlines) != 1 || !h.api.deadlines[0] {
		t.Fatalf("Act's deadlines %v", h.api.deadlines)
	}
}

// Open Diff holds the comparison it showed: a pull request retargeted to
// another base at the same head needs its diff opened again.
func TestARetargetedPullRequestNeedsItsDiffAgain(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.api.preview.RiskHigh = true
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.gh.OpenedDiff(ref31, baseX, headX)
	if ok, why := h.gh.CanMerge(pull31); !ok {
		t.Fatalf("after the diff: %q", why)
	}
	h.api.preview.BaseOid = headY // another base, the same head
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); ok || !strings.Contains(why, "risk:high") {
		t.Fatalf("a retargeted pull request: %v %q", ok, why)
	}
	h.gh.OpenedDiff(ref31, baseX, headX) // the old comparison's callback
	if ok, _ := h.gh.CanMerge(pull31); ok {
		t.Fatal("the old comparison's diff counts for the new one")
	}
}

// A kept sign-in's App installations are read at Open, and a merge waits
// for them and for none to stand in the way.
func TestAKeptSignInChecksTheAppBeforeAMerge(t *testing.T) {
	loop := frostuitest.NewLoop()
	auth := &fakeAuth{signedIn: true}
	api := &fakeAPI{login: github.Login, owners: map[string]string{github.Login: "", "frostyard": "missing contents: write"},
		preview: github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true}}
	g := NewGitHub(GitHubOptions{Post: loop.Post, Auth: auth, NewClient: func(string) GitHubAPI { return api }, Owners: []string{github.Login, "frostyard"}})
	opened := false
	g.Open(func() { opened = true })
	loop.RunUntil(t, func() bool { return opened && g.OwnerProblem("frostyard") != "" })
	g.Check(pull31)
	loop.RunUntil(t, func() bool { return g.Merge(ref31).Phase == MergeChecked })
	if ok, why := g.CanMerge(pull31); ok || !strings.Contains(why, "missing contents: write") {
		t.Fatalf("an owner the App cannot act on: %v %q", ok, why)
	}
}

// A sign-in attempt whose adoption turned stale goes back to a phase it
// can begin again from.
func TestAStaleAdoptionCanBeginAgain(t *testing.T) {
	h := newGHHarness(t)
	h.auth.adoptErr = github.ErrStale
	h.gh.BeginSignIn()
	h.loop.RunUntil(t, func() bool { _, _, fail := h.gh.SignIn(); return fail != "" })
	if p, _, _ := h.gh.SignIn(); p != SignedOut {
		t.Fatalf("after a stale adoption: %v", p)
	}
	h.auth.adoptErr = nil
	h.signIn(t)
}

// A refreshed pair the keyring did not keep is saved again on the owner's
// word.
func TestAnUnsavedPairIsSavedAgain(t *testing.T) {
	h := newGHHarness(t)
	h.auth.unsaved = true
	if !h.gh.CanSaveAgain() {
		t.Fatal("no Save Again for an unsaved pair")
	}
	done := false
	h.gh.SaveAgain(func(error) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if h.auth.savedAgain != 1 || h.gh.CanSaveAgain() {
		t.Fatalf("saved again %d, can save again %v", h.auth.savedAgain, h.gh.CanSaveAgain())
	}
}

// An owner the sign-in never read is no permission: Check reads it, and a
// merge waits for it (Codex review of #24, round 3).
func TestAThirdOwnerIsReadBeforeAMerge(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	other := github.Ref{Owner: "otherorg", Name: "tool", Number: 4}
	p := pull31
	p.Repository, p.Number = "otherorg/tool", 4
	h.gh.Check(pullOf(other))
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(other).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(p); ok || !strings.Contains(why, "not installed on otherorg") {
		t.Fatalf("an owner the App is not installed on: %v %q", ok, why)
	}
}

// An installation read that failed at Open is read again by Check, with
// the kept credential.
func TestCheckReadsTheInstallationsAgain(t *testing.T) {
	loop := frostuitest.NewLoop()
	auth := &fakeAuth{signedIn: true}
	api := &fakeAPI{login: github.Login, checkErr: errors.New("timeout"), preview: github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true}}
	g := NewGitHub(GitHubOptions{Post: loop.Post, Auth: auth, NewClient: func(string) GitHubAPI { return api }, Owners: []string{github.Login, "frostyard"}})
	opened := false
	g.Open(func() { opened = true })
	loop.RunUntil(t, func() bool { _, _, fail := g.SignIn(); return opened && fail != "" })
	g.Check(pull31)
	loop.RunUntil(t, func() bool { return g.Merge(ref31).Phase == MergeChecked })
	if ok, why := g.CanMerge(pull31); !ok {
		t.Fatalf("after Check read the installations: %q", why)
	}
}

// A kept sign-in that acts as someone else now signs the window out.
func TestAKeptSignInForAnotherLoginSignsOut(t *testing.T) {
	loop := frostuitest.NewLoop()
	auth := &fakeAuth{signedIn: true}
	api := &fakeAPI{login: "bketelsen-renamed"}
	g := NewGitHub(GitHubOptions{Post: loop.Post, Auth: auth, NewClient: func(string) GitHubAPI { return api }, Owners: []string{github.Login}})
	opened := false
	g.Open(func() { opened = true })
	loop.RunUntil(t, func() bool { _, _, fail := g.SignIn(); return opened && fail != "" })
	loop.RunUntil(t, func() bool { p, _, _ := g.SignIn(); return p == SignedOut })
	if p, _, fail := g.SignIn(); p != SignedOut || !strings.Contains(fail, "bketelsen-renamed") {
		t.Fatalf("phase %v, %q", p, fail)
	}
	// The pair is gone from the keyring too: a cancelled sign-in does not
	// bring it back (Codex review of #24, round 4).
	if auth.forgot != 1 {
		t.Fatalf("forgot %d times", auth.forgot)
	}
	auth.pollWait = make(chan struct{})
	g.BeginSignIn()
	loop.RunUntil(t, func() bool { p, _, _ := g.SignIn(); return p == SignInCode })
	g.CancelSignIn()
	if p, _, _ := g.SignIn(); p != SignedOut {
		t.Fatalf("after a cancel: %v", p)
	}
	close(auth.pollWait)
}

// An owner the App could not act on is read again by Check: the owner may
// have installed it since (Codex review of #24, round 4).
func TestARefusedOwnerIsReadAgain(t *testing.T) {
	h := newGHHarness(t)
	h.api.owners = map[string]string{github.Login: "", "frostyard": "missing contents: write"}
	h.signIn(t)
	h.api.mu.Lock()
	h.api.owners = map[string]string{github.Login: "", "frostyard": ""}
	h.api.mu.Unlock()
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if ok, why := h.gh.CanMerge(pull31); !ok {
		t.Fatalf("after the App was granted what it lacked: %q", why)
	}
}

// A merge acts on the preview the owner confirmed: one read again since
// refuses, and so does a sign-in other than the check's (Codex review of
// #24, round 5).
func TestAMergeActsOnlyOnWhatWasConfirmed(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	confirmed := h.gh.Merge(ref31)
	h.api.preview.Method = "MERGE" // the method changed on GitHub
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	var res github.Result
	h.gh.ApproveAndMerge(fresh(pull31), confirmed, func(r github.Result) { res = r })
	if res.Kind != "refused" || len(h.api.intents) != 0 {
		t.Fatalf("a preview read again since the confirmation: %+v, intents %+v", res, h.api.intents)
	}

	// The check read with token generation 7; a sign-in adopted since leases 8.
	h.auth.mu.Lock()
	h.auth.leaseGen = 1
	h.auth.mu.Unlock()
	done := false
	h.gh.ApproveAndMerge(fresh(pull31), h.gh.Merge(ref31), func(r github.Result) { res, done = r, true })
	h.loop.RunUntil(t, func() bool { return done })
	if res.Kind != "refused" || !strings.Contains(res.Reason, "sign-in changed") || len(h.api.intents) != 0 {
		t.Fatalf("another sign-in's token: %+v, intents %+v", res, h.api.intents)
	}
}

// The confirmation's read of pocketd goes through the pulls fetch: an
// event newer than the read wins over its copy (Codex review of #24,
// round 5).
func TestReadPullHonorsANewerEvent(t *testing.T) {
	h := newHarness(t)
	ready := proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX,
		Checks: proto.ChecksGreen, Review: &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}}
	h.srv.AddPull(ready)
	h.connect(t)
	// The held answer is the list as it was before the event below.
	release := h.srv.HoldAnswerOnce("GET", proto.PathPulls)
	var got proto.PullRequest
	done := false
	h.store.ReadPull(1, func(f FreshPull, found bool, err error) { got, done = f.Pull(), true })
	notReady := ready
	notReady.Review = &proto.PullReview{ID: 2, Verdict: proto.ReviewNotReady, Head: headX}
	// The server's list keeps the older copy, whenever the held answer
	// was computed: the event alone is newer.
	h.srv.EmitOnly(proto.Event{Type: proto.EventPull, Pull: &notReady})
	h.loop.RunUntil(t, func() bool {
		for _, p := range h.store.Data.Pulls() {
			if p.ID == 1 && p.Review != nil && p.Review.Verdict == proto.ReviewNotReady {
				return true
			}
		}
		return false
	})
	release()
	h.loop.RunUntil(t, func() bool { return done })
	if got.Review == nil || got.Review.Verdict != proto.ReviewNotReady {
		t.Fatalf("the read gave %+v: the newer event lost", got.Review)
	}
}

// A fresh read that a newer, failed fetch superseded proves nothing: the
// list is the cached one, and the confirmation fails (Codex review of #24,
// round 6).
func TestReadPullFailsWhenANewerFetchFailed(t *testing.T) {
	h := newHarness(t)
	h.srv.AddPull(proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX,
		Checks: proto.ChecksGreen, Review: &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}})
	h.connect(t)
	before := len(h.srv.RequestsTo("GET", proto.PathPulls))
	release := h.srv.HoldAnswerOnce("GET", proto.PathPulls)
	var gotErr error
	done := false
	h.store.ReadPull(1, func(_ FreshPull, _ bool, err error) { gotErr, done = err, true })
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", proto.PathPulls)) == before+1 })
	h.srv.FailOnce("GET", proto.PathPulls, 502)
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { st, _ := h.store.Data.PullsStatus(); return st == ListFailed })
	release()
	h.loop.RunUntil(t, func() bool { return done })
	if gotErr == nil {
		t.Fatal("a read a failed fetch superseded answered from the cache")
	}
	h.settle(t)
}

// Sign Out begins on the UI thread: the keyring's delete counts as work
// before the call returns, so a Close of the window that comes next waits
// for it, and a sign-out a closing window refused says so (Codex review of
// #24, round 7).
func TestSignOutBeginsOnTheUIThread(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.auth.mu.Lock()
	h.auth.forgetWait = make(chan struct{})
	h.auth.mu.Unlock()
	done := false
	h.gh.SignOut(func(error) { done = true })
	h.auth.mu.Lock()
	begun, off, forgot := h.auth.begun, h.auth.begunOff, h.auth.forgot
	h.auth.mu.Unlock()
	if begun != 1 || off != 0 || forgot != 0 {
		t.Fatalf("when SignOut returned: begun %d (%d off the UI thread), forgot %d", begun, off, forgot)
	}
	close(h.auth.forgetWait)
	h.loop.RunUntil(t, func() bool { return done })

	h.signIn(t)
	h.auth.mu.Lock()
	h.auth.beginErr = github.ErrClosing
	h.auth.mu.Unlock()
	var got error
	h.gh.SignOut(func(err error) { got = err })
	if !errors.Is(got, github.ErrClosing) {
		t.Fatalf("a refused sign-out gave %v", got)
	}
	if p, _, _ := h.gh.SignIn(); p != SignedIn {
		t.Fatalf("a refused sign-out left phase %v", p)
	}
}

// A confirmation holds for the check it confirmed: a check read since,
// even one GitHub answered the same, needs its own (Codex review of #24,
// round 7).
func TestAConfirmationHoldsForItsCheckOnly(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	confirmed := h.gh.Merge(ref31)
	h.gh.Check(pull31) // the same answer
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	var res github.Result
	h.gh.ApproveAndMerge(fresh(pull31), confirmed, func(r github.Result) { res = r })
	if res.Kind != "refused" || len(h.api.intents) != 0 {
		t.Fatalf("an earlier check's confirmation: %+v, intents %+v", res, h.api.intents)
	}
	done := false
	h.gh.ApproveAndMerge(fresh(pull31), h.gh.Merge(ref31), func(r github.Result) { res, done = r, true })
	h.loop.RunUntil(t, func() bool { return done })
	if res.Kind != "merged" {
		t.Fatalf("the current check's confirmation: %+v", res)
	}
}

// An installation read that answers after a newer one of the same owner
// never undoes it: the kept sign-in's read at Open, held, said the App
// lacked a permission; Check read the owner since and found it granted
// (Codex review of #24, round 7).
func TestAnOlderInstallationReadNeverUndoesANewerOne(t *testing.T) {
	loop := frostuitest.NewLoop()
	auth := &fakeAuth{ui: goid(), signedIn: true}
	held := make(chan struct{})
	api := &fakeAPI{login: github.Login, preview: github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true},
		result:    github.Result{Kind: "merged", Method: "SQUASH"},
		checkWait: map[int]chan struct{}{1: held},
		ownersAt: map[int]map[string]string{
			1: {github.Login: "", "frostyard": "missing contents: write"},
			2: {github.Login: "", "frostyard": ""},
		}}
	g := NewGitHub(GitHubOptions{Post: loop.Post, Auth: auth, NewClient: func(string) GitHubAPI { return api },
		Owners: []string{github.Login, "frostyard"}})
	opened := false
	g.Open(func() { opened = true })
	loop.RunUntil(t, func() bool { return opened })
	// The read at Open reached GitHub, and is held there.
	loop.RunUntil(t, func() bool { api.mu.Lock(); defer api.mu.Unlock(); return api.checks == 1 })
	g.Check(pull31)
	loop.RunUntil(t, func() bool { return g.Merge(ref31).Phase == MergeChecked })
	if ok, why := g.CanMerge(pull31); !ok {
		t.Fatalf("after Check read the owner granted: %q", why)
	}
	ran := loop.Ran()
	close(held)
	loop.RunUntil(t, func() bool { return loop.Ran() > ran }) // the held read's answer
	if ok, why := g.CanMerge(pull31); !ok {
		t.Fatalf("the older read undid the newer one: %q", why)
	}
}

// A kept entry the sign-in rejected can be signed out, and says why it is
// not used (Codex review of #24, round 7).
func TestARejectedKeptSignInCanBeSignedOut(t *testing.T) {
	h := newGHHarness(t)
	h.auth.mu.Lock()
	h.auth.reason = "the GitHub sign-in in the keyring is not one: a field is empty"
	h.auth.mu.Unlock()
	if !h.gh.CanSignOut() || !strings.Contains(h.gh.KeptProblem(), "a field is empty") {
		t.Fatalf("CanSignOut %v, KeptProblem %q", h.gh.CanSignOut(), h.gh.KeptProblem())
	}
	done := false
	h.gh.SignOut(func(error) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if h.auth.forgot != 1 || h.gh.KeptProblem() != "" {
		t.Fatalf("forgot %d, KeptProblem %q", h.auth.forgot, h.gh.KeptProblem())
	}
}

// A pull request pocketd's answer does not hold is not found, though an
// event that came after the read began put it in the list (Codex review of
// #24, round 7).
func TestReadPullFindsOnlyWhatPocketdAnswered(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	// pocketd's answer is its read of GitHub now (Codex review of #26,
	// rounds 8 and 11).
	path := proto.Path(proto.PathPull, 1)
	release := h.srv.HoldAnswerOnce("GET", path)
	found, done := true, false
	h.store.ReadPull(1, func(_ FreshPull, f bool, _ error) { found, done = f, true })
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", path)) == 1 })
	ready := proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX,
		Checks: proto.ChecksGreen, Review: &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}}
	h.srv.EmitOnly(proto.Event{Type: proto.EventPull, Pull: &ready})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Pulls()) == 1 })
	release()
	h.loop.RunUntil(t, func() bool { return done })
	if found {
		t.Fatal("a pull request pocketd's answer did not hold was found")
	}
}

// Save Again begins on the UI thread: a Close of the window that comes
// next waits for the write (Codex review of #24, round 8).
func TestSaveAgainBeginsOnTheUIThread(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.auth.mu.Lock()
	h.auth.unsaved = true
	h.auth.mu.Unlock()
	done := false
	h.gh.SaveAgain(func(error) { done = true })
	h.auth.mu.Lock()
	begun, off := h.auth.saveBegun, h.auth.begunOff
	h.auth.mu.Unlock()
	if begun != 1 || off != 0 {
		t.Fatalf("when SaveAgain returned: begun %d (%d off the UI thread)", begun, off)
	}
	h.loop.RunUntil(t, func() bool { return done })
}

// The merge itself refuses without pocketd's answer at the confirmation:
// a row of the desktop's own, or an answer whose own row does not back the
// head, sends nothing (Codex review of #24, round 8).
func TestApproveAndMergeNeedsPocketdsAnswer(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	notReady := pull31
	notReady.Review = &proto.PullReview{Verdict: proto.ReviewNotReady, Head: headX}
	for name, f := range map[string]FreshPull{
		"no read":                 {merged: pull31, direct: pull31, listed: pull31},
		"pocketd's row not ready": {merged: pull31, direct: notReady, listed: pull31, read: true},
		"another row":             {merged: pull31, direct: proto.PullRequest{ID: 2}, listed: pull31, read: true},
		// The list after the read refuses, though an event won the merge
		// (Codex review of #26, round 9).
		"the list's row not ready": {merged: pull31, direct: pull31, listed: notReady, read: true},
		"no list row":              {merged: pull31, direct: pull31, read: true},
	} {
		var res github.Result
		h.gh.ApproveAndMerge(f, h.gh.Merge(ref31), func(r github.Result) { res = r })
		if res.Kind != "refused" || len(h.api.intents) != 0 {
			t.Fatalf("%s: %+v, intents %+v", name, res, h.api.intents)
		}
	}
}

// An answer a newer fetch superseded proves nothing, though the list
// loaded: the newer answer refused what this one backs, and a replayed
// event put the backed row back (Codex review of #24, round 8).
func TestReadPullRefusesASupersededAnswer(t *testing.T) {
	h := newHarness(t)
	ready := proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX,
		Checks: proto.ChecksGreen, Review: &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}}
	h.srv.AddPull(ready)
	h.connect(t)
	verdict := func(want proto.ReviewVerdict) func() bool {
		return func() bool {
			for _, p := range h.store.Data.Pulls() {
				if p.ID == 1 && p.Review != nil && p.Review.Verdict == want {
					return true
				}
			}
			return false
		}
	}
	before := len(h.srv.RequestsTo("GET", proto.PathPulls))
	release := h.srv.HoldAnswerOnce("GET", proto.PathPulls)
	var gotErr error
	found, done := false, false
	h.store.ReadPull(1, func(_ FreshPull, f bool, err error) { found, gotErr, done = f, err, true })
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", proto.PathPulls)) == before+1 })
	// A newer fetch answers first, with the verdict pocketd has now.
	notReady := ready
	notReady.Review = &proto.PullReview{ID: 2, Verdict: proto.ReviewNotReady, Head: headX}
	h.srv.AddPull(notReady)
	h.store.Refresh()
	h.loop.RunUntil(t, verdict(proto.ReviewNotReady))
	// A replay of the older event puts the ready row back.
	h.srv.EmitOnly(proto.Event{Type: proto.EventPull, Pull: &ready})
	h.loop.RunUntil(t, verdict(proto.ReviewReady))
	release()
	h.loop.RunUntil(t, func() bool { return done })
	if gotErr == nil || found {
		t.Fatalf("a superseded answer was taken: found %v, err %v", found, gotErr)
	}
	h.settle(t)
}

// A sign-out waits for a running approve and merge: its client holds the
// token, which the sign-out cannot take back (Codex review of #24, round 9).
func TestSignOutWaitsForARunningMerge(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.api.mu.Lock()
	h.api.actWait = make(chan struct{})
	h.api.mu.Unlock()
	merged := false
	h.gh.ApproveAndMerge(fresh(pull31), h.gh.Merge(ref31), func(github.Result) { merged = true })
	if !h.gh.Merging() || h.gh.CanSignOut() {
		t.Fatalf("while merging: Merging %v, CanSignOut %v", h.gh.Merging(), h.gh.CanSignOut())
	}
	var got error
	h.gh.SignOut(func(err error) { got = err })
	h.auth.mu.Lock()
	begun := h.auth.begun
	h.auth.mu.Unlock()
	if got == nil || begun != 0 {
		t.Fatalf("a sign-out during a merge: err %v, begun %d", got, begun)
	}
	close(h.api.actWait)
	h.loop.RunUntil(t, func() bool { return merged })
	if !h.gh.CanSignOut() {
		t.Fatal("no sign-out once the merge ended")
	}
}

// holdMerge starts an approve and merge of ref31 whose Act waits until the
// returned release; merged turns true once it ended.
func (h *ghHarness) holdMerge(t *testing.T) (release func(), merged *bool) {
	t.Helper()
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.api.mu.Lock()
	h.api.actWait = make(chan struct{})
	h.api.mu.Unlock()
	merged = new(bool)
	h.gh.ApproveAndMerge(fresh(pull31), h.gh.Merge(ref31), func(github.Result) { *merged = true })
	return func() { close(h.api.actWait) }, merged
}

// A merge stays counted across a new sign-in: its worker holds the old
// lease, so the sign-out still waits for it (Codex review of #24, round 10).
func TestARunningMergeOutlivesANewSignIn(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	release, merged := h.holdMerge(t)
	h.signIn(t) // a new credential: the checks and merges are forgotten
	if !h.gh.Merging() || h.gh.CanSignOut() {
		t.Fatalf("after a new sign-in: Merging %v, CanSignOut %v", h.gh.Merging(), h.gh.CanSignOut())
	}
	release()
	h.loop.RunUntil(t, func() bool { return *merged })
	if h.gh.Merging() || !h.gh.CanSignOut() {
		t.Fatalf("after the merge: Merging %v, CanSignOut %v", h.gh.Merging(), h.gh.CanSignOut())
	}
}

// A credential found to act as another login while a merge runs is out of
// use at once, and goes from the keyring once the merge ended (Codex
// review of #24, round 10).
func TestAWrongLoginFoundDuringAMergeIsForgottenAfter(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	release, merged := h.holdMerge(t)
	h.api.mu.Lock()
	h.api.login = "someone-else"
	h.api.mu.Unlock()
	other := github.Ref{Owner: "someone", Name: "x", Number: 1}
	h.gh.Check(pullOf(other)) // reads the App on a new owner, as the wrong login
	h.loop.RunUntil(t, func() bool { _, _, fail := h.gh.SignIn(); return fail != "" })
	h.auth.mu.Lock()
	begun := h.auth.begun
	h.auth.mu.Unlock()
	if p, _, _ := h.gh.SignIn(); p == SignedIn || begun != 0 {
		t.Fatalf("while the merge runs: phase %v, sign-outs begun %d", p, begun)
	}
	ref32 := github.Ref{Owner: "frostyard", Name: "nsl", Number: 32}
	h.gh.Check(pullOf(ref32))
	if h.gh.Merge(ref32).Phase != MergeNone {
		t.Fatal("the rejected credential still checks")
	}
	release()
	h.loop.RunUntil(t, func() bool { return *merged })
	h.loop.RunUntil(t, func() bool { h.auth.mu.Lock(); defer h.auth.mu.Unlock(); return h.auth.forgot == 1 })
}

// A queue-backed check shows CheckApp's warning for its owner; it blocks
// nothing (Codex review of #24, round 10).
func TestAQueueCheckShowsTheAppsNote(t *testing.T) {
	h := newGHHarness(t)
	h.api.notes = map[string]string{"frostyard": "an enqueue may fail: the App lacks merge_queues: write on frostyard"}
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if note := h.gh.MergeNote(ref31); note != "" {
		t.Fatalf("a direct merge shows %q", note)
	}
	h.api.mu.Lock()
	h.api.preview.Queue = true
	h.api.mu.Unlock()
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	if note := h.gh.MergeNote(ref31); !strings.Contains(note, "merge_queues: write") {
		t.Fatalf("a queue-backed check shows %q", note)
	}
	if ok, why := h.gh.CanMerge(pull31); !ok {
		t.Fatalf("the note blocks: %q", why)
	}
}

// A pull request whose merge runs is reserved across a new sign-in: it
// shows as merging, is not checked again, and a second attempt refuses
// until the first worker returned (Codex review of #24, round 11).
func TestAMergeInFlightIsReservedAcrossSignIns(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	release, merged := h.holdMerge(t)
	h.signIn(t) // a new credential
	if got := h.gh.Merge(ref31).Phase; got != MergeActing {
		t.Fatalf("after a new sign-in the merge shows %v", got)
	}
	h.gh.Check(pull31)
	// Check starts nothing: no check of it under the new credential.
	if _, started := h.gh.merges[ref31.String()]; started || h.gh.Merge(ref31).Phase != MergeActing {
		t.Fatalf("checked again while its merge runs: %v", h.gh.Merge(ref31).Phase)
	}
	var res github.Result
	h.gh.ApproveAndMerge(fresh(pull31), Merge{}, func(r github.Result) { res = r })
	if res.Kind != "refused" || len(h.api.intents) != 0 {
		t.Fatalf("a second attempt: %+v, intents %+v", res, h.api.intents)
	}
	release()
	h.loop.RunUntil(t, func() bool { return *merged })
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
}

// A credential rejected while a merge runs stays out of use until it is
// deleted: a sign-in begun and canceled meanwhile does not bring it back
// (Codex review of #24, round 11).
func TestARejectedCredentialStaysOutAfterACanceledSignIn(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	release, merged := h.holdMerge(t)
	h.api.mu.Lock()
	h.api.login = "someone-else"
	h.api.mu.Unlock()
	h.gh.Check(pullOf(github.Ref{Owner: "someone", Name: "x", Number: 1}))
	h.loop.RunUntil(t, func() bool { _, _, fail := h.gh.SignIn(); return fail != "" })
	h.auth.mu.Lock()
	h.auth.pollWait = make(chan struct{})
	h.auth.mu.Unlock()
	h.gh.BeginSignIn()
	h.loop.RunUntil(t, func() bool { p, _, _ := h.gh.SignIn(); return p == SignInCode })
	h.gh.CancelSignIn()
	if p, _, _ := h.gh.SignIn(); p != SignedOut {
		t.Fatalf("a canceled sign-in left phase %v for the rejected credential", p)
	}
	release()
	h.loop.RunUntil(t, func() bool { return *merged })
	h.loop.RunUntil(t, func() bool { h.auth.mu.Lock(); defer h.auth.mu.Unlock(); return h.auth.forgot == 1 })
}

// A kept sign-in PocketGo signed out on its own says why, also once the
// owner began a new sign-in, until a new pair is adopted (Codex review of
// #24, round 11).
func TestASignInRemovedOnItsOwnSaysWhy(t *testing.T) {
	loop := frostuitest.NewLoop()
	auth := &fakeAuth{ui: goid(), signedIn: true, tok: github.AppToken{ClientID: github.AppClientID, Access: "gho_new", Refresh: "ghr_new"}}
	api := &fakeAPI{login: "someone-else", preview: github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true}}
	g := NewGitHub(GitHubOptions{Post: loop.Post, Auth: auth, NewClient: func(string) GitHubAPI { return api },
		Owners: []string{github.Login, "frostyard"}})
	opened := false
	g.Open(func() { opened = true })
	loop.RunUntil(t, func() bool { return opened })
	loop.RunUntil(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return auth.forgot == 1 })
	if why := g.KeptProblem(); !strings.Contains(why, "acts as someone-else") {
		t.Fatalf("after the removal: %q", why)
	}
	api.mu.Lock()
	api.login = github.Login
	api.mu.Unlock()
	auth.mu.Lock()
	auth.pollWait = make(chan struct{})
	auth.mu.Unlock()
	g.BeginSignIn()
	loop.RunUntil(t, func() bool { p, _, _ := g.SignIn(); return p == SignInCode })
	if why := g.KeptProblem(); !strings.Contains(why, "acts as someone-else") {
		t.Fatalf("once a sign-in began: %q", why)
	}
	close(auth.pollWait)
	loop.RunUntil(t, func() bool { p, _, _ := g.SignIn(); return p == SignedIn })
	if why := g.KeptProblem(); why != "" {
		t.Fatalf("after a new pair: %q", why)
	}
}

// The sign-in window does not open while signing out: no flow could
// begin (Codex review of #24, round 11).
func TestNoSignInOpensWhileSigningOut(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.auth.mu.Lock()
	h.auth.forgetWait = make(chan struct{})
	h.auth.mu.Unlock()
	done := false
	h.gh.SignOut(func(error) { done = true })
	if h.gh.CanOpenSignIn() {
		t.Fatal("the sign-in window may open while signing out")
	}
	close(h.auth.forgetWait)
	h.loop.RunUntil(t, func() bool { return done })
	if !h.gh.CanOpenSignIn() {
		t.Fatal("the sign-in window may not open once signed out")
	}
}

// Backing names the base branch: pocketd's verdict is of its head on the
// branch it read, so a merge into another branch is not backed, through
// the merge's own check of pocketd's row too (Codex review of
// pocketgo#26, round 5).
func TestBackedNeedsTheBase(t *testing.T) {
	if !Backed(pull31, headX, "main") || Backed(pull31, headX, "release") || Backed(pull31, headX, "") {
		t.Fatal("Backed does not hold to the base branch")
	}
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	retargeted := pull31
	retargeted.Base = "release"
	var res github.Result
	h.gh.ApproveAndMerge(FreshPull{merged: pull31, direct: retargeted, listed: pull31, read: true}, h.gh.Merge(ref31), func(r github.Result) { res = r })
	if res.Kind != "refused" || len(h.api.intents) != 0 {
		t.Fatalf("pocketd's row on another base: %+v, intents %+v", res, h.api.intents)
	}
}

// Only an open pull request that is no draft is backed: pocketd's verdict
// and CI of one closed are of before it closed (Codex review of #26,
// round 10).
func TestBackedNeedsAnOpenPull(t *testing.T) {
	closed, merged, draft := pull31, pull31, pull31
	closed.State, merged.State, draft.Draft = proto.PullClosed, proto.PullMerged, true
	for name, p := range map[string]proto.PullRequest{"closed": closed, "merged": merged, "draft": draft} {
		if Backed(p, headX, "main") {
			t.Errorf("a %s pull request is backed", name)
		}
	}
	if !Backed(pull31, headX, "main") {
		t.Fatal("an open one is not")
	}
}

// A confirmation needs pocketd to read the pull request on GitHub at the
// click: a base branch reset since its last round leaves no verdict, and
// a read it cannot make backs nothing (Codex review of #26, round 7).
func TestReadPullHasPocketdReadGitHubFirst(t *testing.T) {
	h := newHarness(t)
	ready := proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX, Base: "main",
		Checks: proto.ChecksGreen, Review: &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}}
	h.srv.AddPull(ready)
	h.connect(t)
	path := proto.Path(proto.PathPull, 1)
	var got error
	found, done := false, false
	h.store.ReadPull(1, func(_ FreshPull, f bool, err error) { found, got, done = f, err, true })
	h.loop.RunUntil(t, func() bool { return done })
	if !found || got != nil || len(h.srv.RequestsTo("GET", path)) != 1 {
		t.Fatalf("found %v, %v, %d reads on GitHub", found, got, len(h.srv.RequestsTo("GET", path)))
	}
	h.srv.FailOnce("GET", path, http.StatusInternalServerError)
	done = false
	h.store.ReadPull(1, func(_ FreshPull, f bool, err error) { found, got, done = f, err, true })
	h.loop.RunUntil(t, func() bool { return done })
	if found || got == nil {
		t.Fatalf("a read pocketd could not make on GitHub: found %v, %v", found, got)
	}
}

// pocketd's answer that read GitHub is the confirmation's own row: one
// that found nothing is not found, though the list read after it lists the
// pull request (Codex review of #26, round 8).
func TestReadPullTrustsOnlyTheReadOfGitHub(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	path := proto.Path(proto.PathPull, 1)
	release := h.srv.HoldAnswerOnce("GET", path)
	found, done := false, false
	var fresh FreshPull
	h.store.ReadPull(1, func(f FreshPull, ok bool, _ error) { fresh, found, done = f, ok, true })
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", path)) == 1 })
	// pocketd's read found nothing; the pull request is linked before the
	// list is read.
	h.srv.AddPull(proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX, Base: "main",
		Checks: proto.ChecksGreen, Review: &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}})
	release()
	h.loop.RunUntil(t, func() bool { return done })
	if found || fresh.read {
		t.Fatalf("a pull request pocketd's read of GitHub did not find: found %v, %+v", found, fresh)
	}
	// pocketd's read found it with no verdict; the list read after it has
	// one: the direct row is the read's.
	unreviewed := proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX, Base: "main",
		Checks: proto.ChecksGreen}
	h.srv.AddPull(unreviewed)
	release = h.srv.HoldAnswerOnce("GET", path)
	done = false
	h.store.ReadPull(1, func(f FreshPull, ok bool, _ error) { fresh, found, done = f, ok, true })
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", path)) == 2 })
	reviewed := unreviewed
	reviewed.Review = &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}
	h.srv.AddPull(reviewed)
	release()
	h.loop.RunUntil(t, func() bool { return done })
	if !found || fresh.direct.Review != nil || Backed(fresh.direct, headX, "main") {
		t.Fatalf("the direct row is not pocketd's read of GitHub: found %v, %+v", found, fresh.direct)
	}
}

// The list read after pocketd's read of GitHub counts as its own row: a
// list that no longer backs the pull request refuses the merge, though a
// replayed event's ready copy wins the merge of the rows (Codex review of
// #26, round 9).
func TestReadPullKeepsTheListsRow(t *testing.T) {
	h := newHarness(t)
	ready := proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX, Base: "main",
		Checks: proto.ChecksGreen, Review: &proto.PullReview{ID: 1, Verdict: proto.ReviewReady, Head: headX}}
	h.srv.AddThread(proto.Thread{ID: 1, State: proto.ThreadOpen})
	h.srv.AddPull(ready)
	h.connect(t)
	path := proto.Path(proto.PathPull, 1)
	release := h.srv.HoldAnswerOnce("GET", path)
	var fresh FreshPull
	found, done := false, false
	h.store.ReadPull(1, func(f FreshPull, ok bool, _ error) { fresh, found, done = f, ok, true })
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", path)) == 1 })
	// pocketd's read backed it; its verdict is gone before the list is
	// read.
	unbacked := ready
	unbacked.Review = nil
	h.srv.AddPull(unbacked)
	before := len(h.srv.RequestsTo("GET", proto.PathPulls))
	releaseList := h.srv.HoldAnswerOnce("GET", proto.PathPulls)
	release()
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", proto.PathPulls)) == before+1 })
	replayed := ready
	replayed.Title = "replayed"
	h.srv.EmitOnly(proto.Event{Type: proto.EventPull, Pull: &replayed})
	h.loop.RunUntil(t, func() bool {
		for _, p := range h.store.Data.Pulls() {
			if p.ID == 1 && p.Title == "replayed" {
				return true
			}
		}
		return false
	})
	releaseList()
	h.loop.RunUntil(t, func() bool { return done })
	if !found || fresh.merged.Review == nil || fresh.listed.Review != nil || Backed(fresh.listed, headX, "main") {
		t.Fatalf("the list's own row: found %v, %+v", found, fresh.listed)
	}
}

// A confirmation whose read of GitHub fails fails alone: the pull
// requests' list keeps its status, and a list fetch begun before it still
// lands (Codex review of #26, round 11).
func TestAFailedConfirmationLeavesTheList(t *testing.T) {
	h := newHarness(t)
	h.srv.AddThread(proto.Thread{ID: 1, State: proto.ThreadOpen})
	h.srv.AddPull(proto.PullRequest{ID: 1, Thread: 1, Repository: "frostyard/nsl", Number: 31, State: proto.PullOpen, HeadSHA: headX})
	h.connect(t)
	if st, _ := h.store.Data.PullsStatus(); st != ListLoaded {
		t.Fatalf("the list before: %v", st)
	}
	earlier := h.store.Data.BeginPulls()
	h.srv.FailOnce("GET", proto.Path(proto.PathPull, 1), http.StatusInternalServerError)
	var got error
	done := false
	h.store.ReadPull(1, func(_ FreshPull, _ bool, err error) { got, done = err, true })
	h.loop.RunUntil(t, func() bool { return done })
	if got == nil {
		t.Fatal("a failed read of GitHub confirmed")
	}
	if st, why := h.store.Data.PullsStatus(); st != ListLoaded {
		t.Fatalf("the list after a failed confirmation: %v %s", st, why)
	}
	if !h.store.Data.SetPulls(earlier, h.store.Data.Pulls()) {
		t.Fatal("a list fetch begun before the confirmation was superseded")
	}
}
