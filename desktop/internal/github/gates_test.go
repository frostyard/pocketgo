package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// factsOf decodes the fake's PR answer as the client does.
func factsOf(t *testing.T, f *fake) PRFacts {
	t.Helper()
	b, _ := json.Marshal(f.data)
	var a rawPRAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	facts, _, err := a.facts()
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func TestCheckAfterApproval(t *testing.T) {
	directRules := RepoRules{Required: []RequiredCheck{{"Test", 15368}}, Bypass: "never"}
	queueRules := RepoRules{Required: []RequiredCheck{{"docs-gate", 15368}}, Queue: true, Bypass: "never", queueMethod: "squash"}
	for _, tc := range []struct {
		name   string
		base   func(*testing.T) *fake
		change func(*fake)
		rules  func(*RepoRules)
		reason string
	}{
		{name: "direct passes", base: direct},
		{name: "queue passes", base: queue},
		{name: "1 login", base: direct, change: func(f *fake) { obj(f.data, "viewer")["login"] = "frostyard-machines" }, reason: "this token is for frostyard-machines"},
		{name: "2 bypass", base: direct, rules: func(r *RepoRules) { r.Bypass = "always" }, reason: "you can bypass"},
		{name: "2 classic", base: direct, rules: func(r *RepoRules) { r.Classic = true }, reason: "classic branch protection"},
		{name: "3 merged", base: direct, change: func(f *fake) { f.pr()["state"] = "MERGED" }, reason: "the PR is merged"},
		{name: "3 draft", base: direct, change: func(f *fake) { f.pr()["isDraft"] = true }, reason: "the PR is a draft"},
		{name: "3 authored", base: direct, change: func(f *fake) { f.pr()["viewerDidAuthor"] = true }, reason: "you authored this PR"},
		{name: "4 label removed", base: direct, change: func(f *fake) { f.pr()["labels"] = labels() }, reason: "no ready-for-human label"},
		{name: "5 conflicting", base: direct, change: func(f *fake) { f.pr()["mergeable"] = "CONFLICTING" }, reason: "the PR has conflicts"},
		{name: "6 required check missing", base: direct, change: func(f *fake) { f.setChecks() }, reason: "required check Test has not run"},
		{name: "6 status for an integration check", base: direct, change: func(f *fake) { f.setChecks(statusCtx("Test", "SUCCESS")) }, reason: "required check Test has not run"},
		{name: "7 check failed", base: direct, change: func(f *fake) {
			f.setChecks(checkRun("Test", "COMPLETED", "SUCCESS", 15368), checkRun("x", "COMPLETED", "CANCELLED", 15368))
		}, reason: "check x: cancelled"},
		{name: "8 changes requested", base: direct, change: func(f *fake) { f.pr()["reviewDecision"] = "CHANGES_REQUESTED" }, reason: "changes are requested"},
		{name: "10 method not allowed", base: direct, change: func(f *fake) { f.repo()["mergeCommitAllowed"] = false }, reason: "not allowed here"},
		{name: "10 queue with no readable method", base: queue, rules: func(r *RepoRules) { r.queueMethod = "" }, reason: "names no method"},
		{name: "10 queue flag on the PR, no rule", base: direct, change: func(f *fake) { f.pr()["isMergeQueueEnabled"] = true }, reason: "names no method"},
		{name: "no approval", base: direct, change: func(f *fake) { f.pr()["viewerLatestReview"] = nil }, reason: "your approval at 3721d27 is not there"},
		{name: "approval at another head", base: direct, change: func(f *fake) { obj(f.pr(), "viewerLatestReview", "commit")["oid"] = newHead }, reason: "is not there"},
		{name: "commented, not approved", base: direct, change: func(f *fake) { obj(f.pr(), "viewerLatestReview")["state"] = "COMMENTED" }, reason: "is not there"},
		{name: "review decision required", base: direct, change: func(f *fake) { f.pr()["reviewDecision"] = "REVIEW_REQUIRED" }, reason: "the review decision is review required"},
		{name: "review decision null", base: direct, change: func(f *fake) { f.pr()["reviewDecision"] = nil }},
		{name: "direct BLOCKED", base: direct, change: func(f *fake) { f.pr()["mergeStateStatus"] = "BLOCKED" }, reason: "GitHub still blocks the merge"},
		{name: "direct BEHIND", base: direct, change: func(f *fake) { f.pr()["mergeStateStatus"] = "BEHIND" }, reason: "behind"},
		{name: "direct UNSTABLE", base: direct, change: func(f *fake) { f.pr()["mergeStateStatus"] = "UNSTABLE" }, reason: "a check is failing"},
		{name: "direct HAS_HOOKS", base: direct, change: func(f *fake) { f.pr()["mergeStateStatus"] = "HAS_HOOKS" }},
		{name: "queue BLOCKED", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "BLOCKED" }},
		{name: "queue BEHIND", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "BEHIND" }},
		{name: "queue DIRTY", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "DIRTY" }, reason: "conflicts"},
		{name: "queue DRAFT", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "DRAFT" }, reason: "draft"},
		{name: "queue UNKNOWN", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "UNKNOWN" }, reason: "not computed"},
		{name: "queue CLEAN", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "CLEAN" }},
		{name: "queue HAS_HOOKS", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "HAS_HOOKS" }},
		// pocketgo ADR-0018, Codex review of #24, round 8: Check Again never
		// offers a second enqueue.
		{name: "queue already queued", base: queue, change: func(f *fake) { f.pr()["isInMergeQueue"] = true }, reason: "already in the merge queue"},
		{name: "queue UNSTABLE", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "UNSTABLE" }, reason: "a check is failing"},
		{name: "queue unknown state", base: queue, change: func(f *fake) { f.pr()["mergeStateStatus"] = "FUTURE_STATE" }, reason: "the merge state is FUTURE_STATE"},
		{name: "direct unknown state", base: direct, change: func(f *fake) { f.pr()["mergeStateStatus"] = "FUTURE_STATE" }, reason: "the merge state is FUTURE_STATE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.base(t)
			approvedBefore(f)
			if tc.change != nil {
				tc.change(f)
			}
			r := directRules
			if f.ref.Name == "core" {
				r = queueRules
			}
			if tc.rules != nil {
				tc.rules(&r)
			}
			ok, why := Check(factsOf(t, f), r, AfterApproval)
			if ok != (tc.reason == "") || !strings.Contains(why, tc.reason) {
				t.Fatalf("ok %v, %q; want %q", ok, why, tc.reason)
			}
		})
	}
}

