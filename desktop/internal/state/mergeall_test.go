package state

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// backedPull is frostyard/nsl#n as pocketd links it, with id n: open, with
// a ready verdict and green CI at headX on main.
func backedPull(n int) proto.PullRequest {
	p := pullOf(github.Ref{Owner: "frostyard", Name: "nsl", Number: n})
	p.ID = int64(n)
	return p
}

func refOf(n int) github.Ref { return github.Ref{Owner: "frostyard", Name: "nsl", Number: n} }

// pocketdReads is pocketd as Store.ReadPull reads it at each turn: its rows
// by id, an error or a hold per id. It notes the ids read, and how many
// approve and merges had begun at each read.
type pocketdReads struct {
	h       *ghHarness
	rows    map[int64]proto.PullRequest
	errs    map[int64]error
	hold    map[int64]chan struct{}
	ids     []int64
	intents []int
	// previews are how many previews GitHub had read at each read.
	previews []int
}

func (r *pocketdReads) read(id int64, done func(FreshPull, bool, error)) {
	r.ids = append(r.ids, id)
	r.h.api.mu.Lock()
	r.intents = append(r.intents, len(r.h.api.intents))
	n := 0
	for _, crs := range r.h.api.crs {
		n += len(crs)
	}
	r.previews = append(r.previews, n)
	r.h.api.mu.Unlock()
	p, found := r.rows[id]
	err, w := r.errs[id], r.hold[id]
	go func() {
		wait(w)
		r.h.loop.Post(func() {
			if err != nil {
				done(FreshPull{}, false, err)
				return
			}
			if !found {
				done(FreshPull{}, false, nil)
				return
			}
			done(fresh(p), true, nil)
		})
	}()
}

// dropped is a read of pocketd whose answer the store drops: it never
// calls done, and holds no goroutine (Codex review of #53, round 2).
func dropped(int64, func(FreshPull, bool, error)) {}

func newPocketdReads(h *ghHarness, rows ...proto.PullRequest) *pocketdReads {
	r := &pocketdReads{h: h, rows: map[int64]proto.PullRequest{}, errs: map[int64]error{}, hold: map[int64]chan struct{}{}}
	for _, p := range rows {
		r.rows[p.ID] = p
	}
	return r
}

// confirmAll runs Merge All Ready over rows to its confirmation.
func confirmAll(t *testing.T, h *ghHarness, rows []proto.PullRequest) MergeAll {
	t.Helper()
	h.gh.MergeAllReady(func() []proto.PullRequest { return rows })
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase != MergeAllChecking })
	m := h.gh.MergeAll()
	if m.Phase != MergeAllConfirming {
		t.Fatalf("merge all %+v", m)
	}
	return m
}

func itemRefs(m MergeAll) []github.Ref {
	var out []github.Ref
	for _, it := range m.Items {
		out = append(out, it.Ref)
	}
	return out
}

// The gates count pocketd's verdict as the row has it: ready, at the head
// pocketd read and on its base branch, of an open pull request that is no
// draft (ADR-0031). Check counts the row's; Approve + Merge counts pocketd's
// answer at the confirmation.
func TestTheGatesCountPocketdsVerdictOfTheRow(t *testing.T) {
	if github.VerdictReady != string(proto.ReviewReady) {
		t.Fatalf("github.VerdictReady %q, proto.ReviewReady %q", github.VerdictReady, proto.ReviewReady)
	}
	if got, want := CrossReviewOf(pull31), (github.CrossReview{Verdict: "ready", Head: headX, Base: "main"}); got != want {
		t.Fatalf("CrossReviewOf(pull31) %+v, want %+v", got, want)
	}
	stale := pull31
	stale.HeadSHA = headY // a new head pocketd read; the verdict is of headX
	draft := pull31
	draft.Draft = true
	closed := pull31
	closed.State = proto.PullClosed
	none := pull31
	none.Review = nil
	noBase := pull31
	noBase.Base = ""
	for name, p := range map[string]proto.PullRequest{"a verdict of an older head": stale, "a draft": draft, "a closed one": closed,
		"no verdict": none, "no base branch": noBase} {
		if got := CrossReviewOf(p); got != (github.CrossReview{}) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	notReady := pull31
	r := *pull31.Review
	r.Verdict = proto.ReviewNotReady
	notReady.Review = &r
	if got := CrossReviewOf(notReady); got.Verdict != "not-ready" {
		t.Errorf("a not-ready verdict goes to the gates as it is, which count it for nothing: %+v", got)
	}

	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(pull31)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(ref31).Phase == MergeChecked })
	h.api.mu.Lock()
	crs := slices.Clone(h.api.crs[ref31.String()])
	h.api.mu.Unlock()
	if len(crs) != 1 || crs[0] != CrossReviewOf(pull31) {
		t.Fatalf("the preview counted %+v", crs)
	}
	done := false
	h.gh.ApproveAndMerge(fresh(pull31), h.gh.Merge(ref31), func(github.Result) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if len(h.api.intents) != 1 || h.api.intents[0].CrossReview != CrossReviewOf(pull31) {
		t.Fatalf("intents %+v", h.api.intents)
	}
}

