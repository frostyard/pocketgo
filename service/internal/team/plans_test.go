package team

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// planTeam is a roster for the plan rules: odrade supervises; vera works
// on two repositories and has a home; duncan has a home; chani is private.
var planTeam = map[string]profiles.Profile{
	"odrade": {ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Home: "/home/odrade"},
	"vera": {ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Home: "/home/vera",
		Repositories: map[string]string{"frostyard/example": "/src/example", "frostyard/other": "/src/other"}},
	"duncan": {ID: "duncan", Name: "Duncan", Role: profiles.RoleAgent, Home: "/home/duncan"},
	"chani":  {ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Home: "/home/chani", Private: true},
	"nomad":  {ID: "nomad", Name: "Nomad", Role: profiles.RoleAgent, Repositories: map[string]string{"frostyard/example": "/src/nomad"}},
}

func findPlanTeam(id string) (profiles.Profile, error) {
	p, ok := planTeam[id]
	if !ok {
		return profiles.Profile{}, fmt.Errorf("no profile %s", id)
	}
	return p, nil
}

func wantErr(t *testing.T, name string, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %v, want an error with %q", name, err, want)
	}
}

func TestCheckPlanRef(t *testing.T) {
	for _, good := range []PlanRef{{Start: true}, {ID: 1, Task: "t2"}} {
		if err := CheckPlanRef(good); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
	wantErr(t, "both", CheckPlanRef(PlanRef{Start: true, ID: 1}), "not both")
	wantErr(t, "neither", CheckPlanRef(PlanRef{}), "start: true")
	wantErr(t, "no task", CheckPlanRef(PlanRef{ID: 1}), "task")
	wantErr(t, "no id", CheckPlanRef(PlanRef{Task: "t1"}), "id")
}

func TestCheckPlanStart(t *testing.T) {
	idea := Delegation{Agent: "duncan", Task: "Plan the signing of the ISO", Plan: &PlanRef{Start: true}}
	supervisor := planTeam["odrade"]
	if err := CheckPlanStart(&supervisor, planTeam["duncan"], nil, idea, MaxPlansOpen-1); err != nil {
		t.Fatalf("a good start: %v", err)
	}
	agent := planTeam["vera"]
	withRepo := idea
	withRepo.Repository = "frostyard/example"
	long := idea
	long.Task = strings.Repeat("x", MaxTask+1)
	for name, tc := range map[string]struct {
		caller    *profiles.Profile
		lead      string
		startable func() error
		d         Delegation
		open      int
		want      string
	}{
		"no caller profile":   {nil, "duncan", nil, idea, 0, "only a supervisor"},
		"an agent starts one": {&agent, "duncan", nil, idea, 0, "only a supervisor"},
		"a private lead":      {&supervisor, "chani", nil, idea, 0, "private"},
		"a supervisor lead":   {&supervisor, "odrade", nil, idea, 0, "supervisor"},
		"a lead that can't":   {&supervisor, "duncan", func() error { return errors.New("no sign-in") }, idea, 0, "no sign-in"},
		"a repository":        {&supervisor, "vera", nil, withRepo, 0, "leave the repository out"},
		"no home":             {&supervisor, "nomad", nil, idea, 0, "no home or workspace"},
		"an empty idea":       {&supervisor, "duncan", nil, Delegation{Agent: "duncan", Task: " "}, 0, "empty"},
		"a long idea":         {&supervisor, "duncan", nil, long, 0, "more than"},
		"open plans at cap":   {&supervisor, "duncan", nil, idea, MaxPlansOpen, "plans are open"},
	} {
		wantErr(t, name, CheckPlanStart(tc.caller, planTeam[tc.lead], tc.startable, tc.d, tc.open), tc.want)
	}
}

func TestCheckPlanCall(t *testing.T) {
	rev := Rev("x")
	for _, good := range []PlanCall{
		{Action: PlanRead},
		{Action: PlanReady},
		{Action: PlanWrite, Text: "# Plan"},
		{Action: PlanWrite, Text: "# Plan", Rev: rev, Asks: []int64{3, 4}},
		{Action: PlanTasks, Tasks: []Task{{ID: "t1"}}},
		{Action: PlanConsult, Agents: []string{"vera"}, Question: "Does it hold?", Ask: 3},
	} {
		if err := CheckPlanCall(good); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
	for name, tc := range map[string]struct {
		c    PlanCall
		want string
	}{
		"an unknown action":   {PlanCall{Action: "approve"}, "not read, write"},
		"read with text":      {PlanCall{Action: PlanRead, Text: "x"}, "nothing but the page"},
		"read a bad page":     {PlanCall{Action: PlanRead, Page: -1}, "not a page"},
		"page 2, no snapshot": {PlanCall{Action: PlanRead, Page: 2}, "needs the snapshot"},
		"write with a page":   {PlanCall{Action: PlanWrite, Text: "x", Page: 2}, "takes no page"},
		"ready with tasks":    {PlanCall{Action: PlanReady, Tasks: []Task{{ID: "t1"}}}, "nothing but the action"},
		"write with no text":  {PlanCall{Action: PlanWrite, Text: "  "}, "whole new text"},
		"write too long":      {PlanCall{Action: PlanWrite, Text: strings.Repeat("x", MaxPlanText+1)}, "more than"},
		"write a short rev":   {PlanCall{Action: PlanWrite, Text: "x", Rev: "abc1234"}, "not a revision"},
		"write with tasks":    {PlanCall{Action: PlanWrite, Text: "x", Tasks: []Task{{ID: "t1"}}}, "takes the text"},
		"write a bad ask":     {PlanCall{Action: PlanWrite, Text: "x", Asks: []int64{0}}, "not an ask's number"},
		"write an ask twice":  {PlanCall{Action: PlanWrite, Text: "x", Asks: []int64{3, 3}}, "named twice"},
		"tasks with no tasks": {PlanCall{Action: PlanTasks}, "needs the graph's tasks"},
		"tasks with text":     {PlanCall{Action: PlanTasks, Text: "x", Tasks: []Task{{ID: "t1"}}}, "only the tasks"},
		"consult no agent":    {PlanCall{Action: PlanConsult, Question: "q"}, "1 to 3 agents"},
		"consult 4 agents":    {PlanCall{Action: PlanConsult, Agents: []string{"a", "b", "c", "d"}, Question: "q"}, "1 to 3 agents"},
		"consult twice":       {PlanCall{Action: PlanConsult, Agents: []string{"vera", "vera"}, Question: "q"}, "named twice"},
		"consult no question": {PlanCall{Action: PlanConsult, Agents: []string{"vera"}}, "needs the question"},
		"consult too long":    {PlanCall{Action: PlanConsult, Agents: []string{"vera"}, Question: strings.Repeat("q", MaxConsultQuestion+1)}, "more than"},
		"consult a bad ask":   {PlanCall{Action: PlanConsult, Agents: []string{"vera"}, Question: "q", Ask: -1}, "not an ask's number"},
		"consult with text":   {PlanCall{Action: PlanConsult, Agents: []string{"vera"}, Question: "q", Text: "x"}, "takes the agents"},
	} {
		wantErr(t, name, CheckPlanCall(tc.c), tc.want)
	}
}

func TestRevIsTheTextsDigest(t *testing.T) {
	a, b := Rev("plan one"), Rev("plan two")
	if a == b || len(a) != 64 || !revPattern.MatchString(a) {
		t.Fatalf("revs %q and %q", a, b)
	}
	if Rev("plan one") != a {
		t.Fatal("a text's rev changed")
	}
	if ShortRev(a) != a[:7] {
		t.Fatalf("short rev %q", ShortRev(a))
	}
}

func graphTask(id, agent, repo string, deps ...string) Task {
	return Task{ID: id, Title: "Task " + id, Agent: agent, Repository: repo, Acceptance: []string{"It works"}, Deps: deps}
}

func TestCheckGraph(t *testing.T) {
	good := []Task{graphTask("t1", "vera", "frostyard/example"), graphTask("t2", "duncan", "", "t1"), graphTask("t3", "vera", "", "t1", "t2")}
	if err := CheckGraph(good, findPlanTeam); err != nil {
		t.Fatalf("a good graph: %v", err)
	}
	many := make([]Task, MaxGraphTasks+1)
	for i := range many {
		many[i] = graphTask(fmt.Sprintf("t%d", i+1), "duncan", "")
	}
	with := func(change func(*Task)) []Task {
		out := CleanTasks(good)
		change(&out[1])
		return out
	}
	for name, tc := range map[string]struct {
		tasks []Task
		want  string
	}{
		"no task":          {nil, "1 to 30 tasks"},
		"too many":         {many, "1 to 30 tasks"},
		"a bad id":         {with(func(x *Task) { x.ID = "task2" }), "not t1 to t999"},
		"t0":               {with(func(x *Task) { x.ID = "t0" }), "not t1 to t999"},
		"a reused id":      {with(func(x *Task) { x.ID = "t1" }), "used twice"},
		"no title":         {with(func(x *Task) { x.Title = "" }), "no title"},
		"a long title":     {with(func(x *Task) { x.Title = strings.Repeat("t", MaxTitle+1) }), "title has"},
		"no acceptance":    {with(func(x *Task) { x.Acceptance = nil }), "0 acceptance lines"},
		"9 acceptance":     {with(func(x *Task) { x.Acceptance = slices.Repeat([]string{"ok"}, 9) }), "9 acceptance lines"},
		"an empty line":    {with(func(x *Task) { x.Acceptance = []string{""} }), "line 1 is empty"},
		"a long line":      {with(func(x *Task) { x.Acceptance = []string{strings.Repeat("l", MaxAcceptanceLine+1)} }), "line 1 has"},
		"an unknown agent": {with(func(x *Task) { x.Agent = "nobody" }), "no profile nobody"},
		"a private agent":  {with(func(x *Task) { x.Agent = "chani" }), "private"},
		"a supervisor":     {with(func(x *Task) { x.Agent = "odrade" }), "supervisor"},
		"not its repo":     {with(func(x *Task) { x.Repository = "frostyard/example" }), "works on no repository"},
		"no home, no repo": {with(func(x *Task) { x.Agent, x.Repository = "nomad", "" }), "no home or workspace"},
		"itself":           {with(func(x *Task) { x.Deps = []string{"t2"} }), "depends on itself"},
		"a missing dep":    {with(func(x *Task) { x.Deps = []string{"t9"} }), "no task of the graph"},
		"a dep twice":      {with(func(x *Task) { x.Deps = []string{"t1", "t1"} }), "names t1 twice"},
	} {
		wantErr(t, name, CheckGraph(tc.tasks, findPlanTeam), tc.want)
	}
	cycle := []Task{graphTask("t1", "duncan", "", "t3"), graphTask("t2", "duncan", "", "t1"), graphTask("t3", "duncan", "", "t2")}
	wantErr(t, "a cycle", CheckGraph(cycle, findPlanTeam), "cycle: t1 → t3 → t2 → t1")
}

func TestCleanTasksTrims(t *testing.T) {
	got := CleanTasks([]Task{{ID: " t1 ", Title: " Do it ", Agent: " vera ", Repository: " frostyard/example ", Acceptance: []string{" ok "}, Deps: []string{" t2 "}}})
	want := Task{ID: "t1", Title: "Do it", Agent: "vera", Repository: "frostyard/example", Acceptance: []string{"ok"}, Deps: []string{"t2"}}
	if fmt.Sprint(got[0]) != fmt.Sprint(want) {
		t.Fatalf("cleaned %+v, want %+v", got[0], want)
	}
	if got := CleanTasks([]Task{{ID: "t1"}}); got[0].Acceptance == nil || got[0].Deps == nil {
		t.Fatal("a cleaned task has a nil list")
	}
}

func TestNowReady(t *testing.T) {
	tasks := []TaskFacts{
		{Task: graphTask("t1", "vera", ""), State: TaskDone},
		{Task: graphTask("t2", "vera", "", "t1"), State: TaskWaiting},
		{Task: graphTask("t3", "vera", "", "t1", "t4"), State: TaskWaiting},
		{Task: graphTask("t4", "vera", ""), State: TaskDelegated},
		{Task: graphTask("t5", "vera", ""), State: TaskWaiting},
		{Task: graphTask("t6", "vera", "", "t1"), State: TaskReady},
	}
	if got := NowReady(tasks); !slices.Equal(got, []string{"t2", "t5"}) {
		t.Fatalf("now ready %v, want t2 and t5", got)
	}
}

func TestGate(t *testing.T) {
	asks := []GateAsk{{ID: 4, Written: false}, {ID: 3, Open: true}, {ID: 5, Written: true}, {ID: 6, Open: true}}
	consults := []GateConsult{{ID: 1, State: ConsultAnswered}, {ID: 2, State: ConsultRunning}, {ID: 7, State: ConsultStarting}, {ID: 8, State: ConsultFailed}}
	got := Gate(asks, consults)
	want := []string{"A3 open", "A6 open", "A4 not written", "K2 running", "K7 starting"}
	if !slices.Equal(got, want) {
		t.Fatalf("gate %v, want %v", got, want)
	}
	if Waiting(got) != "waiting: A3 open, A6 open, A4 not written, K2 running, K7 starting" {
		t.Fatalf("waiting %q", Waiting(got))
	}
	if got := Gate([]GateAsk{{ID: 5, Written: true}}, []GateConsult{{ID: 1, State: ConsultStopped}}); len(got) != 0 || Waiting(got) != "settled" {
		t.Fatalf("a settled plan: %v", got)
	}
}

func TestPlanGateFollowsTheState(t *testing.T) {
	asks := []GateAsk{{ID: 3, Open: true}, {ID: 4}}
	consults := []GateConsult{{ID: 2, State: ConsultRunning}}
	for _, state := range []string{PlanDrafting, PlanReview} {
		if got := PlanGate(state, asks, consults); !slices.Equal(got, []string{"A3 open", "A4 not written", "K2 running"}) {
			t.Errorf("%s: %v", state, got)
		}
	}
	for _, state := range []string{PlanApproved, PlanBuilding} {
		if got := PlanGate(state, asks, consults); !slices.Equal(got, []string{"A3 open"}) {
			t.Errorf("%s: %v, want only the open ask", state, got)
		}
	}
	for _, state := range []string{PlanDone, PlanDropped} {
		if got := PlanGate(state, asks, consults); len(got) != 0 {
			t.Errorf("%s: %v, want nothing", state, got)
		}
	}
}

func TestCheckDraft(t *testing.T) {
	for _, ok := range [][2]string{{PlanDrafting, PlanWrite}, {PlanDrafting, PlanConsult}, {PlanReview, PlanWrite}, {PlanReview, PlanTasks}} {
		if err := CheckDraft(1, ok[0], ok[1]); err != nil {
			t.Errorf("%s in %s: %v", ok[1], ok[0], err)
		}
	}
	wantErr(t, "consult in review", CheckDraft(1, PlanReview, PlanConsult), "a consult is for a plan in drafting")
	for _, state := range []string{PlanApproved, PlanBuilding} {
		wantErr(t, "write in "+state, CheckDraft(1, state, PlanWrite), "no longer change")
	}
	wantErr(t, "write when done", CheckDraft(1, PlanDone, PlanWrite), "plan P1 is done")
	wantErr(t, "write when dropped", CheckDraft(1, PlanDropped, PlanTasks), "plan P1 is dropped")
}

func TestCheckWrite(t *testing.T) {
	current := Rev("old text")
	if err := CheckWrite(1, "", "", "first text"); err != nil {
		t.Fatalf("a first write: %v", err)
	}
	if err := CheckWrite(1, current, current, "new text"); err != nil {
		t.Fatalf("a write from the current rev: %v", err)
	}
	wantErr(t, "a rev on a plan with no text", CheckWrite(1, "", Rev("x"), "text"), "write it with no rev")
	wantErr(t, "no rev on a written plan", CheckWrite(1, current, "", "text"), "changed since rev")
	wantErr(t, "a stale rev", CheckWrite(1, current, Rev("older"), "text"), "it is at rev "+ShortRev(current))
	wantErr(t, "no change", CheckWrite(1, current, current, "old text"), "a write changes it")
}

func TestCheckReadyAndApprove(t *testing.T) {
	rev := Rev("text")
	if err := CheckReady(1, PlanDrafting, rev, rev, nil); err != nil {
		t.Fatalf("a ready plan: %v", err)
	}
	wantErr(t, "in review", CheckReady(1, PlanReview, rev, rev, nil), "waits for the owner's approval already")
	wantErr(t, "approved", CheckReady(1, PlanApproved, rev, rev, nil), "approved already")
	wantErr(t, "dropped", CheckReady(1, PlanDropped, rev, rev, nil), "dropped")
	wantErr(t, "no text", CheckReady(1, PlanDrafting, "", "", nil), "no text")
	wantErr(t, "not settled", CheckReady(1, PlanDrafting, rev, rev, []string{"A3 open"}), "waiting: A3 open")
	wantErr(t, "an old graph", CheckReady(1, PlanDrafting, rev, Rev("older"), nil), "no task graph for its rev")

	if err := CheckApprove(1, PlanReview, rev, 2, nil, rev, 2); err != nil {
		t.Fatalf("an approval as seen: %v", err)
	}
	wantErr(t, "drafting", CheckApprove(1, PlanDrafting, rev, 2, nil, rev, 2), "does not wait for an approval")
	wantErr(t, "not settled", CheckApprove(1, PlanReview, rev, 2, []string{"K1 running"}, rev, 2), "waiting: K1 running")
	wantErr(t, "another rev", CheckApprove(1, PlanReview, rev, 2, nil, Rev("seen"), 2), "not the rev")
	wantErr(t, "another graph", CheckApprove(1, PlanReview, rev, 2, nil, rev, 1), "version 2, not the version 1")
}

func TestCheckDelivery(t *testing.T) {
	task := &TaskFacts{Task: graphTask("t2", "vera", "frostyard/example", "t1"), State: TaskReady}
	d := Delegation{Agent: "vera", Task: "Build t2", Repository: "frostyard/example", Plan: &PlanRef{ID: 1, Task: "t2"}}
	for _, state := range []string{PlanApproved, PlanBuilding} {
		if err := CheckDelivery(1, state, nil, task, d); err != nil {
			t.Fatalf("a ready task in %s: %v", state, err)
		}
	}
	waiting := *task
	waiting.State = TaskWaiting
	delegated := *task
	delegated.State, delegated.Thread = TaskDelegated, 7
	done := *task
	done.State = TaskDone
	home := *task
	home.Repository = ""
	otherAgent, otherRepo, noRepo := d, d, d
	otherAgent.Agent = "duncan"
	otherRepo.Repository = "frostyard/other"
	noRepo.Repository = ""
	for name, tc := range map[string]struct {
		state string
		open  []string
		task  *TaskFacts
		d     Delegation
		want  string
	}{
		"drafting":        {PlanDrafting, nil, task, d, "not approved: it is drafting"},
		"review":          {PlanReview, nil, task, d, "not approved: it is review"},
		"done":            {PlanDone, nil, task, d, "plan P1 is done"},
		"dropped":         {PlanDropped, nil, task, d, "plan P1 is dropped"},
		"an open ask":     {PlanBuilding, []string{"A3 open"}, task, d, "waiting: A3 open"},
		"no such task":    {PlanApproved, nil, nil, d, `no task "t2"`},
		"waiting":         {PlanApproved, nil, &waiting, d, "t2 waits for t1"},
		"delegated":       {PlanBuilding, nil, &delegated, d, "delivered already, in T7"},
		"done task":       {PlanBuilding, nil, &done, d, "t2 is done"},
		"another agent":   {PlanApproved, nil, task, otherAgent, "for vera, not duncan"},
		"another repo":    {PlanApproved, nil, task, otherRepo, "name that repository"},
		"no repo":         {PlanApproved, nil, task, noRepo, "name that repository"},
		"a home with one": {PlanApproved, nil, &home, d, "leave the repository out"},
	} {
		wantErr(t, name, CheckDelivery(1, tc.state, tc.open, tc.task, tc.d), tc.want)
	}
}

func TestCheckConsult(t *testing.T) {
	can := func(profiles.Profile) error { return nil }
	if err := CheckConsult("duncan", []string{"vera"}, findPlanTeam, can, MaxConsultsRunning-1, MaxConsults-1); err != nil {
		t.Fatalf("a good consult: %v", err)
	}
	cannot := func(p profiles.Profile) error { return fmt.Errorf("%s has no sign-in", p.ID) }
	for name, tc := range map[string]struct {
		agents    []string
		startable func(profiles.Profile) error
		running   int
		total     int
		want      string
	}{
		"the lead":         {[]string{"duncan"}, can, 0, 0, "leads the plan"},
		"unknown":          {[]string{"nobody"}, can, 0, 0, "no profile nobody"},
		"private":          {[]string{"chani"}, can, 0, 0, "private"},
		"a supervisor":     {[]string{"odrade"}, can, 0, 0, "supervisor"},
		"cannot start":     {[]string{"vera"}, cannot, 0, 0, "vera has no sign-in"},
		"no home":          {[]string{"nomad"}, can, 0, 0, "no home or workspace"},
		"too many at once": {[]string{"vera"}, can, MaxConsultsRunning, 0, "at most 3 may at once"},
		"a panel too many": {[]string{"vera", "chani"}, func(profiles.Profile) error { return nil }, 0, 0, "private"},
		"too many in all":  {[]string{"vera"}, can, 0, MaxConsults, "may have 8 in all"},
	} {
		wantErr(t, name, CheckConsult("duncan", tc.agents, findPlanTeam, tc.startable, tc.running, tc.total), tc.want)
	}
}

func TestCheckFinding(t *testing.T) {
	for _, stance := range []string{StanceSupports, StanceObjects, StanceNeutral} {
		if err := CheckFinding(Finding{Stance: stance, Summary: "It holds."}); err != nil {
			t.Errorf("%s: %v", stance, err)
		}
	}
	wantErr(t, "a stance", CheckFinding(Finding{Stance: "maybe", Summary: "x"}), "supports, objects or neutral")
	wantErr(t, "no summary", CheckFinding(Finding{Stance: StanceNeutral, Summary: " "}), "empty")
	wantErr(t, "too long", CheckFinding(Finding{Stance: StanceNeutral, Summary: strings.Repeat("s", MaxFindingSummary+1)}), "more than")
}

func TestTaskWorkDone(t *testing.T) {
	for name, tc := range map[string]struct {
		last  string
		pulls []string
		want  bool
	}{
		"done, no pull request":    {StatusDone, nil, true},
		"working, no pull request": {"open", nil, false},
		"blocked, no pull request": {StatusBlocked, nil, false},
		"merged":                   {"open", []string{"merged"}, true},
		"merged and closed":        {StatusDone, []string{"closed", "merged"}, true},
		"merged and open":          {StatusDone, []string{"merged", "open"}, false},
		"only closed":              {StatusDone, []string{"closed"}, false},
		"open":                     {StatusDone, []string{"open"}, false},
	} {
		if got := TaskWorkDone(tc.last, tc.pulls); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestPlanTexts(t *testing.T) {
	rev := Rev("text")
	for name, tc := range map[string]struct {
		got  string
		want []string
	}{
		"the brief": {PlanBrief("Odrade", 5, 1, "Signing", "Plan the ISO signing", "/home/duncan"),
			[]string{"lead plan P1 in thread T5: Signing", "Plan the ISO signing", "You work in /home/duncan with read alone", "team_plan write", "team_plan read shows the plan, and the agents", "asks of team_message", "team_plan consult", "team_plan tasks", "team_plan ready", "only when you are blocked"}},
		"a task": {PlanTaskText("Build it", 1, "Signing", rev, "# Signing\n\nUse the TPM key.", graphTask("t2", "vera", "")),
			[]string{"Build it", "task t2 of plan P1 (Signing)", "rev " + ShortRev(rev), "- It works", "one pull request",
				"The plan, as the owner approved it", "# Signing\n\nUse the TPM key."}},
		"an answer": {PlanAnswerText(3, "Which key?", "The TPM"), []string{`The owner answered A3 ("Which key?"): The TPM`, "team_plan write, naming 3 in asks"}},
		"approved": {ApprovedNote(1, "Signing", rev, 2, []Task{graphTask("t1", "vera", "frostyard/example"), graphTask("t3", "duncan", "")}),
			[]string{"[P1 · approved]", "rev " + ShortRev(rev), "version 2", `plan {"id": 1, "task": "<id>"}`,
				"\n- t1 · Task t1: agent vera, repository frostyard/example", "\n- t3 · Task t3: agent duncan, no repository (its home)"}},
		"ready": {ReadyNote(1, "t1", 7, []Task{graphTask("t2", "vera", "frostyard/example")}),
			[]string{"[P1 · t1 done]", "T7 closed", "Ready now", "- t2 · Task t2: agent vera, repository frostyard/example"}},
		"again":      {AgainNote(1, graphTask("t2", "duncan", ""), 8, []Task{graphTask("t2", "duncan", "")}), []string{"[P1 · t2 ready again]", "T8 closed without t2's work done", "- t2 · Task t2: agent duncan, no repository"}},
		"done":       {DoneNote(1, "Signing", 5), []string{"[P1 · done]", "thread T5 closed"}},
		"closed":     {PlanClosedReport(1), []string{"every task of plan P1 is done"}},
		"a question": {ConsultQuestion("Duncan", 2, 1, "Signing", "Do keys survive?", 3, "Which key?"), []string{"Duncan, who leads plan P1 (Signing), consults you as K2", "Do keys survive?", `A3: "Which key?"`, "team_finding"}},
		"no ask":     {ConsultQuestion("Duncan", 2, 1, "Signing", "Do keys survive?", 0, ""), []string{"consults you as K2"}},
		"a finding":  {FindingNote(2, "Vera", 1, 3, StanceObjects, "They do not."), []string{"[K2 · Vera · consult on P1, on A3] objects: They do not."}},
		"an end":     {ConsultEndNote(2, "Vera", 1, "no finding 60 minutes after the call"), []string{"[K2 · Vera · consult on P1] It ended without a finding: no finding 60 minutes"}},
		"the roster": {PlanRoster(), []string{`plan {"start": true}`, "at most 3 plans", `plan {"id": <plan>, "task": "<id>"}`, "names its plan"}},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.got, w) {
				t.Errorf("%s: %q lacks %q", name, tc.got, w)
			}
		}
	}
	if strings.Contains(ConsultQuestion("Duncan", 2, 1, "Signing", "q", 0, ""), "decision A") {
		t.Error("a question on no ask names one")
	}
}

// The lead reads whom its tasks can go to and its consults can ask, with
// where each works: only the supervisor gets the roster (Codex review of
// #25, round 2).
func TestPlanAgents(t *testing.T) {
	list := make([]profiles.Profile, 0, len(planTeam))
	for _, p := range planTeam {
		list = append(list, p)
	}
	cannot := func(p profiles.Profile) error {
		if p.ID == "nomad" {
			return errors.New("no sign-in")
		}
		return nil
	}
	got := PlanAgents(list, cannot, "duncan")
	for _, want := range []string{"- duncan (Duncan): its home; it leads this plan, so no consult",
		"- vera (Vera): its home; repositories frostyard/example, frostyard/other\n"} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("agents lack %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"odrade", "chani", "nomad"} {
		if strings.Contains(got, "- "+not) {
			t.Errorf("agents list %s:\n%s", not, got)
		}
	}
	nomad := planTeam["nomad"]
	if got := PlanAgents([]profiles.Profile{nomad}, func(profiles.Profile) error { return nil }, "duncan"); !strings.Contains(got, "- nomad (Nomad): repositories frostyard/example; no home, so no consult") {
		t.Errorf("an agent with no home: %s", got)
	}
}

// A read's pages each stay far below what PiG keeps of a tool result, and
// together are the whole read (Codex review of #25, round 5).
func TestPagesKeepEachPartWhole(t *testing.T) {
	big := strings.Repeat("t1: "+strings.Repeat("é", 290)+"\n", 300) + strings.Repeat("x", 3*MaxPageBytes) + "\n" + strings.Repeat("\n", 1000)
	pages := Pages(big)
	if strings.Join(pages, "") != big {
		t.Fatal("the pages are not the whole text")
	}
	for i, p := range pages {
		if len(p) > MaxPageBytes || strings.Count(p, "\n") > MaxPageLines || !utf8.ValidString(p) {
			t.Errorf("page %d: %d bytes, %d lines, valid %v", i+1, len(p), strings.Count(p, "\n"), utf8.ValidString(p))
		}
	}
	if got := Pages("short"); len(got) != 1 || got[0] != "short" {
		t.Errorf("a short read %q", got)
	}
}
