package state

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/proto"
)

// The owner's GitHub sign-in, and the approvals and merges made through it
// (ADR-0018): frostyard/pocket's gates, in desktop/internal/github, run as
// the owner through the GitHub App's user token. GitHub belongs to the UI
// thread, as Store does: each call to GitHub runs on a goroutine and posts
// its result with Options.Post.

// GitHubAuth is the owner's App sign-in: cmd/pocket's githubAuth over
// github.TokenSource and the keyring, a fake in tests.
type GitHubAuth interface {
	// Open takes the sign-in lock and loads the kept sign-in.
	Open() error
	Start(ctx context.Context) (github.DeviceCode, error)
	Poll(ctx context.Context, dc github.DeviceCode) (github.AppToken, error)
	BeginSignIn() int
	CancelSignIn(gen int)
	Adopt(tok github.AppToken, gen int) error
	// Invalidate marks the pair of tokenGen spent, after a 401.
	Invalidate(tokenGen int)
	Forget() error
	Lease(ctx context.Context) (token string, tokenGen int, release func(), err error)
	Status() github.AppStatus
}

// GitHubAPI is what the desktop calls on GitHub as the owner:
// github.Client, a fake in tests.
type GitHubAPI interface {
	CheckApp(ctx context.Context, owners []string) (github.AppCheck, error)
	Preview(ctx context.Context, ref github.Ref) (github.Preview, error)
	Act(ctx context.Context, in github.Intent, op string, step func(string) error) github.Result
}

// GitHubOptions are what NewGitHub needs.
type GitHubOptions struct {
	// Post runs a function on the UI thread; safe from any goroutine.
	Post func(func())
	// Auth is the sign-in; nil turns approve and merge off.
	Auth GitHubAuth
	// NewClient is a client acting with token: github.New in the app.
	NewClient func(token string) GitHubAPI
	// Owners are the accounts whose repositories the owner merges in: the
	// sign-in checks the App on each.
	Owners []string
	// Log gets each line of what GitHub was asked; nil drops them.
	Log func(Level, string)
	// Timeout bounds a read; 0 is a minute. An approve and merge has
	// Act's own bounds.
	Timeout time.Duration
}

// SignInPhase is where the GitHub sign-in stands.
type SignInPhase int

const (
	SignedOut SignInPhase = iota
	// SignInStarting: GitHub is asked for a code.
	SignInStarting
	// SignInCode: the code shows, and GitHub is polled until the owner
	// enters it.
	SignInCode
	// SignInChecking: the new pair is checked against the owner's login
	// and the App's installations.
	SignInChecking
	// SignInSaving: the keyring is keeping the pair.
	SignInSaving
	SignedIn
)

// MergePhase is where one pull request's approve and merge stands.
type MergePhase int

const (
	MergeNone MergePhase = iota
	// MergeChecking: the gates are read on GitHub.
	MergeChecking
	// MergeChecked: Preview holds what the gates said, or Err why the
	// read failed.
	MergeChecked
	// MergeActing: the approval and the merge are on their way.
	MergeActing
	// MergeDone: Result says how the attempt ended.
	MergeDone
)

// Merge is one pull request's approve and merge, as the window shows it.
type Merge struct {
	Phase   MergePhase
	Preview github.Preview
	// Err is why the last read or attempt could not run.
	Err string
	// AckHead is the head whose diff the owner opened: a risk:high pull
	// request merges at that head only.
	AckHead string
	Result  github.Result
}

// GitHub is the owner's sign-in and merges.
type GitHub struct {
	opts GitHubOptions
	// openErr is why the sign-in could not open: another PocketGo holds
	// it, or the keyring failed. Approve and merge are off then.
	openErr string
	opened  bool
	phase   SignInPhase
	code    github.DeviceCode
	fail    string
	gen     int
	poll    context.CancelFunc
	pollID  int
	owners  map[string]string
	merges  map[string]*Merge
	ops     int
}

// NewGitHub returns the sign-in and merges over opts.
func NewGitHub(opts GitHubOptions) *GitHub {
	if opts.Post == nil {
		panic("state.NewGitHub: Options.Post is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = time.Minute
	}
	return &GitHub{opts: opts, merges: map[string]*Merge{}}
}

func (g *GitHub) log(level Level, text string) {
	if g.opts.Log != nil {
		g.opts.Log(level, text)
	}
}

// run runs fn on a goroutine and hands its result to done on the UI thread.
func run[T any](g *GitHub, fn func(ctx context.Context) (T, error), done func(T, error)) {
	post, timeout := g.opts.Post, g.opts.Timeout
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		v, err := fn(ctx)
		post(func() { done(v, err) })
	}()
}

