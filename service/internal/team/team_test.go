package team

import (
	"errors"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

var (
	odrade = &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor}
	vera   = profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent,
		Repositories: map[string]string{"frostyard/example": "/src/example", "frostyard/other": "/src/other"}}
)

func TestCheckDelegation(t *testing.T) {
	good := Delegation{Agent: "vera", Task: "Fix the flaky test", Repository: "frostyard/example"}
	if err := CheckDelegation(odrade, vera, nil, good, 3, 12); err != nil {
		t.Fatalf("a good delegation: %v", err)
	}
	home := vera
	home.Home = "/home/vera"
	if err := CheckDelegation(odrade, home, nil, Delegation{Agent: "vera", Task: "Look around"}, 0, 12); err != nil {
		t.Fatalf("a delegation to the home: %v", err)
	}
	chani := vera
	chani.ID, chani.Private = "chani", true
	boss := vera
	boss.ID, boss.Role = "boss", profiles.RoleSupervisor
	agent := &profiles.Profile{ID: "duncan", Role: profiles.RoleAgent}
	for name, tc := range map[string]struct {
		caller    *profiles.Profile
		target    profiles.Profile
		startable func() error
		d         Delegation
		onDesk    int
		want      string
	}{
		"no caller profile":    {nil, vera, nil, good, 0, "only a supervisor"},
		"an agent delegates":   {agent, vera, nil, good, 0, "only a supervisor"},
		"to a private agent":   {odrade, chani, nil, good, 0, "private"},
		"to a supervisor":      {odrade, boss, nil, good, 0, "supervisor and takes no"},
		"to one that can't":    {odrade, vera, func() error { return errors.New("no sign-in") }, good, 0, "no sign-in"},
		"an empty task":        {odrade, vera, nil, Delegation{Agent: "vera", Task: "  "}, 0, "empty"},
		"a long task":          {odrade, vera, nil, Delegation{Agent: "vera", Task: strings.Repeat("x", MaxTask+1)}, 0, "more than"},
		"a long title":         {odrade, vera, nil, Delegation{Agent: "vera", Task: "x", Title: strings.Repeat("t", MaxTitle+1)}, 0, "title"},
		"a full desk":          {odrade, vera, nil, good, 12, "desk is full"},
		"another repository":   {odrade, vera, nil, Delegation{Agent: "vera", Task: "x", Repository: "frostyard/nope"}, 0, "frostyard/example, frostyard/other"},
		"no repository at all": {odrade, profiles.Profile{ID: "nomad"}, nil, Delegation{Agent: "nomad", Task: "x", Repository: "a/b"}, 0, "no repository"},
		"nowhere to work":      {odrade, profiles.Profile{ID: "nomad"}, nil, Delegation{Agent: "nomad", Task: "x"}, 0, "no home, workspace or repository"},
		"no home, no repo":     {odrade, vera, nil, Delegation{Agent: "vera", Task: "x"}, 0, "name one of its repositories, frostyard/example, frostyard/other"},
		// The cap comes last: a full desk never hides another refusal
		// (Codex review of #25, round 6).
		"full, another repo": {odrade, vera, nil, Delegation{Agent: "vera", Task: "x", Repository: "frostyard/nope"}, 12, "does not work on frostyard/nope"},
	} {
		if err := CheckDelegation(tc.caller, tc.target, tc.startable, tc.d, tc.onDesk, 12); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
}

func TestTitleSlugAndWorktree(t *testing.T) {
	if got := Title(Delegation{Title: "  Fix CI  ", Task: "x"}); got != "Fix CI" {
		t.Errorf("given title %q", got)
	}
	if got := Title(Delegation{Task: "Fix the flaky arm64 job\nIt times out."}); got != "Fix the flaky arm64 job" {
		t.Errorf("first line %q", got)
	}
	if got := Title(Delegation{Task: strings.Repeat("word ", 30)}); len([]rune(got)) > 60 || !strings.HasSuffix(got, "…") {
		t.Errorf("long first line %q", got)
	}
	if got := Slug("Fix the flaky arm64 job!", 12, "k3fa9c1d0e2b4f6a"); got != "fix-the-flaky-arm64-job-t12-k3fa9c1d0e2b4f6a" {
		t.Errorf("slug %q", got)
	}
	if got := Slug("¡¿", 3, "abcdef"); got != "task-t3-abcdef" {
		t.Errorf("empty slug %q", got)
	}
	if got := Slug(strings.Repeat("abcdefgh ", 10), 1, "abcdefghijklmnop"); len(got) > 61 {
		t.Errorf("long slug %q", got)
	}
	if Slug("Fix CI", 1, "aaaaaa0000000001") == Slug("Fix CI", 1, "aaaaaa0000000002") {
		t.Error("two owners that share a prefix share a name")
	}
	path, branch := Worktree("/src/example/", "vera", "fix-ci-t12")
	if path != "/src/example--vera-fix-ci-t12" || branch != "vera/fix-ci-t12" {
		t.Errorf("worktree %q %q", path, branch)
	}
}

func TestBrief(t *testing.T) {
	brief := Brief("Odrade", 12, "Fix CI", "Fix the flaky test.", "/src/example--vera-fix-ci-t12", "vera/fix-ci-t12", "")
	for _, want := range []string{"Odrade", "thread T12: Fix CI", "Fix the flaky test.", "branch vera/fix-ci-t12", "team_message", "asks"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if home := Brief("Odrade", 3, "Look", "Look around.", "/home/vera", "", ""); strings.Contains(home, "branch") {
		t.Errorf("a home brief names a branch:\n%s", home)
	}
}

func TestCheckReport(t *testing.T) {
	good := Report{Message: "PR #3 is open.", Status: StatusDone, Asks: []Ask{{Question: "Squash?", Options: []string{"yes", "no"}}, {Question: "Anything else?"}}}
	if err := CheckReport(good); err != nil {
		t.Fatalf("a good report: %v", err)
	}
	five := make([]Ask, 5)
	for i := range five {
		five[i] = Ask{Question: "q"}
	}
	for name, r := range map[string]Report{
		"a bad status":      {Message: "x", Status: "finished"},
		"an empty message":  {Message: " ", Status: StatusDone},
		"a long message":    {Message: strings.Repeat("x", MaxMessage+1), Status: StatusDone},
		"five asks":         {Message: "x", Status: StatusDone, Asks: five},
		"an empty question": {Message: "x", Status: StatusDone, Asks: []Ask{{Question: ""}}},
		"a long question":   {Message: "x", Status: StatusDone, Asks: []Ask{{Question: strings.Repeat("q", MaxQuestion+1)}}},
		"one option":        {Message: "x", Status: StatusDone, Asks: []Ask{{Question: "q", Options: []string{"yes"}}}},
		"seven options":     {Message: "x", Status: StatusDone, Asks: []Ask{{Question: "q", Options: []string{"1", "2", "3", "4", "5", "6", "7"}}}},
		"a long option":     {Message: "x", Status: StatusDone, Asks: []Ask{{Question: "q", Options: []string{"yes", strings.Repeat("o", MaxOption+1)}}}},
		"a repeated option": {Message: "x", Status: StatusDone, Asks: []Ask{{Question: "q", Options: []string{"yes", " yes"}}}},
		"an empty option":   {Message: "x", Status: StatusDone, Asks: []Ask{{Question: "q", Options: []string{"yes", " "}}}},
	} {
		if err := CheckReport(r); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if ThreadState(StatusDone) != "done" || ThreadState(StatusBlocked) != "blocked" || ThreadState(StatusWorking) != "open" {
		t.Error("thread states")
	}
}

func TestTexts(t *testing.T) {
	if got := ReportText(12, "Vera", StatusDone, " PR #3 is open. ", []int64{7, 8}); got != "[T12 · Vera · done] PR #3 is open.\n\nAsks waiting for the owner: A7, A8" {
		t.Errorf("report %q", got)
	}
	if got := ReportText(12, "Vera", StatusWorking, "Halfway.", nil); strings.Contains(got, "Asks") {
		t.Errorf("a report without asks %q", got)
	}
	if got := AnswerText(7, "Squash?", " yes "); got != `The owner answered A7 ("Squash?"): yes` {
		t.Errorf("answer %q", got)
	}
}

func TestRoster(t *testing.T) {
	chani := vera
	chani.ID, chani.Name, chani.Private = "chani", "Chani", true
	duncan := profiles.Profile{ID: "duncan", Name: "Duncan", Role: profiles.RoleAgent, Home: "/home/duncan"}
	clerk := profiles.Profile{ID: "clerk", Name: "Clerk", Role: profiles.RoleClerk}
	cannot := func(p profiles.Profile) error {
		if p.ID == "clerk" {
			return errors.New("no copilot sign-in")
		}
		return nil
	}
	roster := Roster([]profiles.Profile{vera, *odrade, chani, duncan, clerk}, cannot, 12)
	for _, want := range []string{"- duncan (Duncan): works in its home\n- vera (Vera): repositories frostyard/example, frostyard/other", "at most 12 threads", PlanRoster(),
		"a done or blocked thread is open again; a blocked one's asks that wait for the owner are withdrawn", InboxGuide, ThreadsGuide} {
		if !strings.Contains(roster, want) {
			t.Errorf("roster lacks %q:\n%s", want, roster)
		}
	}
	nowhere := profiles.Profile{ID: "nomad", Name: "Nomad", Role: profiles.RoleAgent}
	if strings.Contains(Roster([]profiles.Profile{nowhere}, cannot, 12), "nomad") {
		t.Error("the roster lists an agent with nowhere to work")
	}
	for _, not := range []string{"odrade", "chani", "clerk"} {
		if strings.Contains(roster, "- "+not) {
			t.Errorf("roster lists %s:\n%s", not, roster)
		}
	}
	// The supervisor checks team_inbox before it reminds the owner, and
	// reads a thread before it asks where it stands, with or without
	// agents to delegate to (ADR-0046, ADR-0071).
	if none := Roster(nil, cannot, 3); !strings.Contains(none, "No agent can take a delegation now.") || !strings.Contains(none, InboxGuide) || !strings.Contains(none, ThreadsGuide) {
		t.Errorf("an empty roster:\n%s", none)
	}
}

func TestClip(t *testing.T) {
	if got := Clip("short", 10); got != "short" {
		t.Errorf("short %q", got)
	}
	if got := Clip(strings.Repeat("é", 20), 10); len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Errorf("long %q", got)
	}
}

// A private or supervisor target is refused before its startability is
// looked at: that check may look at folders, which a refused target never
// costs.
func TestTheStaticRulesComeFirst(t *testing.T) {
	looked := false
	look := func() error { looked = true; return nil }
	chani := vera
	chani.Private = true
	if err := CheckTarget(chani, look); err == nil || looked {
		t.Fatalf("a private target: %v, looked %v", err, looked)
	}
	if err := CheckDelegation(odrade, chani, look, Delegation{Agent: "chani", Task: "x"}, 0, 12); err == nil || looked {
		t.Fatalf("a delegation to a private target: %v, looked %v", err, looked)
	}
}

// A profile's checkout is found in any case, as GitHub compares names
// (Codex review of #26, round 1).
func TestCheckoutKey(t *testing.T) {
	p := profiles.Profile{Repositories: map[string]string{"frostyard/example": "/w/example"}}
	for _, repo := range []string{"frostyard/example", "Frostyard/Example"} {
		if key, ok := CheckoutKey(p, repo); !ok || key != "frostyard/example" {
			t.Errorf("%s: %q %v", repo, key, ok)
		}
	}
	if _, ok := CheckoutKey(p, "frostyard/other"); ok {
		t.Error("another repository has a checkout")
	}
}

func TestBriefSaysWhenItsBranchStartsFromHEAD(t *testing.T) {
	for _, tc := range []struct {
		branch, stale string
		want          bool
	}{
		{"agent/task", "lookup failed", true},
		{"agent/task", "", false},
		{"", "lookup failed", false},
	} {
		brief := Brief("Supervisor", 1, "Task", "Build it.", "/src", tc.branch, tc.stale)
		line := StaleBase("lookup failed")
		if strings.Contains(brief, line) != tc.want {
			t.Fatalf("brief = %q, want stale line = %v", brief, tc.want)
		}
		if tc.want && !strings.Contains(brief, "when the change is ready.\n"+line+"\nReport") {
			t.Fatalf("stale line is misplaced: %q", brief)
		}
	}
}
