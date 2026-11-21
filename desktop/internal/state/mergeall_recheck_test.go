package state

import (
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/proto"
)

// botPreview is a dependency bot's risk:high pull request as the gates
// read it: the run lists it without its diff opened (ADR-0031).
var botPreview = github.Preview{Head: headX, BaseRef: "main", BaseOid: baseX, Method: "SQUASH", Eligible: true, RiskHigh: true,
	DependencyBot: true}

// botHarness is a signed-in harness whose GitHub reads every pull request
// as a dependency bot's risk:high one, on the base branches named by keys
// (baseKey), each at baseX until a merge moves it (ADR-0038).
func botHarness(t *testing.T, keys ...string) *ghHarness {
	t.Helper()
	h := newGHHarness(t)
	h.signIn(t)
	h.api.preview = botPreview
	h.api.bases = map[string]string{}
	for _, k := range keys {
		h.api.bases[k] = baseX
	}
	return h
}

// previewsOf is how many times GitHub read ref's gates.
func previewsOf(h *ghHarness, ref github.Ref) int {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	return len(h.api.crs[ref.String()])
}

// runAll confirms run m with pocketd's rows and runs it to its end.
func runAll(t *testing.T, h *ghHarness, m MergeAll, reads *pocketdReads) MergeAll {
	t.Helper()
	h.gh.ConfirmMergeAll(m.ID, reads.read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	return h.gh.MergeAll()
}

// pullIn is frostyard/<name>#n as pocketd links it, with id: open, with a
// ready verdict and green CI at headX on main.
func pullIn(name string, n int, id int64) proto.PullRequest {
	p := pullOf(github.Ref{Owner: "frostyard", Name: name, Number: n})
	p.ID = id
	return p
}

// Two dependency bots' pull requests into one base both merge in one run:
// the first merge moves the base, and the second, whose waived merge is
// bound to the base its check read, is checked again on GitHub at the new
// base at its turn, then read from pocketd, then merged there (ADR-0038,
// the owner's choice of 2026-11-21).
func TestMergeAllChecksABotsPullAgainOnceAMergeMovesItsBase(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := confirmAll(t, h, rows)
	if !m.Items[0].Waived || !m.Items[1].Waived {
		t.Fatalf("not waived: %+v", m.Items)
	}
	reads := newPocketdReads(h, rows...)
	m = runAll(t, h, m, reads)
	if c := m.Counts(); c != (MergeAllCounts{Merged: 2, Rechecked: 1}) || m.End != "" {
		t.Fatalf("counts %+v, end %q, items %+v", c, m.End, m.Items)
	}
	if m.Items[0].Rechecked || !m.Items[1].Rechecked || m.Items[1].Merge.Preview.BaseOid != movedBase(1) {
		t.Fatalf("items %+v", m.Items)
	}
	in := h.api.intents
	if len(in) != 2 || in[0].Preview.BaseOid != baseX || in[1].Preview.BaseOid != movedBase(1) || !in[1].WaiveDiff || in[1].AckHead != "" {
		t.Fatalf("intents %+v", in)
	}
	// Read on GitHub once more, at the new base, before pocketd's read at
	// its turn; the first is not read again.
	if previewsOf(h, refOf(31)) != 1 || previewsOf(h, refOf(32)) != 2 {
		t.Fatalf("previews: #31 %d, #32 %d", previewsOf(h, refOf(31)), previewsOf(h, refOf(32)))
	}
	if !slices.Equal(reads.ids, []int64{31, 32}) || !slices.Equal(reads.previews, []int{2, 3}) {
		t.Fatalf("pocketd read %v after %v previews", reads.ids, reads.previews)
	}
	if card := h.gh.Merge(refOf(32)); card.Phase != MergeDone || card.Result.Kind != "merged" {
		t.Fatalf("the card %+v", card)
	}
	if last := h.logs[len(h.logs)-1]; last != "Merge all ready: 2 merged, 0 stayed, 0 not tried; 1 checked again at a new base" {
		t.Fatalf("the log says %q", last)
	}
}

// A pull request whose check at the new base fails stays with that
// reason, and the rest go on: one now behind its base on a repository
// whose rules need it up to date (Dependabot rebases its own), and one
// whose head moved since the owner confirmed (ADR-0038).
func TestARecheckThatFailsKeepsItsReason(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	behind, rebased := botPreview, botPreview
	behind.Eligible, behind.Reason = false, "the PR is behind its base"
	rebased.Head = headY
	h.api.onMove = func(string) {
		if h.api.moves == 1 {
			h.api.previews = map[string]github.Preview{refOf(32).String(): behind, refOf(33).String(): rebased}
		}
	}
	rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33), backedPull(34)}
	m := runAll(t, h, confirmAll(t, h, rows), newPocketdReads(h, rows...))
	if c := m.Counts(); c != (MergeAllCounts{Merged: 2, Stayed: 2, Rechecked: 3}) {
		t.Fatalf("counts %+v, items %+v", c, m.Items)
	}
	for i, want := range []string{"the PR is behind its base", "new commit since you confirmed (222222222222): check it again"} {
		if it := m.Items[1+i]; it.Result.Kind != "refused" || it.Result.Reason != want || !it.Rechecked {
			t.Errorf("item %d %+v, want %q", 1+i, it, want)
		}
		// Its card keeps the refusal once the run is dismissed (Codex
		// review of #60, round 1).
		if card := h.gh.Merge(refOf(32 + i)); card.Phase != MergeDone || card.Result.Kind != "refused" || card.Result.Reason != want {
			t.Errorf("the card of #%d %+v, want %q", 32+i, card, want)
		}
	}
	in := h.api.intents
	if len(in) != 2 || in[1].Preview.Ref != refOf(34) || in[1].Preview.BaseOid != movedBase(1) {
		t.Fatalf("intents %+v", in)
	}
}