// Merge All Ready checks every pull request pocketd backs at its head, and
// lists in its confirmation only those whose Approve + Merge is on once
// every check ended (ADR-0031). Nothing merges before the confirmation.
func TestMergeAllReadyListsWhatPassesNow(t *testing.T) {
	h := newGHHarness(t)
	if ok, why := h.gh.CanMergeAll([]proto.PullRequest{backedPull(31)}); ok || !strings.Contains(why, "sign in") {
		t.Fatalf("signed out: %v %q", ok, why)
	}
	h.signIn(t)
	passes, refused, notReady, draft := backedPull(31), backedPull(32), backedPull(33), backedPull(34)
	r := *notReady.Review
	r.Verdict = proto.ReviewNotReady
	notReady.Review = &r
	draft.Draft = true
	h.api.previews = map[string]github.Preview{refOf(32).String(): {Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH",
		Reason: "1 review thread is not resolved"}}
	if ok, why := h.gh.CanMergeAll([]proto.PullRequest{notReady, draft}); ok || !strings.Contains(why, "pocketd backs no pull request") {
		t.Fatalf("none backed: %v %q", ok, why)
	}
	rows := []proto.PullRequest{passes, refused, notReady, draft}
	m := confirmAll(t, h, rows)
	if got := itemRefs(m); !slices.Equal(got, []github.Ref{refOf(31)}) || m.Checked != 2 {
		t.Fatalf("listed %v, checked %d", got, m.Checked)
	}
	if m.Items[0].Merge.CheckID != h.gh.Merge(refOf(31)).CheckID || m.Items[0].Merge.Preview.Head != headX {
		t.Fatalf("the item's check %+v", m.Items[0].Merge)
	}
	h.api.mu.Lock()
	checked := len(h.api.crs)
	_, read33 := h.api.crs[refOf(33).String()]
	h.api.mu.Unlock()
	if checked != 2 || read33 {
		t.Fatalf("checked %d pull requests, the not-ready one read %v", checked, read33)
	}
	if len(h.api.intents) != 0 {
		t.Fatalf("it merged before the confirmation: %+v", h.api.intents)
	}
	if ok, why := h.gh.CanMergeAll(rows); ok || !strings.Contains(why, "running") {
		t.Fatalf("a second run: %v %q", ok, why)
	}
	// The card of the refused one says why, as Check to Merge's would.
	if ok, why := h.gh.CanMerge(refused); ok || why != "1 review thread is not resolved" {
		t.Fatalf("the refused card: %v %q", ok, why)
	}

	// A run that lists none ends, and says so.
	h.gh.CancelMergeAll(m.ID)
	h.api.previews[refOf(31).String()] = h.api.previews[refOf(32).String()]
	h.gh.MergeAllReady(func() []proto.PullRequest { return rows })
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase != MergeAllChecking })
	if m := h.gh.MergeAll(); m.Phase != MergeAllEnded || len(m.Items) != 0 || !strings.Contains(m.End, "no pull request's gates pass now") {
		t.Fatalf("none passes: %+v", m)
	}
}

