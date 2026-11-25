package github

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenSource gives the GitHub jobs a leased access token, and refreshes
// the pair when it nears its end
// (frostyard/pocket's docs/adr/0010-act-as-the-owner-through-a-github-app-user-token.md,
// section "Leases and refresh"):
//
//   - a lease needs 10 minutes of validity and a token not marked spent;
//   - when a refresh is due no new lease starts, and the refresh waits for
//     every lease to end, so it never cuts off a job;
//   - 1 refresh at a time, on its own goroutine and context, so a waiter
//     that leaves does not stop it;
//   - 1 small mutex for the state, never held across a network or keyring
//     call, and 1 write mutex for every credential write. Status never
//     waits.

const (
	// leaseValidity is the validity a lease needs: a job ends within 5
	// minutes, so a leased token never expires inside it.
	leaseValidity = 10 * time.Minute
	// refreshTimeout is how long 1 refresh may take.
	refreshTimeout = 30 * time.Second
	// closeWaitDefault is how long Close waits for a refresh or a write in
	// progress.
	closeWaitDefault = 35 * time.Second
	// suspendWaitDefault is how long Suspend waits for a refresh or a write
	// in progress: iOS gives an app about 5 seconds to leave for the
	// background (pocketgo ADR-0041).
	suspendWaitDefault = 3 * time.Second
)

// AppStore keeps the pair: the desktop keyring in the app, a map in tests.
// config.GitHubApp meets it.
type AppStore interface {
	Load() (AppToken, error)
	Save(AppToken) error
	Forget() error
}

// AppStatus is what Settings shows: no token, so it never leaks 1.
type AppStatus struct {
	SignedIn       bool
	Login          string
	RefreshExpires time.Time
	Unsaved        bool
	DeleteFailed   bool
	Reason         string
}

// TokenSource leases the access token of 1 GitHub App sign-in.
type TokenSource struct {
	oauth *OAuth
	store AppStore

	// mu guards the state below, and is never held across a network or
	// keyring call.
	mu      sync.Mutex
	changed chan struct{} // closed and replaced at each state change

	tok        AppToken // the pair in memory
	gen        int      // the token generation of tok
	have       bool     // tok is a pair PocketGo may lease
	signInGen  int      // the sign-in generation
	clearSeq   int      // how many times the pair was forgotten
	leases     int      // leases handed out and not released
	due        bool     // a refresh is due: no new lease starts
	refreshing bool     // 1 refresh runs
	refreshSeq int      // how many refreshes ended
	lastErr    error    // what the last refresh gave
	spent      bool     // GitHub refused the token: the next lease refreshes
	unsaved    bool     // the keyring did not keep tok
	deleteFail bool     // the keyring did not delete the entry
	reason     string   // why the stored sign-in is not usable
	closed     bool
	suspended  bool // the system suspends the app: no refresh starts

	// wmu serializes every credential write: Adopt, SaveAgain, DeleteAgain,
	// Forget and the save after a refresh.
	wmu sync.Mutex

	// bmu guards busy, the number of a refresh or a credential write in
	// progress; idle closes when it reaches 0.
	bmu  sync.Mutex
	busy int
	idle chan struct{}

	// queued, when set, is told which credential write is about to wait for
	// the write mutex. Tests use it to order 2 writes.
	queued func(what string)
	// beforeRefresh, when set, runs at the top of a refresh worker, before
	// it does anything. Tests use it to replace the pair in that window.
	beforeRefresh func()
	// now is the clock; nil is the OAuth's clock, else time.Now. Tests
	// inject it.
	now func() time.Time
	// closeWait is how long Close waits; 35 seconds unless a test sets it.
	// suspendWait is Suspend's; 3 seconds unless a test sets it. closeAfter
	// is the timer of either wait; nil is a real one, and a test gives one
	// it fires itself.
	closeWait   time.Duration
	suspendWait time.Duration
	closeAfter  func(time.Duration) <-chan time.Time
}

// NewTokenSource is a source of leases over o's sign-in, kept in s.
func NewTokenSource(o *OAuth, s AppStore) *TokenSource {
	return &TokenSource{oauth: o, store: s, changed: make(chan struct{}), closeWait: closeWaitDefault,
		suspendWait: suspendWaitDefault}
}

