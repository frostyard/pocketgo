package state

import (
	"context"
	"errors"
	"regexp"
	"slices"
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
	// BeginForget clears the sign-in and counts its keyring delete as
	// work, at once; finish deletes the entry, on a goroutine.
	BeginForget() (finish func() error, err error)
	// DeleteAgain deletes the keyring entry a sign-out could not; SaveAgain
	// saves the refreshed pair the keyring did not keep.
	DeleteAgain() error
	// BeginSaveAgain counts the save as work, at once; finish writes the
	// pair, on a goroutine (Codex review of #24, round 8).
	BeginSaveAgain() (finish func() error, err error)
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
	// SigningOut: the keyring's entry is being deleted; no sign-in starts
	// until it ends.
	SigningOut
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
	// AckBase and AckHead are the comparison whose diff the owner opened:
	// a risk:high pull request merges at that base and head only.
	AckBase, AckHead string
	Result           github.Result
	// CheckID names the check whose preview this is: a confirmation holds
	// for that check only (Codex review of #24, round 7).
	CheckID int
	// tokenGen is the generation of the token the check read with: the
	// merge runs only with that sign-in's (Codex review of #24, round 5).
	tokenGen int
}

// GitHub is the owner's sign-in and merges.
type GitHub struct {
	opts GitHubOptions
	// openErr is why the sign-in could not open: another PocketGo holds
	// it, or the keyring failed. Approve and merge are off then.
	openErr string
	// opening is set when Open began, ready when it ended: approve and
	// merge are on only once it ended well.
	opening, ready bool
	phase          SignInPhase
	code           github.DeviceCode
	fail           string
	// gen counts the sign-in attempts: a result of an earlier one counts
	// for nothing. cred counts the credentials: a new pair, a sign-out or
	// an ended sign-in makes the checks and merges of the one before stale.
	gen    int
	cred   int
	poll   context.CancelFunc
	pollID int
	owners map[string]string
	// checked says owners holds the App's installations for the
	// credential now: a merge waits for it (Codex review of #24, round 2).
	checked bool
	// reads numbers the installation reads; ownerRead the newest applied
	// per owner, so an older read never undoes a newer one (Codex review
	// of #24, round 7).
	reads     int
	ownerSeen map[string]int
	// notes are CheckApp's warnings per owner, which block nothing (an
	// enqueue may fail), read with owners (Codex review of #24, round 10).
	notes map[string]string
	// acting counts the approve and merges whose workers run, whatever
	// credential came since: each holds its lease (Codex review of #24,
	// round 10). forgetCred is a credential rejected while one ran, whose
	// pair goes once none does; 0 when none waits.
	acting     int
	forgetCred int
	// inFlight counts the running merges per pull request, whatever
	// credential came since: no second attempt at one starts until its
	// worker returned (Codex review of #24, round 11).
	inFlight map[string]int
	// removed is why PocketGo signed out a kept sign-in on its own, kept
	// until a new pair is adopted, so the views can say why (round 11).
	removed string
	checks  int
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
	if g.opts.Auth == nil || g.opening {
		return
	}
	g.opening = true
	auth := g.opts.Auth
	run(g, func(context.Context) (struct{}, error) { return struct{}{}, auth.Open() }, func(_ struct{}, err error) {
		g.ready = true
		switch {
		case errors.Is(err, github.ErrNoSignIn):
		case err != nil:
			g.openErr = err.Error()
		}
		if auth.Status().SignedIn && g.openErr == "" {
			g.phase = SignedIn
			g.checkOwners()
		}
		if done != nil {
			done()
		}
	})
}

// On says whether approve and merge can run: a sign-in whose Open ended
// well, the lock held and the keyring read (Codex review of #24, round 1).
func (g *GitHub) On() bool { return g.opts.Auth != nil && g.ready && g.openErr == "" }

