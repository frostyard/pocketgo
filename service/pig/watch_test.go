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

// A specialist's team_watch call reaches the team with its arguments,
// keyed by its session, its task and its call's id, and the team's text
// is its result (ADR-0073).
func TestTeamWatchCallsTheTeam(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolWatch, map[string]any{"action": "add", "pull": "frostyard/example#12", "until": "checks", "hours": 2.5}, "w1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Thread: 3,
		Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultOf(t, e, id, "watch"); got != "watching" {
		t.Fatalf("the result %q", got)
	}
	if len(fake.watches) != 1 || fake.watches[0].Action != team.WatchAdd || fake.watches[0].Pull != "frostyard/example#12" ||
		fake.watches[0].Until != team.UntilChecks || fake.watches[0].Hours == nil || *fake.watches[0].Hours != 2.5 {
		t.Fatalf("calls %+v", fake.watches)
	}
	if call := fake.calls[len(fake.calls)-1]; call.Session != id || call.CallID != "w1" || call.Task == 0 {
		t.Fatalf("the call %+v", call)
	}
}

// The tool's description says how to wait: end the run, and pocketd's one
// message starts the next; never sleep or poll.
func TestTeamWatchSaysHowToWait(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	for _, tool := range e.teamExt.Tools {
		if tool.Name != toolWatch {
			continue
		}
		for _, want := range []string{"returns at once", "end your run", "one message", "Never sleep or poll", "Only an answer that starts with \"Watching W\" promises a message", "comes with no message: act on it now"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("the description lacks %q: %s", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("no team_watch")
}

// A session made before team_watch that gets it now gets it in the place
// a new session has it, once, every other tool where it is (ADR-0073).
func TestSetWatchToolUpgradesAnOlderSession(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	for name, spec := range map[string]api.SessionSpec{
		"an agent":              {Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent, Tools: []string{"read", "bash"}}},
		"a thread's specialist": {Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent, Tools: []string{"read", "bash"}}, Thread: 3},
		"a supervisor":          {Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}, Roster: "Delegate to vera."},
		"a plan's lead":         {Profile: &profiles.Profile{ID: "duncan", Role: profiles.RoleAgent}, Thread: 4, Plan: 1},
	} {
		spec.Cwd, spec.Model = f.work, fauxRef
		id, err := e.CreateSession(ctx, name, spec)
		if err != nil {
			t.Fatal(err)
		}
		want := agentTools(t, e, id)
		if !slices.Contains(want, toolWatch) {
			t.Fatalf("%s: a new session's tools %q", name, want)
		}
		withoutTool(t, e, id, toolWatch)
		if changed, err := e.SetWatchTool(ctx, id); err != nil || !changed {
			t.Fatalf("%s: the upgrade %v %v", name, changed, err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, want) {
			t.Fatalf("%s: tools after the upgrade %q, want %q", name, got, want)
		}
		if changed, err := e.SetWatchTool(ctx, id); err != nil || changed {
			t.Fatalf("%s: again %v %v", name, changed, err)
		}
	}
}

// A session made before team_memory, with team_delegate or team_message
// and no team_memory, gets team_watch when its caller says it may: the
// caller decides, not the tools it has (protocol rule 34; Codex review of
// #173, round 4). One with no team tool at all gets the team's extension
// and team_watch alone, never the extension's other tools.
func TestSetWatchToolUpgradesASessionFromBeforeTeamMemory(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	for name, spec := range map[string]api.SessionSpec{
		"a supervisor":          {Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}, Roster: "Delegate to vera."},
		"a thread's specialist": {Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent, Tools: []string{"read", "bash"}}, Thread: 3},
	} {
		spec.Cwd, spec.Model = f.work, fauxRef
		id, err := e.CreateSession(ctx, name, spec)
		if err != nil {
			t.Fatal(err)
		}
		withoutTool(t, e, id, toolWatch)
		withoutTool(t, e, id, toolMemory)
		older := agentTools(t, e, id)
		if slices.Contains(older, toolMemory) || !slices.Contains(older, toolDelegate) && !slices.Contains(older, toolMessage) {
			t.Fatalf("%s: the older session's tools %q", name, older)
		}
		if changed, err := e.SetWatchTool(ctx, id); err != nil || !changed {
			t.Fatalf("%s: the upgrade %v %v", name, changed, err)
		}
		// After its team tools, before the question tool, as a new session
		// has it.
		at := slices.Index(older, toolQuestion)
		if at < 0 {
			at = len(older)
		}
		want := slices.Concat(older[:at], []string{toolWatch}, older[at:])
		if got := agentTools(t, e, id); !slices.Equal(got, want) {
			t.Fatalf("%s: tools after the upgrade %q, want %q", name, got, want)
		}
	}
	plain, err := e.CreateSession(ctx, "plain", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := e.SetWatchTool(ctx, plain); err != nil || !changed {
		t.Fatalf("a session with no team tool: %v %v", changed, err)
	}
	if got := agentTools(t, e, plain); !slices.Equal(got, []string{"read", "write", "edit", "bash", toolWatch}) {
		t.Fatalf("a session with no team tool got %q", got)
	}
	if changed, err := e.SetWatchTool(ctx, plain); err != nil || changed {
		t.Fatalf("again: %v %v", changed, err)
	}
}
