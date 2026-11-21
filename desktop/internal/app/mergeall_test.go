package app

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// porter53 is Dependabot's pull request, linked to T2, with pocketd's
// ready cross-review and green CI at mergeHead, which the fake gates read.
var porter53 = proto.PullRequest{ID: 53, Thread: 2, Repository: "frostyard/porter", Number: 53,
	Title: "Bump golang.org/x/net from 0.46.0 to 0.47.0", URL: "https://github.com/frostyard/porter/pull/53",
	State: proto.PullOpen, Head: "dependabot/go_modules/golang.org/x/net-0.47.0", Base: "main", HeadSHA: mergeHead,
	Ready: true, Checks: proto.ChecksGreen,
	Review: &proto.PullReview{ID: 52, Thread: 9, Agent: "irulan", Model: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"},
		Family: "openai", Head: mergeHead, Verdict: proto.ReviewReady, Summary: "A minor bump; CI is green."},
	CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)}

// The owner merges every ready pull request from the Inbox at once: Merge
// All Ready reads each one's gates, one confirmation lists them, and each
// is approved and merged in turn with pocketd's ready cross-review counted
// at its head; one that fails stays with its reason (ADR-0031).
func TestMergeAllReadyFromTheInboxSnapshot(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	x.srv.AddPull(porter53)
	x.a.st.Refresh()
	x.until(t, func() bool { return len(x.a.st.Data.PullsToReview()) == 2 })
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.hasAll(t, "Approve and merge every pull request whose gates pass now, one at a time, as bketelsen.", "Merge All Ready")

	x.click(t, "Merge All Ready")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllConfirming })
	x.hasAll(t, "Approve and merge 2 pull requests?", "frostyard/nsl#31", "frostyard/porter#53",
		"Bump golang.org/x/net from 0.46.0 to 0.47.0", "at cccccccccccc into main · squash", "Approve + Merge 2")
	x.snapshot(t, "inbox-merge-all")
	api.mu.Lock()
	n := len(api.intents)
	crs := api.crs[github.Ref{Owner: "frostyard", Name: "porter", Number: 53}.String()]
	api.mu.Unlock()
	if n != 0 {
		t.Fatal("it merged before the owner confirmed")
	}
	if want := (github.CrossReview{Verdict: "ready", Head: mergeHead, Base: "main"}); len(crs) != 1 || crs[0] != want {
		t.Fatalf("the check of porter#53 counted %+v, want %+v", crs, want)
	}

	// frostyard/nsl#31 is refused at its turn; porter#53 still merges.
	api.mu.Lock()
	api.results = map[string]github.Result{"frostyard/nsl#31": {Kind: "refused", Step: "read", Reason: "a review requests changes"}}
	api.mu.Unlock()
	x.click(t, "Approve + Merge 2")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllEnded })
	x.hasAll(t, "Merge All Ready: 1 merged, 1 stayed, 0 not tried", "frostyard/nsl#31: Refused. a review requests changes",
		"Merged as bketelsen.", "Dismiss")
	x.snapshot(t, "inbox-merge-all-ended")
	api.mu.Lock()
	intents := append([]github.Intent(nil), api.intents...)
	api.mu.Unlock()
	if len(intents) != 2 || intents[0].Preview.Ref.String() != "frostyard/nsl#31" || intents[1].Preview.Ref.String() != "frostyard/porter#53" {
		t.Fatalf("intents %+v", intents)
	}
	for _, in := range intents {
		if in.Mode != "single" || in.CrossReview.Verdict != "ready" || in.CrossReview.Head != mergeHead || in.Preview.Head != mergeHead {
			t.Fatalf("intent %+v", in)
		}
	}
	// pocketd read each on GitHub at its turn (GET /v1/pulls/{id}).
	for _, id := range []int64{1, 53} {
		if got := len(x.srv.RequestsTo("GET", proto.Path(proto.PathPull, id))); got != 1 {
			t.Errorf("pull %d read %d times from pocketd at its turn, want 1", id, got)
		}
	}
	x.click(t, "Dismiss")
	if x.tt.HasText("Merge All Ready: 1 merged") {
		t.Error("the summary stays after Dismiss")
	}
}

