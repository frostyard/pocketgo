package github

import (
	"context"
	"strings"
	"testing"
)

// pocketd's ready cross-review at the head and base the gates read counts
// as 1 approval, once, next to the owner's (pocketgo ADR-0031). A Bot's
// pull request under the frostyard org ruleset (1 approval and the extra
// one for an unattributed change) passes with the two. A verdict at another
// head or base, not ready, or missing counts for nothing; a rule that needs
// 3 still refuses 2; a User's pull request needing 1 is unchanged; and
// every other gate still refuses.
func TestAReadyCrossReviewCountsAsOneApproval(t *testing.T) {
	head, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	org := RepoRules{Bypass: "always", Proof: BypassRules{PullRequest: true, ApprovalRules: []ApprovalRule{{Count: 1, Extra: true}}}}
	three := RepoRules{Bypass: "always", Proof: BypassRules{PullRequest: true, ApprovalRules: []ApprovalRule{{Count: 2, Extra: true}}}}
	lastPush := org
	lastPush.Proof.LastPush = true
	threads := org
	threads.Proof.Threads = true
	facts := func(author string, approvers ...string) PRFacts {
		return PRFacts{Viewer: Login, HeadRefOid: head, BaseRefName: "main",
			Bypass: &BypassFacts{Head: head, AuthorType: author, Approvers: approvers, LastPusher: "dependabot[bot]"}}
	}
	ready := CrossReview{Verdict: VerdictReady, Head: head, Base: "main"}
	changes := facts("Bot")
	changes.Bypass.ChangesRequested = true
	pushed := facts("Bot")
	pushed.Bypass.LastPusher = Login
	unresolved := facts("Bot")
	unresolved.Bypass.Unresolved = 1
	for _, c := range []struct {
		name  string
		f     PRFacts
		r     RepoRules
		stage Stage
		cr    CrossReview
		want  string // "" passes
	}{
		{"a bot: the owner's approval to come and a ready verdict at the head", facts("Bot"), org, AtPreview, ready, ""},
		{"a bot, after the approval: the owner's at the head and the verdict", facts("Bot", Login), org, AfterApproval, ready, ""},
		{"a bot with no verdict", facts("Bot"), org, AtPreview, CrossReview{},
			"the rules need 2 approvals at the head; PocketGo counts 1: no ready cross-review by pocketd at aaaaaaa"},
		{"a verdict at another head", facts("Bot"), org, AtPreview, CrossReview{Verdict: VerdictReady, Head: other, Base: "main"},
			"PocketGo counts 1: no ready cross-review"},
		{"a verdict that is not ready", facts("Bot"), org, AtPreview, CrossReview{Verdict: "not-ready", Head: head, Base: "main"},
			"PocketGo counts 1: no ready cross-review"},
		{"an unclear verdict", facts("Bot"), org, AtPreview, CrossReview{Verdict: "unclear", Head: head, Base: "main"},
			"PocketGo counts 1: no ready cross-review"},
		{"a verdict on another base branch", facts("Bot"), org, AtPreview, CrossReview{Verdict: VerdictReady, Head: head, Base: "release"},
			"PocketGo counts 1: no ready cross-review"},
		{"a verdict that names no base branch", facts("Bot"), org, AtPreview, CrossReview{Verdict: VerdictReady, Head: head},
			"PocketGo counts 1: no ready cross-review"},
		{"a rule needing 3 still refuses 2", facts("Bot"), three, AtPreview, ready,
			"the rules need 3 approvals at the head; PocketGo counts 2, pocketd's ready cross-review among them"},
		{"a user needing 1: the owner's approval alone", facts("User"), org, AtPreview, CrossReview{}, ""},
		{"a user needing 1, with the verdict", facts("User"), org, AtPreview, ready, ""},
		{"after the approval, the verdict never stands in for the owner's", facts("Bot", "someone"), org, AfterApproval, ready,
			"your approval is not at the head in the latest read"},
		{"after the approval, the verdict alone", facts("Bot"), org, AfterApproval, ready,
			"PocketGo counts 1, pocketd's ready cross-review among them"},
		{"a review that requests changes still refuses", changes, org, AtPreview, ready, "a review requests changes"},
		{"the owner's last push still refuses", pushed, lastPush, AtPreview, ready, "you pushed last"},
		{"an unresolved thread still refuses", unresolved, threads, AtPreview, ready, "1 review thread is not resolved"},
	} {
		got := bypassGate(c.f, c.r, c.stage, c.cr)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// botPR is bketelsen/pi-pocket-team#238 by a Bot, on a base whose ruleset
// the owner can bypass, with the frostyard org ruleset's pull_request rule:
// 1 approval, the extra one for an unattributed change, the last push's
// approval, and stale reviews dismissed. Dependabot pushed last.
func botPR(t *testing.T) *fake {
	f := direct(t)
	f.rules = []any{rule("pull_request", prParams(map[string]any{
		"required_approving_review_count":                 1,
		"require_extra_approval_for_unattributed_changes": true,
		"require_last_push_approval":                      true,
		"dismiss_stale_reviews_on_push":                   true,
	}))}
	bypassAll(f, "always")
	obj(f.bypassPR, "data", "repository", "pullRequest", "author")["__typename"] = "Bot"
	f.activityRaw = activity(f.head(), "push", "dependabot[bot]")
	return f
}

// A Bot's pull request under the org ruleset merges as the owner with
// pocketd's ready cross-review at its head: the preview counts it, and so
// does each read of Act. Without it the preview refuses and says what is
// missing (pocketgo ADR-0031).
func TestABotsPullRequestMergesWithTheReadyCrossReview(t *testing.T) {
	f := botPR(t)
	c := f.client()
	ready := CrossReview{Verdict: VerdictReady, Head: f.head(), Base: "main"}
	p, err := c.Preview(context.Background(), f.ref, ready)
	if err != nil || !p.Eligible {
		t.Fatalf("with the verdict: %+v, %v", p, err)
	}
	f.reset()
	res := c.Act(context.Background(), Intent{Preview: p, Mode: "single", CrossReview: ready}, "op-bot", nil)
	if res.Kind != "merged" || strings.Join(f.mutations, ",") != approve+","+merge {
		t.Fatalf("with the verdict: %+v, mutations %v", res, f.mutations)
	}

	f = botPR(t)
	p, err = f.client().Preview(context.Background(), f.ref, CrossReview{})
	if err != nil || p.Eligible || !strings.Contains(p.Reason, "the rules need 2 approvals at the head; PocketGo counts 1: no ready cross-review by pocketd at") {
		t.Fatalf("without the verdict: %+v, %v", p, err)
	}
}

// Act counts only the verdict its intent carries, at each read: none, one
// at another head, or one that is not ready refuses at the first read,
// before any mutation, though the preview counted a ready one (pocketgo
// ADR-0031).
func TestActCountsOnlyTheVerdictOfItsIntent(t *testing.T) {
	for name, cr := range map[string]CrossReview{
		"none":         {},
		"another head": {Verdict: VerdictReady, Head: newHead, Base: "main"},
		"not ready":    {Verdict: "not-ready", Base: "main"},
	} {
		f := botPR(t)
		c := f.client()
		ready := CrossReview{Verdict: VerdictReady, Head: f.head(), Base: "main"}
		if cr.Verdict == "not-ready" {
			cr.Head = f.head()
		}
		p, err := c.Preview(context.Background(), f.ref, ready)
		if err != nil || !p.Eligible {
			t.Fatalf("%s: preview %+v, %v", name, p, err)
		}
		f.reset()
		res := c.Act(context.Background(), Intent{Preview: p, Mode: "single", CrossReview: cr}, "op-bot", nil)
		if res.Kind != "refused" || res.Step != "read" || len(f.mutations) != 0 || !strings.Contains(res.Reason, "no ready cross-review") {
			t.Errorf("%s: %+v, mutations %v", name, res, f.mutations)
		}
	}
}