// Off is why approve and merge cannot run, "" when they can.
func (g *GitHub) Off() string {
	switch {
	case g.opts.Auth == nil:
		return "approve and merge need the desktop keyring"
	case !g.ready:
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
	if !g.On() || g.phase == SignInChecking || g.phase == SignInSaving || g.phase == SigningOut {
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
	// A pair rejected while a merge ran stays out of use until it is
	// deleted or a new pair is adopted (Codex review of #24, round 11).
	if g.forgetCred != 0 && g.forgetCred == g.cred {
		return SignedOut
	}
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
			if gen != g.gen {
				return // a later sign-in attempt's
			}
			if errors.Is(err, github.ErrStale) {
				// The kept sign-in changed under this attempt (a refresh
				// ended it): nothing was kept, and it may begin again.
				g.phase, g.fail = g.signedOutPhase(), "the GitHub sign-in changed while it was kept: sign in again"
				return
			}
			if err != nil {
				g.phase, g.fail = g.signedOutPhase(), "the keyring did not keep the sign-in: "+err.Error()
				return
			}
			g.phase, g.fail, g.removed = SignedIn, "", ""
			// A preview read with another sign-in no longer holds.
			g.newCredential()
			g.reads++
			g.takeOwners(check, g.reads)
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
// A sign-out whose keyring deletion failed is tried again: the next start
// would load the entry the owner removed (Codex review of #24, round 1).
func (g *GitHub) SignOut(done func(error)) {
	if !g.On() || g.phase == SigningOut {
		return
	}
	// A running merge's client holds the token, which a sign-out cannot
	// take back: the sign-out waits for it (Codex review of #24, round 9).
	if g.Merging() {
		if done != nil {
			done(errors.New("an approve and merge is running: sign out once it ends"))
		}
		return
	}
	g.stopPoll()
	g.gen++
	auth := g.opts.Auth
	// Begun here, on the UI thread: the delete counts as work before a
	// Close can come, so the window never lets the lock go before it
	// (Codex review of #24, round 7). Forget, also after a deletion that
	// failed: it ends a sign-in being kept meanwhile, as DeleteAgain does
	// not (round 5).
	finish, err := auth.BeginForget()
	if err != nil {
		if done != nil {
			done(err)
		}
		return
	}
	g.newCredential()
	g.phase, g.fail = SigningOut, ""
	g.log(LevelCommand, "Sign out of GitHub")
	run(g, func(context.Context) (struct{}, error) { return struct{}{}, finish() }, func(_ struct{}, err error) {
		g.phase = g.signedOutPhase()
		if done != nil {
			done(err)
		}
	})
}

// CanSignOut says whether Sign Out has something to do: a sign-in kept, or
// a sign-out whose keyring deletion failed.
//
// A kept entry the sign-in rejected (another App, another login, a value
// that does not parse) can be signed out too: it stays in the keyring
// otherwise (Codex review of #24, round 7).
func (g *GitHub) CanSignOut() bool {
	st := g.Status()
	return g.On() && g.phase != SigningOut && !g.Merging() && (st.SignedIn || st.DeleteFailed || st.Reason != "")
}

// Merging says whether an approve and merge is running, under this
// credential or an earlier one.
func (g *GitHub) Merging() bool { return g.acting > 0 }

// MergeNote is CheckApp's warning for ref's owner when its check would
// enqueue: an enqueue may fail where the App lacks a permission the merge
// queue needs. It blocks nothing (Codex review of #24, round 10).
func (g *GitHub) MergeNote(ref github.Ref) string {
	m := g.merges[ref.String()]
	if m == nil || m.Phase != MergeChecked || !m.Preview.Queue {
		return ""
	}
	return g.notes[ref.Owner]
}

// acted ends a merge's worker: once none runs, a credential rejected
// meanwhile goes from the keyring.
func (g *GitHub) acted() {
	g.acting--
	if g.acting > 0 || g.forgetCred == 0 {
		return
	}
	if g.forgetCred != g.cred {
		g.forgetCred = 0 // a sign-in came since: its pair stays
		return
	}
	g.forgetCred = 0
	fail, removed := g.fail, g.removed
	g.SignOut(func(err error) {
		if err != nil {
			fail += " (forgetting it failed: " + err.Error() + ")"
			g.removed = removed + "; deleting it from the keyring failed: " + err.Error()
		}
		g.fail = fail
	})
	g.fail = fail
}

// KeptProblem is why the kept sign-in is not used, "" when none is kept or
// it is in use.
//
// A sign-in PocketGo signed out on its own says why, until a new pair is
// adopted: the keyring's entry is gone, so no Reason says it (Codex review
// of #24, round 11).
func (g *GitHub) KeptProblem() string {
	if g.removed != "" {
		return "PocketGo signed out of the kept GitHub sign-in: " + g.removed + "."
	}
	if st := g.Status(); !st.SignedIn && st.Reason != "" {
		return "The kept GitHub sign-in is not used: " + st.Reason
	}
	return ""
}

// CanOpenSignIn says whether the sign-in window has something to show: a
// flow that can begin, or one under way. Not while signing out (Codex
// review of #24, round 11).
func (g *GitHub) CanOpenSignIn() bool { return g.On() && g.phase != SigningOut }

// newCredential makes the checks and merges of the credential before
// stale: a new pair, a sign-out, or a sign-in that ended.
func (g *GitHub) newCredential() {
	g.cred++
	g.merges = map[string]*Merge{}
	g.owners, g.checked, g.ownerSeen, g.notes = nil, false, nil, nil
}

// checkOwners reads the App's installations with the kept credential, as
// a new sign-in does: a merge waits until they are read.
func (g *GitHub) checkOwners() {
	cred, owners := g.cred, g.opts.Owners
	g.reads++
	seq := g.reads
	run(g, func(ctx context.Context) (github.AppCheck, error) {
		var check github.AppCheck
		err := g.lease(ctx, func(api GitHubAPI, _ int) (err error) {
			check, err = api.CheckApp(ctx, owners)
			return err
		})
		return check, err
	}, func(check github.AppCheck, err error) {
		if cred != g.cred {
			return
		}
		if err != nil {
			g.fail = "reading the App's installations: " + err.Error()
			g.afterLease(err)
			return
		}
		g.takeOwners(check, seq)
	})
}

// takeOwners records what CheckApp read of the App's installations, for
// the kept credential: a login that is not github.Login signs the window
// out, since the pair acts as someone else now (an account renamed or
// another sign-in; Codex review of #24, round 3).
func (g *GitHub) takeOwners(check github.AppCheck, seq int) bool {
	if check.Login != github.Login {
		// The pair goes from the keyring too: kept, it would come back as
		// the sign-in at a cancel or the next start (Codex review of #24,
		// round 4).
		fail := "the kept GitHub sign-in acts as " + check.Login + ", not " + github.Login + ": sign in again"
		g.removed = "it acts as " + check.Login + ", not " + github.Login
		if g.Merging() {
			// Out of use at once; the pair goes from the keyring once the
			// running merge ends, unless a sign-in came since (Codex review
			// of #24, round 10).
			g.stopPoll()
			g.gen++
			g.newCredential()
			g.phase, g.fail = SignedOut, fail
			g.forgetCred = g.cred
			return false
		}
		removed := g.removed
		g.SignOut(func(err error) {
			if err != nil {
				fail += " (forgetting it failed: " + err.Error() + ")"
				g.removed = removed + "; deleting it from the keyring failed: " + err.Error()
			}
			g.fail = fail
		})
		g.fail = fail
		return false
	}
	if g.owners == nil {
		g.owners = map[string]string{}
	}
	if g.ownerSeen == nil {
		g.ownerSeen = map[string]int{}
	}
	if g.notes == nil {
		g.notes = map[string]string{}
	}
	for owner, why := range check.Owners {
		if seq < g.ownerSeen[owner] {
			continue // a newer read of this owner applied already
		}
		g.owners[owner], g.ownerSeen[owner] = why, seq
		g.notes[owner] = check.Notes[owner]
	}
	g.checked = true
	return true
}

// ownersToRead are the owners Check reads the App on: the configured ones
// until they were read, and ref's owner while it was not (Codex review of
// #24, round 3). None when all were.
func (g *GitHub) ownersToRead(ref github.Ref) []string {
	// An owner the App could not act on is read again: the owner may have
	// installed it, or granted what it lacked (Codex review of #24, round 4).
	why, known := g.owners[ref.Owner]
	if g.checked && known && why == "" {
		return nil
	}
	out := slices.Clone(g.opts.Owners)
	if !slices.Contains(out, ref.Owner) {
		out = append(out, ref.Owner)
	}
	return out
}

// CanSaveAgain says whether the keyring did not keep a refreshed pair:
// leases wait for SaveAgain (Codex review of #24, round 2).
func (g *GitHub) CanSaveAgain() bool { return g.On() && g.Status().Unsaved }

// SaveAgain saves the refreshed pair the keyring did not keep; done gets
// the error on the UI thread.
func (g *GitHub) SaveAgain(done func(error)) {
	if !g.CanSaveAgain() {
		return
	}
	// Begun here, on the UI thread: a Close that comes next waits for the
	// save (Codex review of #24, round 8).
	finish, err := g.opts.Auth.BeginSaveAgain()
	if err != nil {
		if done != nil {
			done(err)
		}
		return
	}
	g.log(LevelCommand, "Save the GitHub sign-in again")
	run(g, func(context.Context) (struct{}, error) { return struct{}{}, finish() }, func(_ struct{}, err error) {
		if done != nil {
			done(err)
		}
	})
}

// ownerRead says whether the App's installation on owner was read for
// the credential now: an owner never read is no permission (Codex review
// of #24, round 3).
func (g *GitHub) ownerRead(owner string) bool {
	_, ok := g.owners[owner]
	return g.checked && ok
}

// Opened says whether Open ended, well or not.
func (g *GitHub) Opened() bool { return g.ready }

// OwnerProblem is why the App cannot act on owner's repositories, as the
// sign-in found it: not installed, suspended, or short of a permission;
// "" when it can or the sign-in did not check it.
func (g *GitHub) OwnerProblem(owner string) string { return g.owners[owner] }

// afterLease brings the sign-in's phase in step after a call with the
// leased token failed: a pair that ended (expired or revoked refresh)
// signs out, and its checks and merges go.
func (g *GitHub) afterLease(err error) {
	if err == nil || g.phase != SignedIn || g.Status().SignedIn {
		return
	}
	g.phase, g.fail = SignedOut, "the GitHub sign-in ended: "+err.Error()
	g.newCredential()
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
	m := g.merges[ref.String()]
	// A merge whose worker runs under an earlier credential still shows:
	// it may yet approve and merge (Codex review of #24, round 11).
	if g.inFlight[ref.String()] > 0 && (m == nil || m.Phase != MergeActing) {
		return Merge{Phase: MergeActing}
	}
	if m != nil {
		return *m
	}
	return Merge{}
}

// lease runs fn with a client of the owner's token, and marks the token
// spent when GitHub refused it (401).
func (g *GitHub) lease(ctx context.Context, fn func(api GitHubAPI, tokenGen int) error) error {
	auth := g.opts.Auth
	token, gen, release, err := auth.Lease(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = fn(g.opts.NewClient(token), gen)
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
	if g.inFlight[key] > 0 {
		return // its merge runs, maybe under an earlier credential
	}
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
	cred := g.cred
	owners := g.ownersToRead(ref)
	seq := 0
	if len(owners) > 0 {
		g.reads++
		seq = g.reads
	}
	type read struct {
		p        github.Preview
		check    *github.AppCheck
		tokenGen int
	}
	run(g, func(ctx context.Context) (read, error) {
		var out read
		err := g.lease(ctx, func(api GitHubAPI, tokenGen int) (err error) {
			out.tokenGen = tokenGen
			if len(owners) > 0 {
				check, err := api.CheckApp(ctx, owners)
				if err != nil {
					return err
				}
				out.check = &check
				if check.Login != github.Login {
					return nil // takeOwners signs out
				}
			}
			out.p, err = api.Preview(ctx, ref)
			return err
		})
		return out, err
	}, func(out read, err error) {
		if cred != g.cred || g.merges[key] != m {
			return // another credential since
		}
		m.Phase = MergeChecked
		if out.check != nil && !g.takeOwners(*out.check, seq) {
			return
		}
		p := out.p
		if err != nil {
			m.Err = err.Error()
			g.afterLease(err)
			return
		}
		if m.AckHead != p.Head || m.AckBase != p.BaseOid {
			m.AckBase, m.AckHead = "", ""
		}
		g.checks++
		m.Preview, m.tokenGen, m.CheckID = p, out.tokenGen, g.checks
	})
}

// OpenedDiff records that the owner opened the diff of head, ref's
// previewed head when the window opened it: a risk:high pull request may
// merge at that head only. A check that read another head since makes it
// count for nothing (Codex review of #24, round 1).
//
// The acknowledgment holds the comparison the diff showed, its base and
// head: a pull request retargeted to another base shows another change at
// the same head (Codex review of #24, round 2).
func (g *GitHub) OpenedDiff(ref github.Ref, base, head string) {
	if m := g.merges[ref.String()]; m != nil && m.Phase == MergeChecked && head != "" && base != "" &&
		m.Preview.Head == head && m.Preview.BaseOid == base {
		m.AckBase, m.AckHead = base, head
	}
}

// CanMerge says whether p may be approved and merged now, and why not:
// its gates passed at the read shown; pocketd recorded a ready
// cross-review at that very head, since GitHub's label names the pull
// request, not a commit (ADR-0017, Codex review of #24, round 1); and a
// risk:high one's diff was opened at that head.
func (g *GitHub) CanMerge(p proto.PullRequest) (bool, string) {
	ref, ok := MergeRef(p)
	if !ok {
		return false, "pocketd names no GitHub pull request here"
	}
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
	case p.Base != m.Preview.BaseRef:
		return false, "pocketd read it merging into " + p.Base + ", the gates into " + m.Preview.BaseRef + ": wait for pocketd to read it again"
	case !Backed(p, m.Preview.Head, m.Preview.BaseRef):
		return false, "pocketd has no ready cross-review with green CI at " + shortSHA(m.Preview.Head) + ", the head the gates read"
	case !g.ownerRead(ref.Owner):
		return false, "the App's installation on " + ref.Owner + " is not read yet: check it again"
	case g.owners[ref.Owner] != "":
		return false, "the App " + github.AppName + " cannot act on " + ref.Owner + ": " + g.owners[ref.Owner]
	case m.Preview.RiskHigh && (m.AckHead != m.Preview.Head || m.AckBase != m.Preview.BaseOid):
		return false, "it has risk:high: open its diff first"
	}
	return true, ""
}

// ApproveAndMerge approves ref at the head shown and merges or enqueues
// it, as the owner (github.Client.Act), when CanMerge says so: Act reads
// the gates again before each mutation, and holds the head and method of
// the preview. done runs on the UI thread with the result.
//
// confirmed is the preview the owner confirmed: a check that read another
// since refuses, never takes its place (Codex review of #24, round 5).
//
// fresh is pocketd's answer at the confirmation (Store.ReadPull), the only
// source of one: both its row merged with the events and pocketd's own
// row must back the confirmed head, so a caller cannot act on a cached
// row (Codex review of #24, rounds 7 and 8). A refusal goes to done.
func (g *GitHub) ApproveAndMerge(fresh FreshPull, confirmedMerge Merge, done func(github.Result)) {
	refuse := func(why string) {
		if done != nil {
			done(github.Result{Kind: "refused", Reason: why})
		}
	}
	p := fresh.merged
	if !g.On() {
		refuse("approve and merge are off")
		return
	}
	if !fresh.read || fresh.direct.ID != p.ID || fresh.listed.ID != p.ID {
		refuse("pocketd was not read at the confirmation")
		return
	}
	if ok, why := g.CanMerge(p); !ok {
		refuse(why)
		return
	}
	ref, _ := MergeRef(p)
	key := ref.String()
	if g.inFlight[key] > 0 {
		refuse("an approve and merge of it is running")
		return
	}
	m := g.merges[key]
	confirmed := confirmedMerge.Preview
	// The check the owner confirmed, not one read since with the same
	// fields and another token (Codex review of #24, round 7).
	if m.CheckID != confirmedMerge.CheckID || !samePreview(m.Preview, confirmed) {
		refuse("it was checked again since you confirmed: confirm what it shows now")
		return
	}
	// pocketd's own answers back the head too: its read of GitHub, and its
	// list after it, since a replayed event may have won the merge of the
	// rows over a list that refuses (Codex review of #24, round 7; of #26,
	// round 9).
	if !Backed(fresh.direct, confirmed.Head, confirmed.BaseRef) || !Backed(fresh.listed, confirmed.Head, confirmed.BaseRef) {
		refuse("pocketd's answer does not back " + shortSHA(confirmed.Head) + ": check it again")
		return
	}
	in := github.Intent{Preview: confirmed, Mode: "single", AckHead: m.AckHead}
	tokenGen := m.tokenGen
	m.Phase, m.Err = MergeActing, ""
	g.ops++
	op := "merge-" + strconv.Itoa(g.ops)
	g.log(LevelCommand, "Approve and merge "+key+" at "+shortSHA(in.Preview.Head)+" as "+github.Login+" ("+in.Preview.Method+")")
	cred := g.cred
	post := g.opts.Post
	g.acting++
	if g.inFlight == nil {
		g.inFlight = map[string]int{}
	}
	g.inFlight[key]++
	go func() {
		// The whole attempt has its deadline, within the leased token's
		// validity (Codex review of #24, round 2).
		ctx, cancel := context.WithTimeout(context.Background(), actTimeout)
		defer cancel()
		var res github.Result
		err := g.leaseWait(ctx, func(api GitHubAPI, gen int) error {
			// Only the sign-in the check read with: one adopted since was
			// not checked for this (Codex review of #24, round 5).
			if gen != tokenGen {
				res = github.Result{Op: op, Kind: "refused", Reason: "the GitHub sign-in changed since the check: check it again"}
				return nil
			}
			res = api.Act(ctx, in, op, nil)
			return res.Err
		})
		if res.Kind == "" && err != nil {
			res = github.Result{Op: op, Kind: "failed", Reason: err.Error(), Err: err}
		}
		post(func() {
			if cred == g.cred && g.merges[key] == m {
				m.Phase, m.Result = MergeDone, res
			}
			if g.inFlight[key]--; g.inFlight[key] <= 0 {
				delete(g.inFlight, key)
			}
			defer g.acted()
			g.afterLease(err)
			g.log(LevelInfo, key+": "+res.Kind+resultReason(res))
			if done != nil {
				done(res)
			}
		})
	}()
}

// FreshPull is a pull request as pocketd answered it at a confirmation:
// the row merged with the events, pocketd's answer that read GitHub
// (direct), and its row in the list read after (listed). Only
// Store.ReadPull makes one (Codex review of #24, round 8; of #26, rounds
// 8 and 9).
type FreshPull struct {
	merged, direct, listed proto.PullRequest
	read                   bool
}

// Pull is the row merged with the events.
func (f FreshPull) Pull() proto.PullRequest { return f.merged }

// Backed says whether pocketd backs merging p at head into base
// (ADR-0017): its newest verdict there is ready, head is the head pocketd
// read and base the branch it read p merging into, and CI was green there
// when pocketd read it. pocketd gives the verdict of p's head on its base
// branch only (ADR-0019), so a pull request GitHub shows retargeted is
// backed by nothing pocketd read (Codex review of pocketgo#26, round 5).
// The live gates on GitHub come on top (Codex review of #24, round 2).
//
// The rule is whole here, in the merge's own file (a risk path), never
// borrowed from a view's (Codex review of #24, round 4).
func Backed(p proto.PullRequest, head, base string) bool {
	r := p.Review
	return head != "" && base != "" && p.HeadSHA == head && p.Base == base && r != nil && r.Verdict == proto.ReviewReady && r.Head == head &&
		p.Checks == proto.ChecksGreen
}

// actTimeout bounds a whole approve and merge: within the lease's
// validity, as TokenSource's jobs are.
const actTimeout = 5 * time.Minute

// leaseWait is lease with the lease's own wait bounded by the read
// timeout.
func (g *GitHub) leaseWait(ctx context.Context, fn func(api GitHubAPI, tokenGen int) error) error {
	auth := g.opts.Auth
	wait, cancel := context.WithTimeout(ctx, g.opts.Timeout)
	token, gen, release, err := auth.Lease(wait)
	cancel()
	if err != nil {
		return err
	}
	defer release()
	err = fn(g.opts.NewClient(token), gen)
	var api *github.APIError
	if errors.As(err, &api) && api.Status == 401 {
		auth.Invalidate(gen)
	}
	return err
}

// samePreview says whether two reads tell the same of what a merge acts
// on: the pull request, its base and head, the method and the queue, and
// risk:high.
func samePreview(a, b github.Preview) bool {
	return a.Ref == b.Ref && a.NodeID == b.NodeID && a.BaseRef == b.BaseRef && a.BaseOid == b.BaseOid && a.Head == b.Head &&
		a.Method == b.Method && a.Queue == b.Queue && a.RiskHigh == b.RiskHigh && a.Eligible == b.Eligible
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
