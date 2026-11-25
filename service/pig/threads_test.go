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

// A supervisor's team_threads call reaches the team, keyed by its session,
// its task and its call's id, with its action and thread, and the team's
// text is its result (ADR-0071).
func TestTeamThreadsCallsTheTeam(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolThreads, map[string]any{"action": "read", "thread": 5}, "t1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{threadsResult: "T5 whole."}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultOf(t, e, id, "threads"); got != "T5 whole." {
		t.Fatalf("the result %q", got)
	}
	if len(fake.threads) != 1 || fake.threads[0] != (team.ThreadsCall{Action: team.ThreadsRead, Thread: 5}) {
		t.Fatalf("calls %+v", fake.threads)
	}
	if last := fake.calls[len(fake.calls)-1]; last.Session != id || last.CallID != "t1" || last.Task == 0 {
		t.Fatalf("the call's keys %+v", last)
	}
}

// team_threads' answer reaches the supervisor whole at its bounds: one
// long line of four-byte characters, as many short lines as it may have,
// and both at once. PiG cuts no part of it (ADR-0071).
func TestThreadsReachTheSupervisorWhole(t *testing.T) {
	wide := strings.Repeat("\U0001F600", team.MaxThreadsBytes/4)
	tall := strings.Repeat("x\n", team.MaxThreadsLines-1) + "x"
	line := strings.Repeat("x", team.MaxThreadsBytes/team.MaxThreadsLines-1)
	full := strings.Repeat(line+"\n", team.MaxThreadsLines-1) + line
	for name, text := range map[string]string{"wide": wide, "tall": tall, "full": full} {
		if len(text) > team.MaxThreadsBytes || strings.Count(text, "\n")+1 > team.MaxThreadsLines {
			t.Fatalf("%s: %d bytes and %d lines is past the bounds", name, len(text), strings.Count(text, "\n")+1)
		}
		f := newFixture(t)
		e := f.open(fauxModels(toolTurn(toolThreads, map[string]any{"action": "list"}, "t1"), answer("ok")))
		e.SetTeam(&fakeTeam{threadsResult: text})
		ctx := context.Background()
		id, err := e.CreateSession(ctx, "odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := toolResultOf(t, e, id, "threads"); got != text {
			t.Errorf("%s: the answer came through with %d of its %d bytes, %d of its %d lines", name, len(got), len(text), strings.Count(got, "\n")+1, strings.Count(text, "\n")+1)
		}
		_ = e.Close(context.Background())
	}
}

// The supervisor's standing session, started before team_threads, gets it
// in the place a new one has it, after team_inbox, and keeps every other
// tool where it is: from SetSupervisorTools, once, and from the idle
// refresh that gives a standing session its profile's tools (Reconfigure,
// ADR-0026), as ADR-0046 gave team_inbox (ADR-0071).
func TestAStandingSupervisorStartedBeforeTeamThreadsGetsIt(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Tools: []string{"read", "bash"}, Standing: true}
	spec := api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade, Route: "profile", Roster: "Delegate to vera.", StandingKey: "standing:odrade"}
	id, err := e.CreateSession(ctx, "u:d:r1", spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"read", "bash", toolDelegate, toolClose, toolSchedule, toolSend, toolGather, toolBoard, toolLaunch, toolInbox, toolThreads, toolMemory, toolQuestion}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("a new standing session's tools %q, want %q", got, want)
	}

	withoutTool(t, e, id, toolThreads)
	if got := agentTools(t, e, id); slices.Contains(got, toolThreads) {
		t.Fatalf("the older session has %q", got)
	}
	if changed, err := e.SetSupervisorTools(ctx, id); err != nil || !changed {
		t.Fatalf("the upgrade: %v %v", changed, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("tools after the upgrade %q, want %q", got, want)
	}
	if changed, err := e.SetSupervisorTools(ctx, id); err != nil || changed {
		t.Fatalf("again: %v %v", changed, err)
	}

	withoutTool(t, e, id, toolThreads)
	if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != api.ReconfigureApplied {
		t.Fatalf("the refresh: %v %v", did, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("tools after the refresh %q, want %q", got, want)
	}
}
