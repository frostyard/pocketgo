package team

import (
	"strings"
	"testing"
)

const head = "0123456789abcdef0123456789abcdef01234567"

func TestCheckVerdict(t *testing.T) {
	ok := Verdict{PullRequest: PullRef{Number: 12}, Head: head, Verdict: VerdictReady, Summary: "Tests cover the fix."}
	if err := CheckVerdict(ok); err != nil {
		t.Fatalf("a ready verdict: %v", err)
	}
	notReady := ok
	notReady.Verdict, notReady.Findings = VerdictNotReady, []string{"The retry loop never ends."}
	if err := CheckVerdict(notReady); err != nil {
		t.Fatalf("a not-ready verdict with a finding: %v", err)
	}
	for name, v := range map[string]Verdict{
		"an unknown verdict":         {Head: head, Verdict: "approve", Summary: "x"},
		"a short head":               {Head: "0123456", Verdict: VerdictReady, Summary: "x"},
		"an upper-case head":         {Head: strings.ToUpper(head), Verdict: VerdictReady, Summary: "x"},
		"no summary":                 {Head: head, Verdict: VerdictReady, Summary: " "},
		"a long summary":             {Head: head, Verdict: VerdictReady, Summary: strings.Repeat("é", MaxSummary+1)},
		"an empty finding":           {Head: head, Verdict: VerdictReady, Summary: "x", Findings: []string{" "}},
		"a long finding":             {Head: head, Verdict: VerdictReady, Summary: "x", Findings: []string{strings.Repeat("a", MaxFinding+1)}},
		"too many findings":          {Head: head, Verdict: VerdictUnclear, Summary: "x", Findings: make([]string, MaxFindings+1)},
		"not ready with no findings": {Head: head, Verdict: VerdictNotReady, Summary: "x"},
	} {
		if err := CheckVerdict(v); err == nil {
			t.Errorf("%s is taken", name)
		}
	}
}

// A cross-review comes from another thread, and another family that code
// can tell.
func TestCheckReviewer(t *testing.T) {
	if err := CheckReviewer(3, 4, "anthropic", "openai"); err != nil {
		t.Errorf("another thread of another family: %v", err)
	}
	for name, err := range map[string]error{
		"its own thread":    CheckReviewer(3, 3, "anthropic", "openai"),
		"the same family":   CheckReviewer(3, 4, "openai", "openai"),
		"an unknown one":    CheckReviewer(3, 4, "anthropic", ""),
		"an unknown author": CheckReviewer(3, 4, "", "openai"),
	} {
		if err == nil {
			t.Errorf("%s reviews", name)
		}
	}
}

func TestCheckHead(t *testing.T) {
	if err := CheckHead("frostyard/x#1", head, head); err != nil {
		t.Errorf("the current head: %v", err)
	}
	other := strings.Repeat("f", 40)
	if err := CheckHead("frostyard/x#1", head, other); err == nil || !strings.Contains(err.Error(), "ffffffffffff") {
		t.Errorf("a moved head: %v", err)
	}
}

// The label goes only on an open pull request that is no draft, with a
// ready verdict at its head and green CI.
func TestWantsReady(t *testing.T) {
	if !WantsReady(true, false, VerdictReady, true) {
		t.Error("a ready, green pull request wants no label")
	}
	for name, got := range map[string]bool{
		"closed":     WantsReady(false, false, VerdictReady, true),
		"a draft":    WantsReady(true, true, VerdictReady, true),
		"not ready":  WantsReady(true, false, VerdictNotReady, true),
		"unclear":    WantsReady(true, false, VerdictUnclear, true),
		"no verdict": WantsReady(true, false, "", true),
		"red CI":     WantsReady(true, false, VerdictReady, false),
	} {
		if got {
			t.Errorf("%s wants the label", name)
		}
	}
}