func (t *TokenSource) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	if t.oauth != nil && t.oauth.now != nil {
		return t.oauth.now()
	}
	return time.Now()
}

// changeLocked wakes every waiter: the state changed.
func (t *TokenSource) changeLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// Load reads the store once: no entry is no sign-in and no error, a value
// that is not a sign-in sets the reason, and any other error comes back (an
// unsupported or locked keyring turns Approve + merge off).
func (t *TokenSource) Load() error {
	tok, err := t.store.Load()
	if err != nil {
		var bad *BadSignIn
		if errors.As(err, &bad) {
			t.mu.Lock()
			t.reason = bad.Error()
			t.changeLocked()
			t.mu.Unlock()
			return nil
		}
		if errors.Is(err, ErrNoSignIn) {
			return nil
		}
		return err
	}
	if !tok.valid() {
		t.mu.Lock()
		t.reason = (&BadSignIn{Reason: "a field is empty"}).Error()
		t.changeLocked()
		t.mu.Unlock()
		return nil
	}
	t.mu.Lock()
	t.tok, t.have = tok, true
	t.gen++
	t.due, t.spent, t.unsaved, t.deleteFail, t.reason = false, false, false, false, ""
	t.lastErr = nil
	t.changeLocked()
	t.mu.Unlock()
	return nil
}

// Lease gives an access token, its token generation and a release, which
// the caller must call when its job ends. It waits while a refresh is due
// and a lease or another refresh holds the pair; ctx ends that wait.
func (t *TokenSource) Lease(ctx context.Context) (token string, tokenGen int, release func(), err error) {
	// seq is the count of the refreshes that ended when this waiter last
	// slept: a waiter reports the failure of the refresh it waited for, and
	// a lease that comes later tries again.
	seq := -1
	for {
		// A waiter that has already left holds no lease and starts no
		// refresh.
		if err := ctx.Err(); err != nil {
			return "", 0, nil, err
		}
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return "", 0, nil, fmt.Errorf("%w: PocketGo is closing", ErrNotSignedIn)
		}
		if seq >= 0 && t.lastErr != nil && (!t.have || t.refreshSeq != seq) {
			// The refresh this waiter waited for failed: it reports it, and
			// a later lease tries again.
			err := t.lastErr
			t.mu.Unlock()
			return "", 0, nil, err
		}
		if !t.have {
			t.mu.Unlock()
			return "", 0, nil, ErrNotSignedIn
		}
		if t.unsaved {
			t.mu.Unlock()
			return "", 0, nil, ErrUnsaved
		}
		if !t.spent && !t.due && t.tok.AccessExpires.Sub(t.clock()) >= leaseValidity {
			t.leases++
			gen, tok := t.gen, t.tok.Access
			t.mu.Unlock()
			return tok, gen, t.release, nil
		}
		// A refresh is due: no new lease starts.
		if !t.due {
			t.due = true
			t.changeLocked()
		}
		if !t.refreshing && t.leases == 0 {
			if !t.tok.RefreshExpires.After(t.clock()) {
				// The refresh token expired: the sign-in ended. The delete
				// counts as work before mu goes, so Close never lets the
				// lock go between the two (Codex review of #24, round 4).
				cleared := t.clearLocked()
				t.changeLocked()
				t.startWork()
				t.mu.Unlock()
				// The delete runs on its own goroutine, to its end: the write
				// mutex and the keyring do not watch ctx, so this caller
				// waits for it only until ctx ends (Codex review of #24,
				// round 11).
				deleted := make(chan struct{})
				go func() {
					defer close(deleted)
					t.deleteCounted(cleared)
				}()
				select {
				case <-deleted:
					return "", 0, nil, ErrSignIn
				case <-ctx.Done():
					return "", 0, nil, fmt.Errorf("%w: %w", ErrSignIn, ctx.Err())
				}
			}
			// While the system suspends the app no refresh starts: GitHub
			// could spend the refresh token and the app stop before the new
			// pair reaches the store. The lease waits for Resume (pocketgo
			// ADR-0041).
			if !t.suspended {
				t.refreshing = true
				t.startWork()
				// The pair this refresh is for, taken here: a Sign out and a
				// new sign-in may replace it before the worker runs, and the
				// worker must never spend the newer pair's refresh token
				// while a job holds a lease of that pair.
				go t.refresh(t.tok, t.gen)
			}
		}
		seq = t.refreshSeq
		ch := t.changed
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", 0, nil, ctx.Err()
		}
	}
}

