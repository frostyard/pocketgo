package team

import (
	"strings"
	"testing"
)

func TestPullRepository(t *testing.T) {
	for _, c := range []struct {
		ref        PullRef
		repository string
		want       string
		ok         bool
	}{
		{PullRef{Number: 3}, "frostyard/x", "frostyard/x", true},
		{PullRef{Repository: "Frostyard/X", Number: 3}, "frostyard/x", "frostyard/x", true},
		{PullRef{Repository: "frostyard/y", Number: 3}, "frostyard/x", "", false},
		{PullRef{Number: 3}, "", "", false},
		{PullRef{Repository: "frostyard/y", Number: 3}, "", "frostyard/y", true},
		{PullRef{Number: 0}, "frostyard/x", "", false},
	} {
		got, err := PullRepository(c.ref, c.repository)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("PullRepository(%+v, %q) = %q, %v", c.ref, c.repository, got, err)
		}
	}
}

// A worktree thread's pull request is in its repository, from its branch,
// not from a fork; a home thread's is in a repository the profile names.
func TestCheckPull(t *testing.T) {
	ok := PullFacts{Repository: "frostyard/x", Number: 3, Head: "vera/fix-t1-abc", HeadRepository: "frostyard/x"}
	profile := []string{"frostyard/x", "frostyard/y"}
	if err := CheckPull("frostyard/x", "vera/fix-t1-abc", profile, ok); err != nil {
		t.Errorf("the thread's own: %v", err)
	}
	// The owner took the repository off the profile since: no new pull
	// request of it (Codex review of #20, round 1).
	if err := CheckPull("frostyard/x", "vera/fix-t1-abc", []string{"frostyard/y"}, ok); err == nil || !strings.Contains(err.Error(), "does not name") {
		t.Errorf("a repository off the profile: %v", err)
	}
	for name, p := range map[string]PullFacts{
		"another repository": {Repository: "frostyard/y", Number: 3, Head: "vera/fix-t1-abc", HeadRepository: "frostyard/y"},
		"another branch":     {Repository: "frostyard/x", Number: 3, Head: "main", HeadRepository: "frostyard/x"},
		"a fork":             {Repository: "frostyard/x", Number: 3, Head: "vera/fix-t1-abc", HeadRepository: "evil/x"},
		"a gone fork":        {Repository: "frostyard/x", Number: 3, Head: "vera/fix-t1-abc"},
	} {
		if err := CheckPull("frostyard/x", "vera/fix-t1-abc", profile, p); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	home := PullFacts{Repository: "Frostyard/Hotedge", Number: 1, Head: "anything", HeadRepository: "someone/hotedge"}
	if err := CheckPull("", "", []string{"frostyard/hotedge"}, home); err != nil {
		t.Errorf("a home thread's: %v", err)
	}
	if err := CheckPull("", "", []string{"frostyard/nsl"}, home); err == nil || !strings.Contains(err.Error(), "does not name") {
		t.Errorf("a repository the profile does not name: %v", err)
	}
}

func TestCheckPullRefs(t *testing.T) {
	if err := CheckPullRefs([]PullRef{{Number: 1}, {Number: 2}}, "frostyard/x"); err != nil {
		t.Error(err)
	}
	if err := CheckPullRefs([]PullRef{{Number: 1}, {Repository: "FROSTYARD/x", Number: 1}}, "frostyard/x"); err == nil {
		t.Error("a pull request named twice was taken")
	}
	many := make([]PullRef, MaxPulls+1)
	for i := range many {
		many[i] = PullRef{Number: int64(i + 1)}
	}
	if err := CheckPullRefs(many, "frostyard/x"); err == nil {
		t.Error("more than MaxPulls were taken")
	}
	if got := ReportText(3, "Vera", StatusDone, "Done.", nil, "frostyard/x#4 (open)"); !strings.HasSuffix(got, "\n\nPull requests: frostyard/x#4 (open)") {
		t.Errorf("ReportText: %q", got)
	}
	if got := MergedText([]string{"frostyard/x#4"}); got != "Closed: its pull requests merged (frostyard/x#4)." {
		t.Errorf("MergedText: %q", got)
	}
}