// A verdict names its pull request's repository, else its thread's; a
// reviewer in one repository may review a pull request of another.
func TestVerdictRepository(t *testing.T) {
	for _, c := range []struct {
		ref        PullRef
		repository string
		want       string
	}{
		{PullRef{Number: 3}, "frostyard/a", "frostyard/a"},
		{PullRef{Repository: "frostyard/b", Number: 3}, "frostyard/a", "frostyard/b"},
		{PullRef{Repository: "frostyard/b", Number: 3}, "", "frostyard/b"},
	} {
		if got, err := VerdictRepository(c.ref, c.repository); err != nil || got != c.want {
			t.Errorf("VerdictRepository(%+v, %q) = %q, %v; want %q", c.ref, c.repository, got, err, c.want)
		}
	}
	if _, err := VerdictRepository(PullRef{Number: 3}, ""); err == nil {
		t.Error("a home thread's verdict without a repository is taken")
	}
	if _, err := VerdictRepository(PullRef{Repository: "frostyard/b"}, "frostyard/a"); err == nil {
		t.Error("a verdict on no number is taken")
	}
}

// A review brief names the change set exactly, says the review is
// read-only, and how to file the verdict; a re-review names the last
// review and the change since (ADR-0019).
func TestReviewBrief(t *testing.T) {
	base, head, prior := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	r := ReviewSpec{Thread: 9, Author: "Sheeana", Repository: "frostyard/frostmarks", Number: 12, BaseBranch: "main",
		Base: base, Head: head, Cwd: "/w/review"}
	got := ReviewBrief(r)
	for _, want := range []string{
		"thread T9", "frostyard/frostmarks#12", "Sheeana", "/w/review", "read-only", "team_diff",
		"git diff " + base + " " + head,
		"exactly one team_verdict for frostyard/frostmarks#12 at head " + head,
		"high blocking: the retry never ends",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "re-review") {
		t.Error("a first review is called a re-review")
	}
	r.Prior = &PriorReview{Head: prior, Verdict: VerdictNotReady, Summary: "One test is missing.", Findings: []string{"high blocking: no test"}}
	got = ReviewBrief(r)
	for _, want := range []string{"re-review", prior, "high blocking: no test", "git diff " + prior + " " + head, "since_last_review"} {
		if !strings.Contains(got, want) {
			t.Errorf("the re-review brief lacks %q:\n%s", want, got)
		}
	}
}

// A review is keyed by its pull request and head: one review a head.
func TestReviewKey(t *testing.T) {
	a := ReviewKey("frostyard/frostmarks", 12, strings.Repeat("a", 40))
	if a != ReviewKey("frostyard/frostmarks", 12, strings.Repeat("a", 40)) || a != ReviewKey("Frostyard/FrostMarks", 12, strings.Repeat("a", 40)) {
		t.Fatal("one head gives two keys")
	}
	for _, other := range []string{
		ReviewKey("frostyard/frostmarks", 12, strings.Repeat("b", 40)),
		ReviewKey("frostyard/frostmarks", 13, strings.Repeat("a", 40)),
		ReviewKey("frostyard/frostui", 12, strings.Repeat("a", 40)),
	} {
		if other == a {
			t.Fatalf("two reviews share the key %s", a)
		}
	}
	if MaxReviews != 4 {
		t.Fatalf("the budget is %d reviews, not the owner's 1 and 3 re-reviews", MaxReviews)
	}
}

// The author hears a verdict once, and a verdict that is not ready says
// what to do next; the reviewer of a review thread is not sent to
// team_message, which it does not have.
func TestAuthorVerdictText(t *testing.T) {
	if got := ReviewThreadVerdictText(3, "frostyard/frostmarks#12", strings.Repeat("b", 40), VerdictReady); strings.Contains(got, "team_message") {
		t.Errorf("a review thread's answer names team_message: %s", got)
	}
	head := strings.Repeat("b", 40)
	ready := AuthorVerdictText(9, "frostyard/frostmarks#12", head, VerdictReady, "Looks right.", nil)
	if !strings.Contains(ready, "[T9 · cross-review · ready]") || strings.Contains(ready, "Fix what blocks") {
		t.Errorf("ready: %s", ready)
	}
	not := AuthorVerdictText(9, "frostyard/frostmarks#12", head, VerdictNotReady, "A test is missing.", []string{"high blocking: no test"})
	if !strings.Contains(not, "high blocking: no test") || !strings.Contains(not, "Fix what blocks") {
		t.Errorf("not ready: %s", not)
	}
}