// release ends 1 lease.
func (t *TokenSource) release() {
	t.mu.Lock()
	if t.leases > 0 {
		t.leases--
	}
	t.changeLocked()
	t.mu.Unlock()
}

// refresh trades tok's refresh token for a new pair, saves it, and lets the
// leases go on: 1 at a time, on its own context and goroutine. tok and gen
// are the pair the Lease that started it decided for, taken under the small
// mutex there. Every outcome applies only to that pair: a Sign out or a new
// pair that ran meanwhile wins, and its keyring entry stays.
func (t *TokenSource) refresh(tok AppToken, gen int) {
	defer t.endWork()
	defer t.endRefresh()
	if t.beforeRefresh != nil {
		t.beforeRefresh()
	}
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	next, err := t.oauth.Refresh(ctx, tok.Refresh)
	switch {
	case err == nil:
	case errors.Is(err, ErrSignIn):
		// The refresh token is spent, refused or expired: the sign-in ended.
		t.endSignIn(gen, err)
		return
	default:
		// No answer: the entry stays, because the refresh may have landed;
		// the waiters fail and the next lease tries again.
		t.mu.Lock()
		if t.have && t.gen == gen {
			t.due = true
			t.lastErr = fmt.Errorf("%w: could not refresh the GitHub sign-in", ErrNoAnswer)
		}
		t.mu.Unlock()
		return
	}
	if next.ClientID != tok.ClientID {
		t.endSignIn(gen, fmt.Errorf("%w: the new pair is for another build of PocketGo", ErrSignIn))
		return
	}
	// A refresh names no login: the pair keeps the current one.
	next.Login = tok.Login
	// The store holds the new pair before a lease may use it, and only
	// while the pair it refreshes is still the current one. A refresh that
	// was already running still saves when PocketGo closes: GitHub has spent
	// the old refresh token, so the new pair must not be lost.
	saved, serr := t.saveRefresh(next, gen)
	t.mu.Lock()
	if saved && t.have && t.gen == gen {
		t.tok, t.gen = next, t.gen+1
		t.due, t.spent, t.unsaved = false, false, serr != nil
		if serr == nil {
			t.reason = ""
			t.lastErr = nil
		}
	}
	t.mu.Unlock()
}

// endSignIn ends the sign-in that a refresh of generation gen found over: the
// pair in memory and the entry go, but only while gen is still the current
// pair, and the delete runs under the write mutex where it checks again. A
// Sign out or a new pair that ran meanwhile wins.
func (t *TokenSource) endSignIn(gen int, reason error) {
	t.startWork()
	defer t.endWork()
	t.queue("end sign-in")
	t.wmu.Lock()
	defer t.wmu.Unlock()
	t.mu.Lock()
	if !t.have || t.gen != gen {
		t.mu.Unlock()
		return
	}
	cleared := t.clearLocked()
	t.lastErr = reason
	t.changeLocked()
	t.mu.Unlock()
	err := t.deleteLocked(cleared)
	t.mu.Lock()
	if t.clearSeq == cleared {
		t.deleteFail = err != nil
	}
	t.changeLocked()
	t.mu.Unlock()
}

// endRefresh says the 1 refresh is over, and wakes the waiters: the next
// count tells them whether the refresh they waited for failed.
func (t *TokenSource) endRefresh() {
	t.mu.Lock()
	t.refreshing = false
	t.refreshSeq++
	t.changeLocked()
	t.mu.Unlock()
}