// Open takes the sign-in lock and loads the kept sign-in, on a goroutine:
// the keyring may wait for the owner. done runs on the UI thread.
func (g *GitHub) Open(done func()) {
	if g.opts.Auth == nil || g.opened {
		return
	}
	g.opened = true
	auth := g.opts.Auth
	run(g, func(context.Context) (struct{}, error) { return struct{}{}, auth.Open() }, func(_ struct{}, err error) {
		switch {
		case errors.Is(err, github.ErrNoSignIn):
		case err != nil:
			g.openErr = err.Error()
		}
		if auth.Status().SignedIn {
			g.phase = SignedIn
		}
		if done != nil {
			done()
		}
	})
}

// On says whether approve and merge can run: a sign-in that opened.
func (g *GitHub) On() bool { return g.opts.Auth != nil && g.opened && g.openErr == "" }

// Off is why approve and merge cannot run, "" when they can.
func (g *GitHub) Off() string {
	switch {
	case g.opts.Auth == nil:
		return "approve and merge need the desktop keyring"
	case !g.opened:
		return "the GitHub sign-in is not open yet"
	}
	return g.openErr
}

// SignIn is where the sign-in stands: its phase, the code to enter while
// it shows, and why the last sign-in failed.
func (g *GitHub) SignIn() (SignInPhase, github.DeviceCode, string) { return g.phase, g.code, g.fail }

// Status is the kept sign-in, without its token.
func (g *GitHub) Status() github.AppStatus {
	if g.opts.Auth == nil {
		return github.AppStatus{}
	}
	return g.opts.Auth.Status()
}

// Owners says, for each owner the sign-in checked, why the App cannot act
// there; "" where it can. Empty before a sign-in in this window.
func (g *GitHub) Owners() map[string]string { return g.owners }

// BeginSignIn starts the device flow: GitHub gives a code the owner enters
// at github.DeviceURL, and the window polls until they did. A new
// sign-in ends the one before; only its own results count.
func (g *GitHub) BeginSignIn() {
	if !g.On() || g.phase == SignInChecking || g.phase == SignInSaving {
		return
	}
	g.stopPoll()
	auth := g.opts.Auth
	g.gen = auth.BeginSignIn()
	gen := g.gen
	g.phase, g.fail = SignInStarting, ""
	g.log(LevelCommand, "Sign in to GitHub")
	run(g, auth.Start, func(dc github.DeviceCode, err error) {
		if gen != g.gen || g.phase != SignInStarting {
			return
		}
		if err != nil {
			g.phase, g.fail = g.signedOutPhase(), err.Error()
			return
		}
		g.phase, g.code = SignInCode, dc
		g.pollSignIn(dc, gen)
	})
}

// signedOutPhase is the phase a sign-in that ended goes back to: the kept
// sign-in's, if one is.
func (g *GitHub) signedOutPhase() SignInPhase {
	if g.Status().SignedIn {
		return SignedIn
	}
	return SignedOut
}

// pollSignIn polls for the pair until the code expires; CancelSignIn, a
// new sign-in and SignOut stop it.
func (g *GitHub) pollSignIn(dc github.DeviceCode, gen int) {
	auth, post := g.opts.Auth, g.opts.Post
	ctx, cancel := context.WithDeadline(context.Background(), dc.Expires)
	g.pollID++
	id := g.pollID
	g.poll = cancel
	go func() {
		tok, err := auth.Poll(ctx, dc)
		post(func() {
			cancel()
			if g.pollID == id {
				g.poll = nil
			}
			if gen != g.gen || g.phase != SignInCode {
				return
			}
			if err != nil {
				g.phase, g.fail = g.signedOutPhase(), err.Error()
				return
			}
			g.checkPair(tok, gen)
		})
	}()
}

func (g *GitHub) stopPoll() {
	if g.poll != nil {
		g.poll()
		g.poll = nil
	}
}

