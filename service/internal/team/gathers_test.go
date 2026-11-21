package team

import (
	"errors"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

func ptrTo[T any](v T) *T { return &v }

// A gather names a skill by its folder, distinct agents and a bounded note
// (ADR-0033).
func TestCheckGather(t *testing.T) {
	for _, g := range []Gather{{Skill: "code-grooming"}, {Skill: "issue_triage", Agents: []string{"duncan", "vera"}, Note: "Look at the CI."}} {
		if err := CheckGather(g); err != nil {
			t.Errorf("%+v: %v", g, err)
		}
	}
	for name, g := range map[string]Gather{
		"no skill":       {},
		"a path":         {Skill: "../x"},
		"a dot":          {Skill: "a.b"},
		"a long name":    {Skill: strings.Repeat("s", 65)},
		"an agent twice": {Skill: "s", Agents: []string{"duncan", "duncan"}},
		"an empty agent": {Skill: "s", Agents: []string{""}},
		"a long note":    {Skill: "s", Note: strings.Repeat("n", MaxGatherNote+1)},
	} {
		if err := CheckGather(g); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// An agent is asked as a delegation's target is, with a home or workspace
// to propose in.
func TestAskable(t *testing.T) {
	ok := func() error { return nil }
	for name, tc := range map[string]struct {
		p    profiles.Profile
		want string
	}{
		"an agent":     {profiles.Profile{ID: "duncan", Role: profiles.RoleAgent, Home: "/h"}, ""},
		"a supervisor": {profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Home: "/h"}, "supervisor"},
		"private":      {profiles.Profile{ID: "p", Role: profiles.RoleAgent, Home: "/h", Private: true}, "private"},
		"no home":      {profiles.Profile{ID: "vera", Role: profiles.RoleAgent, Repositories: map[string]string{"a/b": "/b"}}, "no home or workspace"},
	} {
		err := Askable(tc.p, ok)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if err := Askable(profiles.Profile{ID: "d", Role: profiles.RoleAgent, Home: "/h"}, func() error { return errors.New("no model") }); err == nil {
		t.Error("an agent that cannot start is asked")
	}
}

// A proposal is checked in code: its title, task, stop condition, rounds,
// repository and note (ADR-0033).
func TestProposalFields(t *testing.T) {
	repos := []string{"frostyard/a"}
	add := ProposalFields{Title: ptrTo(" Fix the flaky test "), Task: ptrTo("It times out."), StopWhen: ptrTo("CI is green.")}
	got, err := add.Apply(Proposal{}, repos, true)
	if err != nil || got.Title != "Fix the flaky test" || got.MaxRounds != DefaultShiftRounds || got.Repository != "" {
		t.Fatalf("an add: %+v %v", got, err)
	}
	edited, err := ProposalFields{Repository: ptrTo("frostyard/a"), MaxRounds: ptrTo(5)}.Apply(got, repos, false)
	if err != nil || edited.Repository != "frostyard/a" || edited.MaxRounds != 5 || edited.Title != got.Title {
		t.Fatalf("an edit: %+v %v", edited, err)
	}
	if cleared, err := (ProposalFields{Repository: ptrTo("")}).Apply(edited, repos, false); err != nil || cleared.Repository != "" {
		t.Fatalf("a cleared repository: %+v %v", cleared, err)
	}
	for name, f := range map[string]ProposalFields{
		"no title":      {Task: add.Task, StopWhen: add.StopWhen},
		"a long title":  {Title: ptrTo(strings.Repeat("t", MaxTitle+1)), Task: add.Task, StopWhen: add.StopWhen},
		"no task":       {Title: add.Title, StopWhen: add.StopWhen},
		"a long task":   {Title: add.Title, Task: ptrTo(strings.Repeat("t", MaxTask+1)), StopWhen: add.StopWhen},
		"no stop":       {Title: add.Title, Task: add.Task},
		"a long stop":   {Title: add.Title, Task: add.Task, StopWhen: ptrTo(strings.Repeat("s", MaxProposalStop+1))},
		"no rounds":     {Title: add.Title, Task: add.Task, StopWhen: add.StopWhen, MaxRounds: ptrTo(0)},
		"many rounds":   {Title: add.Title, Task: add.Task, StopWhen: add.StopWhen, MaxRounds: ptrTo(MaxShiftRounds + 1)},
		"another repo":  {Title: add.Title, Task: add.Task, StopWhen: add.StopWhen, Repository: ptrTo("frostyard/b")},
		"a long note":   {Title: add.Title, Task: add.Task, StopWhen: add.StopWhen, Note: ptrTo(strings.Repeat("n", MaxProposalNote+1))},
		"a blank title": {Title: ptrTo("  "), Task: add.Task, StopWhen: add.StopWhen},
	} {
		if _, err := f.Apply(Proposal{}, repos, true); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

func TestCheckProposeAndBoard(t *testing.T) {
	title := ProposalFields{Title: ptrTo("t")}
	for _, c := range []ProposeCall{{Action: "add", ProposalFields: title}, {Action: "edit", Row: 1, Rev: 1, ProposalFields: title}, {Action: "drop", Row: 1, Rev: 2}, {Action: "list"}, {Action: "list", Page: 2}, {Action: "done"}} {
		if err := CheckPropose(c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []ProposeCall{{Action: "add", Row: 1}, {Action: "edit", Row: 1, ProposalFields: title}, {Action: "edit", Row: 1, Rev: 1}, {Action: "drop", Row: 1},
		{Action: "drop", Row: 1, Rev: 1, ProposalFields: title}, {Action: "list", Row: 1}, {Action: "list", Page: -1}, {Action: "done", Page: 1}, {Action: "done", ProposalFields: title}, {Action: "pick"}} {
		if err := CheckPropose(c); err == nil {
			t.Errorf("%+v: taken", c)
		}
	}
	for _, c := range []BoardCall{{Action: "list"}, {Action: "list", Page: 2}, {Action: "read", Row: 3}, {Action: "edit", Row: 3, Rev: 2, ProposalFields: title},
		{Action: "drop", Row: 3, Rev: 2}, {Action: "order", Rows: []int64{3, 1, 2}}} {
		if err := CheckBoard(c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []BoardCall{{Action: "list", Row: 1}, {Action: "read"}, {Action: "read", Row: 1, Page: 1}, {Action: "edit", Row: 3, ProposalFields: title},
		{Action: "drop", Row: 3}, {Action: "order"}, {Action: "order", Rows: []int64{1, 1}}, {Action: "order", Rows: []int64{0}}, {Action: "add", ProposalFields: title}, {Action: "list", Page: -1}} {
		if err := CheckBoard(c); err == nil {
			t.Errorf("%+v: taken", c)
		}
	}
}

// The texts pocketd writes for a gather: the brief, its start's answer and
// the supervisor's one note.
func TestGatherTexts(t *testing.T) {
	brief := ProposalBrief(3, "Odrade", "code-grooming", "Find dead code.\n", "Look at the tests too.", map[string]string{"frostyard/b": "/b", "frostyard/a": "/a"})
	for _, want := range []string{"[G3 · code-grooming] Odrade, the team's supervisor, gathers proposals", "The supervisor's note:\nLook at the tests too.",
		"from the skill \"code-grooming\":\n\nFind dead code.", "- frostyard/a at /a\n- frostyard/b at /b", "team_propose add", "team_propose done", "60 minutes"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, brief)
		}
	}
	if home := ProposalBrief(1, "Odrade", "s", "x", "", nil); !strings.Contains(home, "You have no repository") || strings.Contains(home, "note:") {
		t.Errorf("a brief with no repository:\n%s", home)
	}
	if got := GatherStartedText(4, "s", []string{"duncan", "vera"}); !strings.Contains(got, "Gather G4 started with the skill s: pocketd asks duncan, vera") {
		t.Error(got)
	}
	done := GatherDoneText(4, "s", []AskOutcome{
		{Agent: "duncan", State: AskDone, Proposals: []ProposalTitle{{12, "Fix A"}, {13, "Fix B"}}},
		{Agent: "vera", State: AskDone},
		{Agent: "irulan", State: AskFailed, Why: "its 60 minutes passed"},
	})
	for _, want := range []string{"[G4 · s · proposals in] 2 proposals from 3 agents asked.", "\n#12 duncan: Fix A\n#13 duncan: Fix B", "Nothing to propose: vera.", "No answer: irulan (its 60 minutes passed).", "team_board"} {
		if !strings.Contains(done, want) {
			t.Errorf("the note lacks %q:\n%s", want, done)
		}
	}
	var many []ProposalTitle
	for i := range MaxDoneLines + 5 {
		many = append(many, ProposalTitle{int64(i + 1), "t"})
	}
	if got := GatherDoneText(5, "s", []AskOutcome{{Agent: "d", State: AskDone, Proposals: many}}); strings.Count(got, "\n#") != MaxDoneLines || !strings.Contains(got, "and 5 more") {
		t.Errorf("a long note:\n%s", got)
	}
	if got := GatherDoneText(6, "s", []AskOutcome{{Agent: "d", State: AskDone}}); strings.Contains(got, "team_board") {
		t.Errorf("a note with no proposal names the board:\n%s", got)
	}
}