// After the one confirmation, the listed pull requests merge one at a time,
// each read from pocketd at its turn and handed to ApproveAndMerge with the
// check the confirmation listed; one that fails stays with its reason on
// its card, and the rest go on (ADR-0031).
func TestMergeAllReadyMergesOneAtATimeAndGoesOn(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33)}
	h.api.results = map[string]github.Result{refOf(32).String(): {Kind: "refused", Step: "read", Reason: "a review requests changes"}}
	m := confirmAll(t, h, rows)
	reads := newPocketdReads(h, rows...)
	h.gh.ConfirmMergeAll(m.ID, reads.read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if !slices.Equal(reads.ids, []int64{31, 32, 33}) || !slices.Equal(reads.intents, []int{0, 1, 2}) {
		t.Fatalf("read %v with %v approve and merges begun: not one at a time", reads.ids, reads.intents)
	}
	if c := m.Counts(); c != (MergeAllCounts{Merged: 2, Stayed: 1}) || m.End != "" {
		t.Fatalf("counts %+v, end %q", c, m.End)
	}
	if r := m.Items[1].Result; r.Kind != "refused" || r.Reason != "a review requests changes" {
		t.Fatalf("the failed one %+v", r)
	}
	if card := h.gh.Merge(refOf(32)); card.Phase != MergeDone || card.Result.Reason != "a review requests changes" {
		t.Fatalf("the failed card %+v", card)
	}
	for i, in := range h.api.intents {
		if in.Mode != "single" || in.Preview.Ref != refOf(31+i) || in.CrossReview != CrossReviewOf(rows[i]) {
			t.Fatalf("intent %d %+v", i, in)
		}
	}
	h.gh.DismissMergeAll()
	if h.gh.MergeAll().Phase != MergeAllNone {
		t.Fatal("dismissed, the run stays")
	}
}

// Each turn reads pocketd fresh: a pull request pocketd no longer backs,
// one it no longer lists, and one it cannot read now are refused before any
// request goes to GitHub, with their reasons on their cards, and the rest
// go on (ADR-0031).
func TestMergeAllReadyReadsEachFromPocketdAtItsTurn(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33), backedPull(34)}
	m := confirmAll(t, h, rows)
	reads := newPocketdReads(h, rows...)
	replaced := backedPull(31)
	r := *replaced.Review
	r.Verdict = proto.ReviewNotReady
	replaced.Review = &r
	reads.rows[31] = replaced
	delete(reads.rows, 32)
	reads.errs[33] = errors.New("pocketd is restarting")
	h.gh.ConfirmMergeAll(m.ID, reads.read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if len(h.api.intents) != 1 || h.api.intents[0].Preview.Ref != refOf(34) {
		t.Fatalf("intents %+v", h.api.intents)
	}
	for i, want := range []string{"no ready cross-review with green CI", "pocketd no longer lists it", "pocketd is restarting"} {
		if r := m.Items[i].Result; r.Kind != "refused" || !strings.Contains(r.Reason, want) {
			t.Errorf("item %d %+v, want %q", i, r, want)
		}
		if card := h.gh.Merge(refOf(31 + i)); card.Phase != MergeDone || !strings.Contains(card.Result.Reason, want) {
			t.Errorf("card %d %+v, want %q", i, card, want)
		}
	}
	if m.Items[3].Result.Kind != "merged" {
		t.Fatalf("the last %+v", m.Items[3].Result)
	}
}

// Stop ends the run once the merge under way ended: the rest are not tried
// (ADR-0031).
func TestMergeAllStopEndsAfterTheMergeUnderWay(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33)}
	m := confirmAll(t, h, rows)
	h.api.actWait = make(chan struct{})
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, h.gh.Merging)
	h.gh.StopMergeAll()
	if !h.gh.MergeAll().Stopping {
		t.Fatal("not stopping")
	}
	close(h.api.actWait)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if c := m.Counts(); c != (MergeAllCounts{Merged: 1, Untried: 2}) || !strings.Contains(m.End, "stopped") {
		t.Fatalf("counts %+v, end %q", c, m.End)
	}
	if len(h.api.intents) != 1 {
		t.Fatalf("intents %+v", h.api.intents)
	}

	// Stopped while its checks run, it ends at once, and lists nothing.
	h.api.previewWait = make(chan struct{})
	h.gh.MergeAllReady(func() []proto.PullRequest { return []proto.PullRequest{backedPull(35)} })
	h.gh.StopMergeAll()
	close(h.api.previewWait)
	h.loop.Flush()
	if m := h.gh.MergeAll(); m.Phase != MergeAllEnded || len(m.Items) != 0 || !strings.Contains(m.End, "stopped") {
		t.Fatalf("stopped while checking: %+v", m)
	}
}