// A run ends when pocketd's sign-in does: the store drops the answer of a
// read begun under the earlier sign-in, so a turn waiting on it would wait
// forever, and no later turn or confirmation carries over to the next
// sign-in (Codex review of #53, round 1).
func TestMergeAllReadyEndsWhenPocketdSignsOut(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	x.srv.AddPull(porter53)
	x.a.st.Refresh()
	x.until(t, func() bool { return len(x.a.st.Data.PullsToReview()) == 2 })
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.click(t, "Merge All Ready")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllConfirming })
	path := proto.Path(proto.PathPull, 1)
	release := x.srv.HoldAnswerOnce("GET", path)
	x.click(t, "Approve + Merge 2")
	x.until(t, func() bool { return x.srv.Holding("GET", path) })
	x.a.signOut()
	release()
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if m := x.a.gh.MergeAll(); m.Phase != state.MergeAllEnded || !strings.Contains(m.End, "pocketd's sign-in changed") {
		t.Fatalf("after pocketd's sign-out: %+v", m)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.intents) != 0 {
		t.Fatalf("it merged after pocketd's sign-out: %+v", api.intents)
	}
}

// The summary keeps a pull request added to the merge queue, which may yet
// leave it unmerged, and one whose end is unknown, which may have merged,
// apart from those merged and those that stayed (Codex review of #53,
// round 1).
func TestMergeAllReadySaysWhatWasQueuedOrIsUnknown(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	x.srv.AddPull(porter53)
	x.a.st.Refresh()
	x.until(t, func() bool { return len(x.a.st.Data.PullsToReview()) == 2 })
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	api.mu.Lock()
	api.results = map[string]github.Result{
		"frostyard/nsl#31":    {Kind: "unknown", Step: "reconcile merge", Reason: "merge may be pending"},
		"frostyard/porter#53": {Kind: "queued", Step: "merge", Method: "queue"},
	}
	api.mu.Unlock()
	x.click(t, "Merge All Ready")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllConfirming })
	x.click(t, "Approve + Merge 2")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllEnded })
	x.hasAll(t, "Merge All Ready: 0 merged, 1 in the merge queue, 1 to check again, 0 stayed, 0 not tried",
		"frostyard/nsl#31: Its end is unknown: check it again. merge may be pending", "frostyard/porter#53: Added to the merge queue.")
}

// A confirmation pocketd's sign-out came under goes with it, as a single
// one does (Codex review of #53, round 1).
func TestAMergeAllConfirmationGoesWithPocketdsSignIn(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, _ := newMergeApp(t, auth)
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.click(t, "Merge All Ready")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllConfirming })
	x.a.signOut()
	x.settle()
	if m := x.a.gh.MergeAll(); m.Phase == state.MergeAllConfirming || x.tt.HasText("Approve + Merge 1") {
		t.Fatalf("the confirmation stays after pocketd's sign-out: %+v", m)
	}
}

// Merge All Ready is off, and says why, while pocketd backs none of the
// pull requests that wait (ADR-0031).
func TestMergeAllReadyIsOffWithNothingBacked(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, _ := newMergeApp(t, auth)
	var p proto.PullRequest
	for _, q := range x.a.st.Data.Pulls() {
		if q.ID == 1 {
			p = q
		}
	}
	review := *p.Review
	review.Verdict, review.Findings = proto.ReviewNotReady, []string{"A test is missing."}
	p.Review = &review
	x.srv.AddPull(p)
	x.a.st.Refresh()
	x.until(t, func() bool {
		for _, q := range x.a.st.Data.Pulls() {
			if q.ID == 1 && q.Review != nil && q.Review.Verdict == proto.ReviewNotReady {
				return true
			}
		}
		return false
	})
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	b, ok := x.tt.AccessByRole(ui.RoleButton, "Merge All Ready")
	if !ok || !b.Disabled {
		t.Fatalf("Merge All Ready with nothing backed: %+v, %v", b, ok)
	}
	if !strings.Contains(strings.Join(x.tt.Texts(), "\n"), "Merge All Ready: pocketd backs no pull request here now") {
		t.Fatalf("no reason in %q", x.tt.Texts())
	}
}