// Every merge moves the base, and the run still ends: each pull request
// is checked again once, at its own turn, at the base the merge before it
// moved, never more (ADR-0038).
func TestMergeAllEndsWhenEveryMergeMovesTheBase(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	var rows []proto.PullRequest
	for n := 31; n <= 36; n++ {
		rows = append(rows, backedPull(n))
	}
	reads := newPocketdReads(h, rows...)
	m := runAll(t, h, confirmAll(t, h, rows), reads)
	if c := m.Counts(); c != (MergeAllCounts{Merged: 6, Rechecked: 5}) || m.End != "" || h.api.moves != 6 {
		t.Fatalf("counts %+v, end %q, %d moves", c, m.End, h.api.moves)
	}
	if len(h.api.intents) != 6 {
		t.Fatalf("intents %+v", h.api.intents)
	}
	for i, in := range h.api.intents {
		base, previews := movedBase(i), 2
		if i == 0 {
			base, previews = baseX, 1
		}
		if in.Preview.Ref != refOf(31+i) || in.Preview.BaseOid != base {
			t.Errorf("intent %d %+v, want base %s", i, in.Preview, base)
		}
		if n := previewsOf(h, refOf(31+i)); n != previews {
			t.Errorf("#%d read on GitHub %d times, want %d", 31+i, n, previews)
		}
	}
	if !slices.Equal(reads.ids, []int64{31, 32, 33, 34, 35, 36}) {
		t.Fatalf("pocketd read %v", reads.ids)
	}
}

// A merge moves only its own base: a pull request in another repository,
// or into another branch of the same one, is not read again (ADR-0038).
func TestARecheckReadsOnlyTheBaseThatMoved(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main", "frostyard/nsl:release", "frostyard/porter:main")
	release := botPreview
	release.BaseRef = "release"
	h.api.previews = map[string]github.Preview{refOf(33).String(): release}
	porter := pullIn("porter", 53, 53)
	r33 := backedPull(33)
	r33.Base = "release"
	rows := []proto.PullRequest{backedPull(31), porter, r33, backedPull(32)}
	m := runAll(t, h, confirmAll(t, h, rows), newPocketdReads(h, rows...))
	if c := m.Counts(); c != (MergeAllCounts{Merged: 4, Rechecked: 1}) || !m.Items[3].Rechecked {
		t.Fatalf("counts %+v, items %+v", c, m.Items)
	}
	porterRef := github.Ref{Owner: "frostyard", Name: "porter", Number: 53}
	if previewsOf(h, porterRef) != 1 || previewsOf(h, refOf(33)) != 1 || previewsOf(h, refOf(32)) != 2 {
		t.Fatalf("previews: porter#53 %d, nsl#33 %d, nsl#32 %d", previewsOf(h, porterRef), previewsOf(h, refOf(33)), previewsOf(h, refOf(32)))
	}
	if in := h.api.intents; len(in) != 4 || in[1].Preview.BaseOid != baseX || in[2].Preview.BaseOid != baseX || in[3].Preview.BaseOid != movedBase(1) {
		t.Fatalf("intents %+v", in)
	}
}