// A new GitHub sign-in, or a sign-out, ends the run: before the merges,
// nothing merges, and between them the rest are not tried (ADR-0031).
func TestMergeAllEndsWhenTheSignInChanges(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := confirmAll(t, h, rows)
	signOut := func() {
		t.Helper()
		out := false
		h.gh.SignOut(func(error) { out = true })
		h.loop.RunUntil(t, func() bool { return out })
	}
	signOut()
	if m := h.gh.MergeAll(); m.Phase != MergeAllEnded || !strings.Contains(m.End, "sign-in changed") {
		t.Fatalf("signed out at the confirmation: %+v", m)
	}
	reads := newPocketdReads(h, rows...)
	h.gh.ConfirmMergeAll(m.ID, reads.read)
	if len(reads.ids) != 0 {
		t.Fatal("a confirmation after the sign-out read pocketd")
	}

	// Between the merges: the sign-out comes while pocketd answers the
	// first turn's read.
	h.signIn(t)
	m = confirmAll(t, h, rows)
	reads = newPocketdReads(h, rows...)
	reads.hold[31] = make(chan struct{})
	h.gh.ConfirmMergeAll(m.ID, reads.read)
	signOut()
	close(reads.hold[31])
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if c := m.Counts(); c != (MergeAllCounts{Untried: 2}) || !strings.Contains(m.End, "sign-in changed") || len(h.api.intents) != 0 {
		t.Fatalf("counts %+v, end %q, intents %+v", c, m.End, h.api.intents)
	}
}

// A confirmation holds for its own run, and for the checks it listed: a
// pull request checked again since is refused at its turn, and its card
// keeps the newer check (ADR-0031; Codex review of #24, round 7).
func TestAMergeAllConfirmationHoldsForItsRunAndItsChecks(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := confirmAll(t, h, rows)
	reads := newPocketdReads(h, rows...)
	h.gh.ConfirmMergeAll(m.ID+1, reads.read)
	if h.gh.MergeAll().Phase != MergeAllConfirming || len(reads.ids) != 0 {
		t.Fatal("another run's confirmation went")
	}
	h.gh.CancelMergeAll(m.ID)
	if h.gh.MergeAll().Phase != MergeAllNone {
		t.Fatal("a canceled list stays")
	}

	m = confirmAll(t, h, rows)
	h.gh.Check(backedPull(31))
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(refOf(31)).Phase == MergeChecked })
	newer := h.gh.Merge(refOf(31)).CheckID
	h.gh.ConfirmMergeAll(m.ID, reads.read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if r := m.Items[0].Result; r.Kind != "refused" || !strings.Contains(r.Reason, "checked again since you confirmed") {
		t.Fatalf("the re-checked one %+v", r)
	}
	if card := h.gh.Merge(refOf(31)); card.Phase != MergeChecked || card.CheckID != newer {
		t.Fatalf("the re-checked card %+v", card)
	}
	if len(h.api.intents) != 1 || h.api.intents[0].Preview.Ref != refOf(32) {
		t.Fatalf("intents %+v", h.api.intents)
	}
}