// clearLocked forgets the pair and ends the sign-in generation: no lease
// starts after it, and the jobs that hold 1 finish. It gives the clearSeq it
// left, which a delete checks. The caller wakes the waiters.
func (t *TokenSource) clearLocked() int {
	t.tok, t.have = AppToken{}, false
	t.signInGen++
	t.clearSeq++
	t.due, t.spent, t.unsaved, t.lastErr = false, false, false, nil
	return t.clearSeq
}

// deleteCounted deletes the entry of a cleared sign-in, under the write
// mutex, and only while that clear is still the last 1: a pair adopted
// while this waited keeps its entry. The caller counted it as work under mu
// (startWork); it ends that work.
func (t *TokenSource) deleteCounted(cleared int) {
	defer t.endWork()
	t.queue("delete")
	t.wmu.Lock()
	err := t.deleteLocked(cleared)
	t.wmu.Unlock()
	t.mu.Lock()
	if t.clearSeq == cleared {
		t.deleteFail = err != nil
	}
	t.changeLocked()
	t.mu.Unlock()
}

// deleteLocked deletes the entry while the clear of cleared is still the last
// 1 and no pair is in memory: a pair adopted after that clear keeps its
// entry. The caller holds wmu, so no other credential write runs between the
// check and the delete.
func (t *TokenSource) deleteLocked(cleared int) error {
	t.mu.Lock()
	stale := t.clearSeq != cleared || t.have
	t.mu.Unlock()
	if stale {
		return nil
	}
	return t.store.Forget()
}

// saveRefresh writes the refreshed pair under the write mutex, as a
// credential write Close waits for, but only while gen is still the current
// pair: it gives false, and writes nothing, when a Sign out or another
// sign-in ran during the refresh. Close does not refuse it: GitHub has
// already spent the old refresh token.
func (t *TokenSource) saveRefresh(next AppToken, gen int) (bool, error) {
	t.startWork()
	defer t.endWork()
	t.queue("refresh save")
	t.wmu.Lock()
	defer t.wmu.Unlock()
	t.mu.Lock()
	stale := !t.have || t.gen != gen
	t.mu.Unlock()
	if stale {
		return false, nil
	}
	if err := t.store.Save(next); err != nil {
		return true, fmt.Errorf("%w: %s", ErrUnsaved, Redact(err, next.secrets()...))
	}
	return true, nil
}

// startWork counts a refresh or a credential write in progress.
// queue says the write is about to wait for the write mutex.
func (t *TokenSource) queue(what string) {
	if t.queued != nil {
		t.queued(what)
	}
}

// ErrClosing is a credential write asked for once Close began: none starts
// then, so the sign-in lock goes only after the last write (pocketgo
// ADR-0018, Codex review of #24, round 3).
var ErrClosing = errors.New("PocketGo is closing: the GitHub sign-in writes nothing more")

// admit counts a credential write in, unless Close began: under mu, so
// Close either sees it at work or it never starts.
func (t *TokenSource) admit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosing
	}
	t.startWork()
	return nil
}

func (t *TokenSource) startWork() {
	t.bmu.Lock()
	t.busy++
	t.bmu.Unlock()
}

// endWork ends 1.
func (t *TokenSource) endWork() {
	t.bmu.Lock()
	t.busy--
	if t.busy == 0 && t.idle != nil {
		close(t.idle)
		t.idle = nil
	}
	t.bmu.Unlock()
}

// BeginSignIn starts a sign-in: it adds 1 to the sign-in generation, and
// Adopt must carry it.
func (t *TokenSource) BeginSignIn() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.signInGen++
	t.changeLocked()
	return t.signInGen
}

// CancelSignIn cancels sign-in gen: a sign-in that has not committed saves
// nothing. A cancel of an older generation changes nothing.
func (t *TokenSource) CancelSignIn(gen int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.signInGen {
		return
	}
	t.signInGen++
	t.changeLocked()
}