// checkPair keeps the new pair only when it acts as the owner, through
// the App: CheckApp names its login, and it must be github.Login.
func (g *GitHub) checkPair(tok github.AppToken, gen int) {
	auth, newClient, owners := g.opts.Auth, g.opts.NewClient, g.opts.Owners
	g.phase = SignInChecking
	run(g, func(ctx context.Context) (github.AppCheck, error) {
		return newClient(tok.Access).CheckApp(ctx, owners)
	}, func(check github.AppCheck, err error) {
		if gen != g.gen {
			return
		}
		if why := PairProblem(tok, check, err); why != "" {
			g.phase, g.fail = g.signedOutPhase(), why
			return
		}
		tok.Login = check.Login
		g.phase = SignInSaving
		run(g, func(context.Context) (struct{}, error) { return struct{}{}, auth.Adopt(tok, gen) }, func(_ struct{}, err error) {
			if gen != g.gen || errors.Is(err, github.ErrStale) {
				return
			}
			if err != nil {
				g.phase, g.fail = g.signedOutPhase(), "the keyring did not keep the sign-in: "+err.Error()
				return
			}
			g.phase, g.fail, g.owners = SignedIn, "", check.Owners
			// A preview read with another sign-in no longer holds.
			g.merges = map[string]*Merge{}
			g.log(LevelInfo, "Signed in to GitHub as "+check.Login)
		})
	})
}

// PairProblem says why a new pair is not kept, "" when it is: the check
// failed, or it acts as another login than github.Login, or it comes from
// another App than github.AppClientID.
func PairProblem(tok github.AppToken, check github.AppCheck, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case check.Login != github.Login:
		return "this sign-in is for " + check.Login + ", not " + github.Login + ": PocketGo acts only as the owner"
	case tok.ClientID != github.AppClientID:
		return "this sign-in is for another App than " + github.AppName
	}
	return ""
}

// CancelSignIn ends the sign-in that waits for its code: it keeps nothing.
func (g *GitHub) CancelSignIn() {
	if g.phase != SignInStarting && g.phase != SignInCode {
		return
	}
	g.stopPoll()
	g.opts.Auth.CancelSignIn(g.gen)
	g.gen++
	g.phase, g.fail = g.signedOutPhase(), ""
}

// SignOut forgets the sign-in: the keyring's entry goes, and every check
// with it. done gets the error, on the UI thread.
func (g *GitHub) SignOut(done func(error)) {
	if !g.On() {
		return
	}
	g.stopPoll()
	g.gen++
	auth := g.opts.Auth
	g.merges, g.owners = map[string]*Merge{}, nil
	g.log(LevelCommand, "Sign out of GitHub")
	run(g, func(context.Context) (struct{}, error) { return struct{}{}, auth.Forget() }, func(_ struct{}, err error) {
		g.phase = g.signedOutPhase()
		if done != nil {
			done(err)
		}
	})
}

// MergeRef is the pull request p as a GitHub call names it: from its
// repository and number, never its URL (ADR-0018).
func MergeRef(p proto.PullRequest) (github.Ref, bool) {
	if _, ok := PullURL(p); !ok {
		return github.Ref{}, false
	}
	ref, err := github.ParseRef(p.Repository + "#" + strconv.FormatInt(p.Number, 10))
	return ref, err == nil
}

// Merge is ref's approve and merge as it stands.
func (g *GitHub) Merge(ref github.Ref) Merge {
	if m := g.merges[ref.String()]; m != nil {
		return *m
	}
	return Merge{}
}

