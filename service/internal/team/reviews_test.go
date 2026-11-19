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