// A check of the pull request from its card, begun before the merge moved
// its base, is not the run's: the run waits for it to end, then reads its
// own at the new base (ADR-0038).
func TestARecheckWaitsForACheckFromItsCardAndReadsItsOwn(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := confirmAll(t, h, rows)
	h.api.actWait = make(chan struct{})
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, h.gh.Merging)
	held := make(chan struct{})
	h.api.mu.Lock()
	h.api.previewWait = held
	h.api.mu.Unlock()
	h.gh.Check(backedPull(32)) // from its card, at the base before the merge
	h.loop.RunUntil(t, func() bool { return previewsOf(h, refOf(32)) == 2 })
	close(h.api.actWait)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Rechecking })
	if n := previewsOf(h, refOf(32)); n != 2 {
		t.Fatalf("read %d times while its card's check runs", n)
	}
	h.api.mu.Lock()
	h.api.previewWait = nil
	h.api.mu.Unlock()
	close(held)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	m = h.gh.MergeAll()
	if c := m.Counts(); c != (MergeAllCounts{Merged: 2, Rechecked: 1}) || previewsOf(h, refOf(32)) != 3 {
		t.Fatalf("counts %+v, %d previews, items %+v", c, previewsOf(h, refOf(32)), m.Items)
	}
	if in := h.api.intents; len(in) != 2 || in[1].Preview.BaseOid != movedBase(1) {
		t.Fatalf("intents %+v", in)
	}
}

// Stop, a GitHub sign-out, or GitHub's rate limit while the run checks a
// pull request again ends the run once that check ended: it is not tried,
// nor are the rest (ADR-0038).
func TestARecheckEndsTheRunWhenStoppedSignedOutOrLimited(t *testing.T) {
	for _, tc := range []struct {
		name, end string
		during    func(t *testing.T, h *ghHarness)
		limit     bool
	}{
		{name: "stop", end: "you stopped it: the rest were not tried", during: func(_ *testing.T, h *ghHarness) { h.gh.StopMergeAll() }},
		{name: "sign-out", end: "the GitHub sign-in changed: the rest were not tried", during: func(t *testing.T, h *ghHarness) {
			out := false
			h.gh.SignOut(func(error) { out = true })
			h.loop.RunUntil(t, func() bool { return out })
		}},
		{name: "rate limit", end: "GitHub limits the owner's requests until 2026-11-20 12:34 UTC: the rest were not tried", limit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := botHarness(t, "frostyard/nsl:main")
			held := make(chan struct{})
			h.api.onMove = func(string) {
				h.api.previewWait = held
				if tc.limit {
					h.api.err = rateLimit
				}
			}
			rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33)}
			m := confirmAll(t, h, rows)
			h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
			h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Rechecking })
			if tc.during != nil {
				tc.during(t, h)
			}
			close(held)
			h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
			m = h.gh.MergeAll()
			if c := m.Counts(); c.Merged != 1 || c.Untried != 2 || c.Stayed != 0 || m.End != tc.end || len(h.api.intents) != 1 {
				t.Fatalf("counts %+v, end %q, intents %d", c, m.End, len(h.api.intents))
			}
		})
	}
}

// A check read again at a new base holds what the owner confirmed but its
// base commit: the pull request, its head and base branch, the method and
// the queue, risk:high and a dependency bot's authorship. A base is one
// branch of one repository, which GitHub names in any case (ADR-0038).
func TestARecheckHoldsAllTheOwnerConfirmedButTheBaseCommit(t *testing.T) {
	confirmed := botPreview
	confirmed.Ref, confirmed.NodeID = refOf(31), "PR_31"
	moved := confirmed
	moved.BaseOid, moved.Title, moved.Note = movedBase(1), "Bump x/net to 0.48.0", github.PreviewNote
	if why := changedSince(confirmed, moved); why != "" {
		t.Fatalf("only the base commit moved: %q", why)
	}
	for name, change := range map[string]func(p *github.Preview){
		"another pull request": func(p *github.Preview) { p.NodeID = "PR_32" },
		"new commit":           func(p *github.Preview) { p.Head = headY },
		"the base branch":      func(p *github.Preview) { p.BaseRef = "release" },
		"the merge method":     func(p *github.Preview) { p.Method = "MERGE" },
		"the merge queue":      func(p *github.Preview) { p.Queue = true },
		"risk:high":            func(p *github.Preview) { p.RiskHigh = false },
		"dependency bot":       func(p *github.Preview) { p.DependencyBot = false },
	} {
		p := moved
		change(&p)
		if why := changedSince(confirmed, p); why == "" {
			t.Errorf("%s: no reason", name)
		}
	}
	upper := confirmed
	upper.Ref.Owner, upper.Ref.Name = "Frostyard", "NSL"
	if !sameBase(confirmed, upper) {
		t.Error("a repository named in another case is another base")
	}
	other := confirmed
	other.BaseRef = "Main"
	if sameBase(confirmed, other) {
		t.Error("a branch named in another case is the same base")
	}
}