// lease runs fn with a client of the owner's token, and marks the token
// spent when GitHub refused it (401).
func (g *GitHub) lease(ctx context.Context, fn func(api GitHubAPI) error) error {
	auth := g.opts.Auth
	token, gen, release, err := auth.Lease(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = fn(g.opts.NewClient(token))
	var api *github.APIError
	if errors.As(err, &api) && api.Status == 401 {
		auth.Invalidate(gen)
	}
	return err
}

// Check reads ref's gates on GitHub as the owner (github.Client.Preview).
// A head the owner opened the diff of stays acknowledged while the read
// finds the same head.
func (g *GitHub) Check(ref github.Ref) {
	if !g.On() || g.phase != SignedIn {
		return
	}
	key := ref.String()
	m := g.merges[key]
	if m == nil {
		m = &Merge{}
		g.merges[key] = m
	}
	if m.Phase == MergeChecking || m.Phase == MergeActing {
		return
	}
	m.Phase, m.Err = MergeChecking, ""
	g.log(LevelCommand, "Check "+key+" to merge")
	gen := g.gen
	run(g, func(ctx context.Context) (github.Preview, error) {
		var p github.Preview
		err := g.lease(ctx, func(api GitHubAPI) (err error) {
			p, err = api.Preview(ctx, ref)
			return err
		})
		return p, err
	}, func(p github.Preview, err error) {
		if gen != g.gen || g.merges[key] != m {
			return // signed out or in again since
		}
		m.Phase = MergeChecked
		if err != nil {
			m.Err = err.Error()
			return
		}
		if m.AckHead != p.Head {
			m.AckHead = ""
		}
		m.Preview = p
	})
}

// OpenedDiff records that the owner opened the diff of ref's previewed
// head: a risk:high pull request may merge at that head.
func (g *GitHub) OpenedDiff(ref github.Ref) {
	if m := g.merges[ref.String()]; m != nil && m.Phase == MergeChecked && m.Preview.Head != "" {
		m.AckHead = m.Preview.Head
	}
}

// CanMerge says whether ref may be approved and merged now, and why not:
// its gates passed at the read shown, and a risk:high one's diff was
// opened at that head.
func (g *GitHub) CanMerge(ref github.Ref) (bool, string) {
	m := g.merges[ref.String()]
	switch {
	case m == nil || m.Phase == MergeNone:
		return false, "check it on GitHub first"
	case m.Phase == MergeChecking:
		return false, "checking it on GitHub"
	case m.Phase == MergeActing:
		return false, "approving and merging"
	case m.Phase == MergeDone:
		return false, "check it again first"
	case m.Err != "":
		return false, m.Err
	case !m.Preview.Eligible:
		return false, m.Preview.Reason
	case m.Preview.RiskHigh && m.AckHead != m.Preview.Head:
		return false, "it has risk:high: open its diff first"
	}
	return true, ""
}

// ApproveAndMerge approves ref at the head shown and merges or enqueues
// it, as the owner (github.Client.Act), when CanMerge says so: Act reads
// the gates again before each mutation, and holds the head and method of
// the preview. done runs on the UI thread with the result.
func (g *GitHub) ApproveAndMerge(ref github.Ref, done func(github.Result)) {
	if ok, _ := g.CanMerge(ref); !ok || !g.On() {
		return
	}
	key := ref.String()
	m := g.merges[key]
	in := github.Intent{Preview: m.Preview, Mode: "single", AckHead: m.AckHead}
	m.Phase, m.Err = MergeActing, ""
	g.ops++
	op := "merge-" + strconv.Itoa(g.ops)
	g.log(LevelCommand, "Approve and merge "+key+" at "+shortSHA(in.Preview.Head)+" as "+github.Login+" ("+in.Preview.Method+")")
	gen := g.gen
	post := g.opts.Post
	go func() {
		// Act bounds its own reads and mutations; the lease waits at most
		// the read timeout.
		ctx := context.Background()
		var res github.Result
		err := g.leaseWait(ctx, func(api GitHubAPI) error {
			res = api.Act(ctx, in, op, nil)
			return res.Err
		})
		if res.Kind == "" && err != nil {
			res = github.Result{Op: op, Kind: "failed", Reason: err.Error(), Err: err}
		}
		post(func() {
			if gen == g.gen && g.merges[key] == m {
				m.Phase, m.Result = MergeDone, res
			}
			g.log(LevelInfo, key+": "+res.Kind+resultReason(res))
			if done != nil {
				done(res)
			}
		})
	}()
}

// leaseWait is lease with the lease's own wait bounded by the read
// timeout.
func (g *GitHub) leaseWait(ctx context.Context, fn func(api GitHubAPI) error) error {
	auth := g.opts.Auth
	wait, cancel := context.WithTimeout(ctx, g.opts.Timeout)
	token, gen, release, err := auth.Lease(wait)
	cancel()
	if err != nil {
		return err
	}
	defer release()
	err = fn(g.opts.NewClient(token))
	var api *github.APIError
	if errors.As(err, &api) && api.Status == 401 {
		auth.Invalidate(gen)
	}
	return err
}

func resultReason(r github.Result) string {
	if r.Reason == "" {
		return ""
	}
	return ": " + r.Reason
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

var oid = regexp.MustCompile(`^[0-9a-f]{40}$`)

// DiffURL is the link to the preview's change on GitHub, the compare of
// its base and head commits, built from the reference and the two commit
// ids GitHub gave; ok is false when either is no commit id.
func DiffURL(p github.Preview) (string, bool) {
	if !oid.MatchString(p.BaseOid) || !oid.MatchString(p.Head) || p.Ref.Owner == "" || p.Ref.Name == "" {
		return "", false
	}
	return "https://github.com/" + p.Ref.Owner + "/" + p.Ref.Name + "/compare/" + p.BaseOid + "..." + p.Head, true
}