// A Check to Merge begun before pocketd's verdict came counts no
// cross-review: a run begun while it reads waits for it, then reads the
// gates again with the row's verdict before it lists what passes (Codex
// review of #53, round 1).
func TestMergeAllReadyChecksAgainAPreviewOfAnOlderVerdict(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.api.needsVerdict = true
	before := backedPull(31)
	before.Review = nil // no verdict yet
	held := make(chan struct{})
	h.api.previewWait = held
	h.gh.Check(before)
	h.loop.RunUntil(t, func() bool { h.api.mu.Lock(); defer h.api.mu.Unlock(); return len(h.api.crs[refOf(31).String()]) == 1 })
	now := backedPull(31)
	h.gh.MergeAllReady(func() []proto.PullRequest { return []proto.PullRequest{now} })
	h.api.mu.Lock()
	h.api.previewWait = nil // the next preview answers at once
	h.api.mu.Unlock()
	close(held)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase != MergeAllChecking })
	m := h.gh.MergeAll()
	if m.Phase != MergeAllConfirming || !slices.Equal(itemRefs(m), []github.Ref{refOf(31)}) {
		t.Fatalf("merge all %+v", m)
	}
	h.api.mu.Lock()
	crs := slices.Clone(h.api.crs[refOf(31).String()])
	h.api.mu.Unlock()
	if len(crs) != 2 || crs[0] != (github.CrossReview{}) || crs[1] != CrossReviewOf(now) {
		t.Fatalf("previews counted %+v", crs)
	}
}

// pocketd's sign-in changing ends the run: at once while its turn reads
// pocketd, whose answer the store drops, and once the merge under way on
// GitHub ended, whose end is recorded (Codex review of #53, round 1).
func TestMergeAllEndsWithPocketdsSignIn(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33)}
	m := confirmAll(t, h, rows)
	h.gh.ConfirmMergeAll(m.ID, dropped)
	h.gh.PocketdSignInChanged()
	// Ended, and gone from view with the sign-in (round 3): a new run can
	// begin.
	if m := h.gh.MergeAll(); m.Phase != MergeAllNone {
		t.Fatalf("while its turn read pocketd: %+v", m)
	}
	if ok, why := h.gh.CanMergeAll(rows); !ok {
		t.Fatalf("no new run: %q", why)
	}

	m = confirmAll(t, h, rows)
	h.api.actWait = make(chan struct{})
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, h.gh.Merging)
	h.gh.PocketdSignInChanged()
	// Gone from view at once, the merge under way included: the next
	// sign-in's Inbox never shows the old service's pull requests (Codex
	// review of #53, round 4).
	if m := h.gh.MergeAll(); m.Phase != MergeAllNone || len(m.Items) != 0 {
		t.Fatalf("with a merge under way: %+v", m)
	}
	close(h.api.actWait)
	h.loop.RunUntil(t, func() bool { return !h.gh.Merging() })
	h.loop.Flush()
	// The merge under way ended and its card says how; the run stays gone,
	// and the rest were not tried.
	if card := h.gh.Merge(refOf(31)); card.Phase != MergeDone || card.Result.Kind != "merged" || len(h.api.intents) != 1 {
		t.Fatalf("card %+v, intents %+v", card, h.api.intents)
	}
	if m := h.gh.MergeAll(); m.Phase != MergeAllNone {
		t.Fatalf("after the merge under way: %+v", m)
	}
}

// The store says each time its sign-in changes: a new one, a sign-out, and
// a token pocketd refused (Codex review of #53, round 1).
func TestTheStoreSaysWhenItsSignInChanges(t *testing.T) {
	h := &harness{srv: pocketdtest.New(), loop: newTestLoop(), clock: &fakeClock{}}
	changes := 0
	h.store = New(Options{Post: h.loop.Post, After: h.clock.After, Transport: h.srv.Transport(),
		SignedOut: func(_, reason string) { h.signedOut = reason }, SignInChanged: func() { changes++ }})
	t.Cleanup(h.store.Close)
	h.connect(t)
	if changes != 1 {
		t.Fatalf("after the sign-in: %d", changes)
	}
	h.store.SignOut()
	if changes != 2 {
		t.Fatalf("after the sign-out: %d", changes)
	}
	h.connect(t)
	h.srv.Revoke()
	h.store.Open(1)
	h.loop.RunUntil(t, func() bool { return h.signedOut != "" })
	if changes != 4 {
		t.Fatalf("after pocketd refused the token: %d", changes)
	}
}