// Adopt saves tok as the sign-in of generation gen: ErrStale when that
// generation is no longer current, ErrUnsaved when the keyring keeps
// nothing.
func (t *TokenSource) Adopt(tok AppToken, gen int) error {
	if !tok.valid() {
		return fmt.Errorf("%w: the sign-in is incomplete", ErrUnsaved)
	}
	if err := t.admit(); err != nil {
		return err
	}
	defer t.endWork()
	t.queue("adopt")
	t.wmu.Lock()
	defer t.wmu.Unlock()
	t.mu.Lock()
	stale := gen != t.signInGen
	cleared := t.clearSeq
	t.mu.Unlock()
	if stale {
		return ErrStale
	}
	if err := t.store.Save(tok); err != nil {
		return fmt.Errorf("%w: %s", ErrUnsaved, Redact(err, tok.secrets()...))
	}
	t.mu.Lock()
	// A Sign out while the save ran applies after it: the pair does not
	// come back.
	if t.clearSeq == cleared {
		t.tok, t.have = tok, true
		t.gen++
		t.due, t.spent, t.unsaved, t.deleteFail, t.reason = false, false, false, false, ""
		t.lastErr = nil
	}
	t.changeLocked()
	t.mu.Unlock()
	return nil
}

// SaveAgain saves the pair the keyring did not keep, and only that pair: a
// Sign out, or a newer pair, that ran while this waited for the write mutex
// wins, and this saves nothing.
func (t *TokenSource) SaveAgain() error {
	finish, err := t.BeginSaveAgain()
	if err != nil {
		return err
	}
	return finish()
}

// BeginSaveAgain is SaveAgain's first half, which never waits: it counts
// the save as work, so a Close that comes before finish waits for it, as
// BeginForget does (Codex review of #24, round 8). The caller runs finish
// once, on any goroutine; it writes the pair.
func (t *TokenSource) BeginSaveAgain() (finish func() error, err error) {
	t.mu.Lock()
	tok, gen, unsaved := t.tok, t.gen, t.unsaved
	if !unsaved {
		t.mu.Unlock()
		return func() error { return nil }, nil
	}
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosing
	}
	t.startWork()
	t.mu.Unlock()
	return func() error {
		defer t.endWork()
		t.queue("save again")
		t.wmu.Lock()
		defer t.wmu.Unlock()
		if !t.current(gen, true) {
			return nil
		}
		if err := t.store.Save(tok); err != nil {
			return fmt.Errorf("%w: %s", ErrUnsaved, Redact(err, tok.secrets()...))
		}
		t.mu.Lock()
		if t.have && t.gen == gen {
			t.unsaved = false
		}
		t.changeLocked()
		t.mu.Unlock()
		return nil
	}, nil
}

// current says the pair of generation gen is still the 1 in memory, and,
// with unsaved, that it is still the 1 the keyring did not keep. The caller
// holds wmu, so no other credential write runs between this and the write.
func (t *TokenSource) current(gen int, unsaved bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.have && t.gen == gen && t.unsaved == unsaved
}

// DeleteAgain deletes the entry a Sign out could not delete, and only while
// no newer pair was adopted: that pair's entry stays.
func (t *TokenSource) DeleteAgain() error {
	t.mu.Lock()
	failed := t.deleteFail
	t.mu.Unlock()
	if !failed {
		return nil
	}
	if err := t.admit(); err != nil {
		return err
	}
	defer t.endWork()
	t.queue("delete again")
	t.wmu.Lock()
	t.mu.Lock()
	// A pair adopted since the failed Sign out keeps its entry.
	if t.have {
		t.mu.Unlock()
		t.wmu.Unlock()
		return nil
	}
	t.mu.Unlock()
	err := t.store.Forget()
	t.wmu.Unlock()
	t.mu.Lock()
	if !t.have {
		t.deleteFail = err != nil
	}
	t.changeLocked()
	t.mu.Unlock()
	if err != nil {
		return fmt.Errorf("the keyring did not delete the GitHub sign-in: %s", err)
	}
	return nil
}

// Forget is Sign out: no new lease starts, the jobs that hold 1 finish, and
// the entry goes. The delete runs under the write mutex, and only while no
// newer pair was adopted: a sign-in that committed while this waited keeps
// its entry. A delete that fails keeps DeleteFailed for Delete again.
func (t *TokenSource) Forget() error {
	finish, err := t.BeginForget()
	if err != nil {
		return err
	}
	return finish()
}

