package state

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/github"
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
}

func (r *pocketdReads) read(id int64, done func(FreshPull, bool, error)) {
	r.ids = append(r.ids, id)
	r.h.api.mu.Lock()
	r.intents = append(r.intents, len(r.h.api.intents))
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
	if merged, stayed, untried := m.Counts(); merged != 2 || stayed != 1 || untried != 0 || m.End != "" {
		t.Fatalf("counts %d %d %d, end %q", merged, stayed, untried, m.End)
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
	if merged, stayed, untried := m.Counts(); merged != 1 || stayed != 0 || untried != 2 || !strings.Contains(m.End, "stopped") {
		t.Fatalf("counts %d %d %d, end %q", merged, stayed, untried, m.End)
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
	if merged, _, untried := m.Counts(); merged != 0 || untried != 2 || !strings.Contains(m.End, "sign-in changed") || len(h.api.intents) != 0 {
		t.Fatalf("counts %d, %d untried, end %q, intents %+v", merged, untried, m.End, h.api.intents)
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