// A merge the owner began from a listed pull request's card is not the
// run's: pocketd's sign-in changing while the run's turn reads pocketd ends
// the run at once, though that merge still runs (Codex review of #53,
// round 2).
func TestASignOutEndsTheRunWhateverACardsMergeDoes(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31)}
	m := confirmAll(t, h, rows)
	h.api.actWait = make(chan struct{})
	defer close(h.api.actWait)
	h.gh.ApproveAndMerge(fresh(backedPull(31)), h.gh.Merge(refOf(31)), nil) // from its card
	if !h.gh.Merging() {
		t.Fatal("the card's merge did not start")
	}
	h.gh.ConfirmMergeAll(m.ID, dropped)
	h.gh.PocketdSignInChanged()
	if m := h.gh.MergeAll(); m.Phase != MergeAllNone {
		t.Fatalf("after pocketd's sign-in changed: %+v", m)
	}
}

// Stop wins over a read of pocketd that ends after it: a read that failed
// then is no turn, and the pull request counts as not tried, its card as
// it was (Codex review of #53, round 2).
func TestStopWinsOverAReadThatFailsAfterIt(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := confirmAll(t, h, rows)
	reads := newPocketdReads(h, rows...)
	reads.hold[31] = make(chan struct{})
	reads.errs[31] = errors.New("pocketd timed out")
	h.gh.ConfirmMergeAll(m.ID, reads.read)
	h.gh.StopMergeAll()
	close(reads.hold[31])
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if c := m.Counts(); c != (MergeAllCounts{Untried: 2}) || !strings.Contains(m.End, "stopped") {
		t.Fatalf("counts %+v, end %q", c, m.End)
	}
	if card := h.gh.Merge(refOf(31)); card.Phase != MergeChecked {
		t.Fatalf("the card %+v", card)
	}
}

// A run reads at most mergeAllChecks pull requests' gates at once, so one
// click never goes past GitHub's limit on concurrent requests, and lists
// every one once all were read (Codex review of #53, round 3).
func TestMergeAllReadyReadsABoundedNumberAtOnce(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	var rows []proto.PullRequest
	for n := 1; n <= mergeAllChecks+3; n++ {
		rows = append(rows, backedPull(n))
	}
	held := make(chan struct{})
	h.api.previewWait = held
	h.gh.MergeAllReady(func() []proto.PullRequest { return rows })
	checking := 0
	for _, p := range rows {
		if h.gh.Merge(refOf(int(p.Number))).Phase == MergeChecking {
			checking++
		}
	}
	if checking != mergeAllChecks || h.gh.MergeAll().Checking != len(rows) {
		t.Fatalf("%d reads at once, %d waiting; want %d, %d", checking, h.gh.MergeAll().Checking, mergeAllChecks, len(rows))
	}
	h.api.mu.Lock()
	h.api.previewWait = nil
	h.api.mu.Unlock()
	close(held)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase != MergeAllChecking })
	if m := h.gh.MergeAll(); m.Phase != MergeAllConfirming || len(m.Items) != len(rows) || m.Checked != len(rows) {
		t.Fatalf("merge all %+v", m)
	}
}

// A run belongs to the pocketd sign-in it began under: its summary, and
// the end of one that waited for the merge under way, never show in the
// next sign-in's Inbox (Codex review of #53, round 3).
func TestAMergeAllSummaryGoesWithPocketdsSignIn(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31)}
	m := confirmAll(t, h, rows)
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	h.gh.PocketdSignInChanged()
	if m := h.gh.MergeAll(); m.Phase != MergeAllNone || len(m.Items) != 0 {
		t.Fatalf("an ended run after pocketd's sign-in changed: %+v", m)
	}

	m = confirmAll(t, h, rows)
	h.api.actWait = make(chan struct{})
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, h.gh.Merging)
	h.gh.PocketdSignInChanged()
	close(h.api.actWait)
	h.loop.RunUntil(t, func() bool { return !h.gh.Merging() })
	h.loop.Flush()
	if m := h.gh.MergeAll(); m.Phase != MergeAllNone || len(m.Items) != 0 {
		t.Fatalf("a run that waited for its merge after pocketd's sign-in changed: %+v", m)
	}
	if len(h.api.intents) != 2 {
		t.Fatalf("intents %+v", h.api.intents)
	}
}