// A check again holds the sign-in's token the confirmation's check read
// with, as ApproveAndMerge does: one read after a refresh of the token is
// refused, and its card keeps why (Codex review of #60, round 1).
func TestARecheckHoldsTheConfirmedSignInToken(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	h.api.onMove = func(string) {
		h.auth.mu.Lock()
		h.auth.leaseGen++ // the token refreshed
		h.auth.mu.Unlock()
	}
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := runAll(t, h, confirmAll(t, h, rows), newPocketdReads(h, rows...))
	want := "the GitHub sign-in changed since the check: check it again"
	if r := m.Items[1].Result; r.Kind != "refused" || r.Reason != want || len(h.api.intents) != 1 {
		t.Fatalf("the second %+v, intents %d", r, len(h.api.intents))
	}
	if card := h.gh.Merge(refOf(32)); card.Phase != MergeDone || card.Result.Reason != want {
		t.Fatalf("its card %+v", card)
	}
}

// Only a merge moves a base: an approval whose end is unknown sent no
// merge, so the next pull request on that branch is not checked again,
// and a merge whose end is unknown may have merged, so it is (Codex review
// of #60, round 1).
func TestAnUnknownApprovalMovesNoBase(t *testing.T) {
	for _, tc := range []struct {
		step    string
		recheck bool
	}{{"reconcile approve", false}, {"reconcile merge", true}} {
		t.Run(tc.step, func(t *testing.T) {
			h := botHarness(t, "frostyard/nsl:main")
			h.api.results = map[string]github.Result{refOf(31).String(): {Kind: "unknown", Step: tc.step, Reason: "may be pending"}}
			rows := []proto.PullRequest{backedPull(31), backedPull(32)}
			m := runAll(t, h, confirmAll(t, h, rows), newPocketdReads(h, rows...))
			previews := 1
			if tc.recheck {
				previews = 2
			}
			if m.Items[1].Rechecked != tc.recheck || previewsOf(h, refOf(32)) != previews || m.Items[1].Result.Kind != "merged" {
				t.Fatalf("after an unknown at %s: %+v, %d previews", tc.step, m.Items[1], previewsOf(h, refOf(32)))
			}
		})
	}
}

// Stopped while it checks a pull request again, the run ends only once
// that check ended, whatever check of another pull request ends first: no
// new run begins while its read is under way (Codex review of #60, round
// 1).
func TestAStoppedRecheckWaitsForItsOwnCheck(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	own, card := make(chan struct{}), make(chan struct{})
	h.api.onMove = func(string) {
		h.api.holds = map[string]chan struct{}{refOf(32).String(): own, refOf(40).String(): card}
	}
	rows := []proto.PullRequest{backedPull(31), backedPull(32)}
	m := confirmAll(t, h, rows)
	h.gh.ConfirmMergeAll(m.ID, newPocketdReads(h, rows...).read)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Rechecking && previewsOf(h, refOf(32)) == 2 })
	h.gh.Check(backedPull(40)) // another pull request's card
	h.gh.StopMergeAll()
	close(card)
	h.loop.RunUntil(t, func() bool { return h.gh.Merge(refOf(40)).Phase == MergeChecked })
	if m := h.gh.MergeAll(); m.Phase != MergeAllMerging || !m.Rechecking {
		t.Fatalf("ended while its check runs: %+v", m)
	}
	if ok, _ := h.gh.CanMergeAll(rows); ok {
		t.Fatal("a new run can begin while its check runs")
	}
	close(own)
	h.loop.RunUntil(t, func() bool { return h.gh.MergeAll().Phase == MergeAllEnded })
	if m := h.gh.MergeAll(); m.End != "you stopped it: the rest were not tried" || m.Counts().Merged != 1 || m.Counts().Untried != 1 {
		t.Fatalf("after its check: %+v", m)
	}
}