func TestCheckIntent(t *testing.T) {
	rules := RepoRules{Required: []RequiredCheck{{"Test", 15368}}, Bypass: "never"}
	for _, tc := range []struct {
		name   string
		change func(*fake)
		intent func(*Intent)
		reason string
		// tip is the base branch's commit the rules read; "" is the
		// preview's base.
		tip string
	}{
		{name: "same"},
		{name: "another PR", intent: func(in *Intent) { in.Preview.NodeID = "PR_other" }, reason: "not the PR you confirmed"},
		{name: "new head", change: func(f *fake) { f.setHead(newHead) }, reason: "new commit since you confirmed (eeeeeee)"},
		{name: "new base", change: func(f *fake) { f.pr()["baseRefName"] = "next" }, reason: "the base branch changed to next"},
		{name: "new method", change: func(f *fake) { f.repo()["viewerDefaultMergeMethod"] = "REBASE" }, reason: "the merge method changed"},
		{name: "method not allowed", change: func(f *fake) { f.repo()["mergeCommitAllowed"] = false }, reason: "the merge method changed"},
		{name: "queue now", change: func(f *fake) { f.pr()["isMergeQueueEnabled"] = true }, reason: "the base has a merge queue now"},
		{name: "risk added, single", change: func(f *fake) { f.pr()["labels"] = labels(labelReady, labelRisk) }, intent: func(in *Intent) { in.AckHead = in.Preview.Head }, reason: "risk:high was added"},
		{name: "risk added, batch", change: func(f *fake) { f.pr()["labels"] = labels(labelReady, labelRisk) }, intent: func(in *Intent) { in.Mode = "batch" }, reason: "risk:high was added"},
		{name: "risk, batch", change: func(f *fake) { f.pr()["labels"] = labels(labelReady, labelRisk) }, intent: func(in *Intent) {
			in.Preview.RiskHigh, in.Mode, in.AckHead = true, "batch", in.Preview.Head
		}, reason: "never in a batch"},
		{name: "risk, single, no diff", change: func(f *fake) { f.pr()["labels"] = labels(labelReady, labelRisk) }, intent: func(in *Intent) { in.Preview.RiskHigh = true }, reason: "open the diff"},
		{name: "risk, single, diff at the head", change: func(f *fake) { f.pr()["labels"] = labels(labelReady, labelRisk) }, intent: func(in *Intent) {
			in.Preview.RiskHigh, in.AckHead = true, in.Preview.Head
		}},
		// The preview's risk:high binds the diff, also once the label went
		// (Codex review of #24, round 6).
		{name: "risk at the preview, gone now, no diff", intent: func(in *Intent) { in.Preview.RiskHigh = true }, reason: "open the diff"},
		{name: "risk at the preview, gone now, the diff opened", intent: func(in *Intent) { in.Preview.RiskHigh, in.AckHead = true, in.Preview.Head }},
		{name: "risk at the preview, gone now, the base moved", intent: func(in *Intent) { in.Preview.RiskHigh, in.AckHead = true, in.Preview.Head },
			tip: newHead, reason: "the base branch is at"},
		// pocketgo ADR-0018: the diff compares the base commit to the head,
		// and label names ignore case.
		{name: "risk, base moved since the diff", change: func(f *fake) {
			f.pr()["labels"] = labels(labelReady, labelRisk)
			f.pr()["baseRefOid"] = newHead
		}, intent: func(in *Intent) { in.Preview.RiskHigh, in.AckHead = true, in.Preview.Head }, reason: "the base moved since you opened the diff"},
		{name: "risk, the branch tip moved between the reads", change: func(f *fake) { f.pr()["labels"] = labels(labelReady, labelRisk) },
			intent: func(in *Intent) { in.Preview.RiskHigh, in.AckHead = true, in.Preview.Head }, tip: newHead, reason: "the base branch is at"},
		{name: "Risk:High in another case", change: func(f *fake) { f.pr()["labels"] = labels(labelReady, "Risk:High") }, intent: func(in *Intent) { in.AckHead = in.Preview.Head }, reason: "risk:high was added"},
		{name: "unknown mode", intent: func(in *Intent) { in.Mode = "all" }, reason: "unknown mode all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := direct(t)
			p := Preview{Ref: f.ref, NodeID: f.pr()["id"].(string), BaseRef: "main", BaseOid: f.pr()["baseRefOid"].(string), Head: f.head(),
				Method: "merge", Eligible: true}
			if tc.change != nil {
				tc.change(f)
			}
			in := Intent{Preview: p, Mode: "single"}
			if tc.intent != nil {
				tc.intent(&in)
			}
			r := rules
			r.baseTip = p.BaseOid
			if tc.tip != "" {
				r.baseTip = tc.tip
			}
			ok, why := CheckIntent(in, factsOf(t, f), r)
			if ok != (tc.reason == "") || !strings.Contains(why, tc.reason) {
				t.Fatalf("ok %v, %q; want %q", ok, why, tc.reason)
			}
		})
	}
}

func TestRealWaitHonorsTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(testToken).wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// Each pull_request rule's approvals count on their own: two rules, one of
// two approvals and one of one plus an extra, need two for a bot's pull
// request, not three (pocketgo ADR-0018, Codex review of #24, round 4).
func TestApprovalsNeededPerRule(t *testing.T) {
	two := ApprovalRule{Count: 2}
	oneExtra := ApprovalRule{Count: 1, Extra: true}
	for _, c := range []struct {
		name   string
		p      BypassRules
		author string
		want   int
	}{
		{"two rules, a bot", BypassRules{ApprovalRules: []ApprovalRule{two, oneExtra}}, "Bot", 2},
		{"two rules, a user", BypassRules{ApprovalRules: []ApprovalRule{two, oneExtra}}, "User", 2},
		{"one rule with the extra, a bot", BypassRules{ApprovalRules: []ApprovalRule{{Count: 2, Extra: true}}}, "Bot", 3},
		{"no count, the extra alone", BypassRules{ApprovalRules: []ApprovalRule{{Extra: true}}}, "Bot", 0},
		{"built without rules", BypassRules{Approvals: 1, ExtraUnattributed: true}, "Bot", 2},
	} {
		if got := approvalsNeeded(c.p, c.author); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// After the approval, the bypass read must show the owner's approval at
// the head: one dismissed between the reads is none (Codex review of #24,
// round 6).
func TestTheLaterReadMustShowTheOwnersApproval(t *testing.T) {
	head := strings.Repeat("a", 40)
	f := PRFacts{Viewer: Login, HeadRefOid: head, Bypass: &BypassFacts{Head: head, Approvers: []string{"someone"}}}
	r := RepoRules{Bypass: "always"}
	if why := bypassGate(f, r, AfterApproval); !strings.Contains(why, "your approval is not at the head") {
		t.Fatalf("after the approval, without it: %q", why)
	}
	f.Bypass.Approvers = append(f.Bypass.Approvers, Login)
	if why := bypassGate(f, r, AfterApproval); why != "" {
		t.Fatalf("after the approval, with it: %q", why)
	}
	f.Bypass.Approvers = []string{"someone"}
	if why := bypassGate(f, r, AtPreview); why != "" {
		t.Fatalf("at the preview, before the approval: %q", why)
	}
}