// Merge All Ready is off while every pull request pocketd backs is merging
// already, and says so, not that pocketd backs none (Codex review of #53,
// round 3).
func TestMergeAllReadySaysWhenEveryBackedPullIsMerging(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.gh.Check(backedPull(31))
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(refOf(31)).Phase == MergeChecked })
	h.api.actWait = make(chan struct{})
	defer close(h.api.actWait)
	h.gh.ApproveAndMerge(fresh(backedPull(31)), h.gh.Merge(refOf(31)), nil)
	if ok, why := h.gh.CanMergeAll([]proto.PullRequest{backedPull(31)}); ok || why != "every pull request pocketd backs is merging now" {
		t.Fatalf("%v %q", ok, why)
	}
}

// rateLimit is GitHub's answer when the owner's token is rate limited.
var rateLimit = &github.APIError{Status: 403, Message: "API rate limit exceeded",
	RetryAfter: time.Date(2026, 11, 20, 12, 34, 0, 0, time.UTC)}

// A rate limit GitHub puts on the owner's token ends the run: no more
// checks start once a check met it, and no more turns once a merge did,
// so the rest of the list is not tried into the limit (Codex review of
// #53, round 4).
func TestARateLimitEndsTheRun(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	var rows []proto.PullRequest
	for n := 1; n <= mergeAllChecks+1; n++ {
		rows = append(rows, backedPull(n))
	}
	held := make(chan struct{})
	h.api.previewWait = held
	h.gh.MergeAllReady(func() []proto.PullRequest { return rows })
	h.loop.RunUntil(t, func() bool { h.api.mu.Lock(); defer h.api.mu.Unlock(); return len(h.api.crs) == mergeAllChecks })
	h.api.mu.Lock()
	h.api.previewWait, h.api.err = nil, rateLimit
	h.api.mu.Unlock()
	close(held)
	h.loop.RunUntil(t, func() bool {
		for _, p := range rows {
			if h.gh.Merge(refOf(int(p.Number))).Phase == MergeChecking {
				return false
			}
		}
		return true
	})
	m := h.gh.MergeAll()
	h.api.mu.Lock()
	previewed := len(h.api.crs)
	h.api.mu.Unlock()
	if m.Phase != MergeAllEnded || !strings.Contains(m.End, "GitHub limits the owner's requests until 2026-11-20 12:34 UTC") || previewed != mergeAllChecks {
		t.Fatalf("checks: %+v, %d previewed", m, previewed)
	}

	h = newGHHarness(t)
	h.signIn(t)
	rows = []proto.PullRequest{backedPull(31), backedPull(32)}
	h.api.results = map[string]github.Result{refOf(31).String(): {Kind: "failed", Step: "approve", Reason: rateLimit.Error(), Err: rateLimit}}
	m = confirmAll(t, h, rows)
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if c := m.Counts(); c != (MergeAllCounts{Stayed: 1, Untried: 1}) || !strings.Contains(m.End, "GitHub limits the owner's requests") || len(h.api.intents) != 1 {
		t.Fatalf("turns: counts %+v, end %q, intents %d", c, m.End, len(h.api.intents))
	}
}

// A pull request whose check runs already when a run begins is counted
// once, as checking, and read once (Codex review of #53, round 4).
func TestACheckRunningAtTheStartCountsOnce(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	var rows []proto.PullRequest
	for n := 1; n <= mergeAllChecks+1; n++ {
		rows = append(rows, backedPull(n))
	}
	held := make(chan struct{})
	h.api.previewWait = held
	last := rows[len(rows)-1]
	h.gh.Check(last) // from its card
	h.gh.MergeAllReady(func() []proto.PullRequest { return rows })
	if m := h.gh.MergeAll(); m.Checking != len(rows) || m.Checked != 0 {
		t.Fatalf("checking %d, checked %d; want %d, 0", m.Checking, m.Checked, len(rows))
	}
	h.api.mu.Lock()
	h.api.previewWait = nil
	h.api.mu.Unlock()
	close(held)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase != MergeAllChecking })
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	for _, p := range rows {
		if n := len(h.api.crs[refOf(int(p.Number)).String()]); n != 1 {
			t.Errorf("%d read %d times", p.Number, n)
		}
	}
}