// A review's findings say their severity and whether they block, and agree
// with the verdict (ADR-0019).
func TestCheckReviewFindings(t *testing.T) {
	ok := []Verdict{
		{Verdict: VerdictReady},
		{Verdict: VerdictReady, Findings: []string{"low non-blocking: a name could be clearer"}},
		{Verdict: VerdictNotReady, Findings: []string{"high blocking: no test", "low non-blocking: a typo"}},
		{Verdict: VerdictUnclear, Findings: []string{"medium non-blocking: CI could not be read"}},
	}
	for _, v := range ok {
		if err := CheckReviewFindings(v); err != nil {
			t.Errorf("%+v: %v", v, err)
		}
	}
	bad := map[string]Verdict{
		"no form":               {Verdict: VerdictNotReady, Findings: []string{"no test"}},
		"no blocking word":      {Verdict: VerdictNotReady, Findings: []string{"high: no test"}},
		"not ready, none block": {Verdict: VerdictNotReady, Findings: []string{"low non-blocking: a typo"}},
		"ready, one blocks":     {Verdict: VerdictReady, Findings: []string{"high blocking: no test"}},
		"an unknown severity":   {Verdict: VerdictNotReady, Findings: []string{"critical blocking: no test"}},
		"nothing after":         {Verdict: VerdictNotReady, Findings: []string{"high blocking: "}},
	}
	for name, v := range bad {
		if err := CheckReviewFindings(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A team_diff call reads only paths in the folder, and its git arguments
// let nothing the checkout configures run (ADR-0019).
func TestDiff(t *testing.T) {
	for _, d := range []DiffRequest{{}, {Stat: true}, {Log: true, Paths: []string{"service/x.go", "docs"}}} {
		if err := CheckDiff(d); err != nil {
			t.Errorf("%+v: %v", d, err)
		}
	}
	for name, d := range map[string]DiffRequest{
		"absolute":          {Paths: []string{"/etc/passwd"}},
		"leaves":            {Paths: []string{"a/../../b"}},
		"an option":         {Paths: []string{"--output=/tmp/x"}},
		"magic":             {Paths: []string{":(top)x"}},
		"empty":             {Paths: []string{""}},
		"stat and log":      {Stat: true, Log: true},
		"a negative offset": {Offset: -1},
		"too many":          {Paths: make([]string, MaxDiffPaths+1)},
	} {
		if err := CheckDiff(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	from, to := strings.Repeat("a", 40), strings.Repeat("b", 40)
	args := strings.Join(DiffArgs(from, to, DiffRequest{Paths: []string{"x.go"}}), " ")
	for _, want := range []string{"--no-replace-objects", "core.hooksPath=/dev/null", "core.fsmonitor=false", "diff.external=", "--no-ext-diff", "--no-textconv", from + " " + to + " -- x.go"} {
		if !strings.Contains(args, want) {
			t.Errorf("diff args %q lack %q", args, want)
		}
	}
	logArgs := strings.Join(DiffArgs(from, to, DiffRequest{Log: true}), " ")
	for _, want := range []string{"--no-replace-objects", "log.showSignature=false", "log --no-show-signature", "--no-ext-diff --no-textconv --no-color -p " + from + ".." + to} {
		if !strings.Contains(logArgs, want) {
			t.Errorf("log args %q lack %q", logArgs, want)
		}
	}
	if strings.Contains(logArgs, "%G") {
		t.Errorf("the log's format asks for a signature check: %q", logArgs)
	}
}