// BeginForget is Forget's first half, which never waits: it clears the
// pair and counts the delete as work, so a Close that comes before finish
// runs waits for it (pocketgo ADR-0018, Codex review of #24, round 7).
// finish deletes the entry; the caller runs it once, on any goroutine.
func (t *TokenSource) BeginForget() (finish func() error, err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosing
	}
	cleared := t.clearLocked()
	t.changeLocked()
	t.startWork()
	t.mu.Unlock()
	return func() error { return t.forgetCounted(cleared) }, nil
}

// forgetCounted deletes the entry of the clear cleared, a delete already
// counted as work, and ends that work.
func (t *TokenSource) forgetCounted(cleared int) error {
	defer t.endWork()
	t.queue("sign out")
	t.wmu.Lock()
	err := t.deleteLocked(cleared)
	t.wmu.Unlock()
	t.mu.Lock()
	if t.clearSeq == cleared {
		t.deleteFail = err != nil
		if err == nil {
			// The entry the sign-in rejected is gone with it (Codex review
			// of #24, round 7).
			t.reason = ""
		}
	}
	t.changeLocked()
	t.mu.Unlock()
	if err != nil {
		return fmt.Errorf("the keyring still holds the GitHub sign-in: %s", err)
	}
	return nil
}

// Invalidate marks the pair of tokenGen spent, after a 401: an older
// generation changes nothing, and the current one refreshes at the next
// lease.
func (t *TokenSource) Invalidate(tokenGen int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.have || tokenGen != t.gen || t.spent {
		return
	}
	t.spent = true
	t.changeLocked()
}

// Status copies the state: it never waits, not on a refresh and not on a
// write.
func (t *TokenSource) Status() AppStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := AppStatus{
		SignedIn:     t.have,
		Unsaved:      t.unsaved,
		DeleteFailed: t.deleteFail,
		Reason:       t.reason,
	}
	if t.have {
		st.Login = t.tok.Login
		st.RefreshExpires = t.tok.RefreshExpires
	}
	return st
}

// Close stops new leases and waits closeWait (35 seconds) at most for a
// refresh or a credential write in progress. The sign-in lock goes only
// with the process, never here.
//
// It says whether the work ended: false when the wait ran out with a
// refresh or a write still running, and the caller then keeps the
// sign-in lock until the process ends.
func (t *TokenSource) Close() bool {
	t.mu.Lock()
	t.closed = true
	t.changeLocked()
	t.mu.Unlock()
	return t.waitIdle(t.closeWait)
}

// Suspend is the system suspending the app, as iOS does when it goes to the
// background (pocketgo ADR-0041; the rest of this file is
// frostyard/pocket's): no refresh starts until Resume, and it waits
// suspendWait (3 seconds) at most for a refresh or a credential write in
// progress, so a pair GitHub has refreshed reaches the store before the
// app stops. Leases of a pair that needs no refresh go on. It says whether
// the work ended. The sign-in lock stays: it goes only with the process.
func (t *TokenSource) Suspend() bool {
	t.mu.Lock()
	t.suspended = true
	t.changeLocked()
	t.mu.Unlock()
	return t.waitIdle(t.suspendWait)
}

// Resume is the app back from the background: a lease that waited for a
// refresh starts it.
func (t *TokenSource) Resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.suspended {
		t.suspended = false
		t.changeLocked()
	}
}

// waitIdle waits wait at most for no refresh or credential write to be in
// progress, and says whether that came.
func (t *TokenSource) waitIdle(wait time.Duration) bool {
	// One deadline for the whole wait; a test gives its own (closeAfter).
	var expired <-chan time.Time
	if t.closeAfter != nil {
		expired = t.closeAfter(wait)
	} else {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		expired = timer.C
	}
	for {
		t.bmu.Lock()
		if t.busy == 0 {
			t.bmu.Unlock()
			return true
		}
		if t.idle == nil {
			t.idle = make(chan struct{})
		}
		ch := t.idle
		t.bmu.Unlock()
		select {
		case <-ch:
		case <-expired:
			return false
		}
	}
}
