package pig

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// An agent's team_work call reaches the team, keyed by its session, its
// task and its call's id, with its arguments, and the team's text is its
// result (ADR-0073).
func TestTeamWorkCallsTheTeam(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolWork, map[string]any{"repository": "frostyard/example", "task": "Fix the flaky test.", "title": "Flaky"}, "w1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultOf(t, e, id, "work"); got != "Started work session 9." {
		t.Fatalf("the result %q", got)
	}
	if len(fake.works) != 1 || fake.works[0] != (team.Work{Repository: "frostyard/example", Task: "Fix the flaky test.", Title: "Flaky"}) {
		t.Fatalf("works %+v", fake.works)
	}
	if call := fake.calls[len(fake.calls)-1]; call.Session != id || call.CallID != "w1" || call.Task == 0 {
		t.Fatalf("call %+v", call)
	}
	if !slices.Equal(fake.allowed, []int64{id}) {
		t.Fatalf("TeamAllows asked about %v", fake.allowed)
	}
}

// An agent's own session is told where it changes a repository, after its
// identity; a work session, a thread's specialist, a supervisor and a
// private agent are not (ADR-0073).
func TestOnlyASessionThatStartsWorkGetsTheWorkGuide(t *testing.T) {
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Identity: "# Vera"}
	for name, tc := range map[string]struct {
		spec api.SessionSpec
		want bool
	}{
		"an agent's own session": {api.SessionSpec{Profile: vera}, true},
		"a clerk's own session":  {api.SessionSpec{Profile: &profiles.Profile{ID: "clerk", Name: "Clerk", Role: profiles.RoleClerk}}, true},
		"a work session":         {api.SessionSpec{Profile: vera, Work: true}, false},
		"a thread's specialist":  {api.SessionSpec{Profile: vera, Thread: 3}, false},
		"a supervisor":           {api.SessionSpec{Profile: &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor}, Roster: "R"}, false},
		"a private agent":        {api.SessionSpec{Profile: &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Private: true}}, false},
		"a plain session":        {api.SessionSpec{}, false},
	} {
		tc.spec.Cwd = "/work"
		got := sessionInstructions(tc.spec, team.Loaded{Folder: "FOLDER"})
		if strings.Contains(got, team.WorkGuide) != tc.want {
			t.Errorf("%s: the guide %v, want %v:\n%s", name, strings.Contains(got, team.WorkGuide), tc.want, got)
		}
		if tc.want && !strings.Contains(got, ". Your profile follows.\n\n"+tc.spec.Profile.Identity+"\n\n"+team.WorkGuide+"\n\nFOLDER") {
			t.Errorf("%s: the guide is not after the identity, before the folder's AGENTS.md:\n%s", name, got)
		}
	}
}

// An agent's session made before team_work gets it, in the place a new
// session has it, before team_memory, once; one with no team extension
// gets the extension too, and team_work alone among its team tools
// (ADR-0073).
func TestSetWorkToolGivesItToAnOlderAgentSession(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Tools: []string{"read", "bash"}}
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: vera})
	if err != nil {
		t.Fatal(err)
	}
	want := agentTools(t, e, id)
	if !slices.Equal(want, []string{"read", "bash", toolWork, toolMemory}) {
		t.Fatalf("a new agent's session's tools %q", want)
	}
	asMadeBefore(t, e, id, []string{toolWork})
	if changed, err := e.SetWorkTool(ctx, id); err != nil || !changed {
		t.Fatalf("SetWorkTool = %v, %v", changed, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("tools %q, want %q", got, want)
	}
	if changed, err := e.SetWorkTool(ctx, id); err != nil || changed {
		t.Fatalf("again: %v, %v", changed, err)
	}
	bare, err := e.CreateSession(ctx, t.Name()+"-bare", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: vera})
	if err != nil {
		t.Fatal(err)
	}
	asMadeBefore(t, e, bare, []string{toolWork, toolMemory}, e.teamExt.Name)
	if changed, err := e.SetWorkTool(ctx, bare); err != nil || !changed {
		t.Fatalf("SetWorkTool on a session of no team tool = %v, %v", changed, err)
	}
	if got := agentTools(t, e, bare); !slices.Equal(got, []string{"read", "bash", toolWork}) {
		t.Fatalf("a session of no team tool: %q", got)
	}
	conversation, _ := e.conversation(ctx, bare)
	agent, _ := conversation.Agent(ctx)
	if !slices.Contains(extensionNames(agent.Extensions), e.teamExt.Name) {
		t.Fatalf("extensions %q", extensionNames(agent.Extensions))
	}
}

// An agent's standing session takes team_work, and the guide, with its
// profile at its idle refresh, as it takes every tool its profile gives
// (ADR-0026, ADR-0073).
func TestAStandingAgentSessionTakesTeamWorkWithItsProfile(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	duncan := &profiles.Profile{ID: "duncan", Name: "Duncan", Role: profiles.RoleAgent, Home: f.work, Identity: "# Duncan", Standing: true, Tools: []string{"read"}}
	id, err := e.CreateSession(ctx, "standing:duncan", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: duncan, StandingKey: "standing:duncan"})
	if err != nil {
		t.Fatal(err)
	}
	asMadeBefore(t, e, id, []string{toolWork})
	did, err := e.Reconfigure(ctx, id, api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: duncan})
	if err != nil || did != api.ReconfigureApplied {
		t.Fatalf("Reconfigure = %v, %v", did, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, []string{"read", toolWork, toolMemory}) {
		t.Fatalf("the standing session's tools %q", got)
	}
	if got := sessionInstructionsOf(t, e, id); !strings.Contains(got, team.WorkGuide) {
		t.Fatalf("the standing session's instructions:\n%s", got)
	}
}