// Merge All Ready lists a dependency bot's risk:high pull request without
// its diff opened, and its merge goes with the waiver for Act to check
// again; an agents' App's risk:high pull request is not listed until its
// diff was opened; and a card's merge of the dependency bot's still needs
// the diff (ADR-0031, the owner's choice of 2026-11-21).
func TestMergeAllWaivesOpenDiffForADependencyBotOnly(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	risky := github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true, RiskHigh: true}
	bot, agent := risky, risky
	bot.DependencyBot = true // Dependabot's, as GitHub read its author
	h.api.previews = map[string]github.Preview{refOf(31).String(): bot, refOf(32).String(): agent}
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}

	m := confirmAll(t, h, rows)
	if got := itemRefs(m); !slices.Equal(got, []github.Ref{refOf(31)}) || !m.Items[0].Waived {
		t.Fatalf("listed %v, %+v", got, m.Items)
	}
	if ok, why := h.gh.CanMerge(backedPull(31)); ok || !strings.Contains(why, "open its diff first") {
		t.Fatalf("a card's merge of Dependabot's: %v %q", ok, why)
	}
	if ok, why := h.gh.CanMerge(backedPull(32)); ok || !strings.Contains(why, "open its diff first") {
		t.Fatalf("a card's merge of the agents' App's: %v %q", ok, why)
	}
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	if len(h.api.intents) != 1 || h.api.intents[0].Preview.Ref != refOf(31) || !h.api.intents[0].WaiveDiff || h.api.intents[0].AckHead != "" {
		t.Fatalf("intents %+v", h.api.intents)
	}
	h.gh.DismissMergeAll()

	// A card's merge of Dependabot's, checked again: no waiver.
	h.gh.Check(backedPull(31))
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(refOf(31)).Phase == MergeChecked })
	var res github.Result
	h.gh.ApproveAndMerge(fresh(backedPull(31)), h.gh.Merge(refOf(31)), func(r github.Result) { res = r })
	if res.Kind != "refused" || !strings.Contains(res.Reason, "open its diff first") || len(h.api.intents) != 1 {
		t.Fatalf("a card's merge of Dependabot's: %+v, intents %d", res, len(h.api.intents))
	}

	// The agents' App's, once its diff was opened, is listed, with no
	// waiver.
	h.gh.Check(backedPull(32))
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(refOf(32)).Phase == MergeChecked })
	h.gh.OpenedDiff(refOf(32), baseX, headX)
	m = confirmAll(t, h, []proto.PullRequest{backedPull(32)})
	if got := itemRefs(m); !slices.Equal(got, []github.Ref{refOf(32)}) || m.Items[0].Waived {
		t.Fatalf("after its diff: %v, %+v", got, m.Items)
	}
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, backedPull(32)).read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	if last := h.api.intents[len(h.api.intents)-1]; last.Preview.Ref != refOf(32) || last.WaiveDiff || last.AckHead != headX {
		t.Fatalf("the agents' App's intent %+v", last)
	}
}

// A run dropped by pocketd's sign-in while a merge is under way on GitHub
// logs that merge apart from those never tried: GitHub may have merged it
// already (Codex review of #53, round 5).
func TestADroppedRunLogsTheMergeUnderWay(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := confirmAll(t, h, rows)
	h.api.actWait = make(chan struct{})
	defer close(h.api.actWait)
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, h.gh.Merging)
	h.gh.PocketdSignInChanged()
	last := h.logs[len(h.logs)-1]
	if !strings.Contains(last, "1 under way on GitHub (frostyard/nsl#31)") || !strings.Contains(last, "1 not tried") {
		t.Fatalf("the log says %q", last)
	}
}

// A dependency bot's risk:high pull request whose check names no base
// commit is not listed without its diff: the waived merge would bind to no
// base (Codex review of #53, round 6).
func TestNoWaiverWithoutABaseCommit(t *testing.T) {
	h := newGHHarness(t)
	h.signIn(t)
	h.api.previews = map[string]github.Preview{refOf(31).String(): {Head: headX, BaseRef: "main", Method: "SQUASH", Eligible: true,
		RiskHigh: true, DependencyBot: true}}
	h.gh.MergeAllReady(func() []proto.PullRequest { return []proto.PullRequest{backedPull(31)} })
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase != MergeAllChecking })
	if m := h.gh.MergeAll(); m.Phase != MergeAllEnded || len(m.Items) != 0 {
		t.Fatalf("listed with no base commit: %+v", m)
	}
}
