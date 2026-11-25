package pig

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A supervisor's team_inbox call reaches the team, keyed by its session,
// its task and its call's id, with no arguments, and the team's text is
// its result (ADR-0046).
func TestTeamInboxCallsTheTeam(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolInbox, map[string]any{}, "i1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{inboxResult: "Asks: none."}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultOf(t, e, id, "inbox"); got != "Asks: none." {
		t.Fatalf("the result %q", got)
	}
	if len(fake.inboxes) != 1 || fake.inboxes[0].Session != id || fake.inboxes[0].CallID != "i1" || fake.inboxes[0].Task == 0 {
		t.Fatalf("calls %+v", fake.inboxes)
	}
}

// team_inbox's answer reaches the supervisor whole at its bounds: one long
// line of four-byte characters, as many short lines as it may have, and
// both at once. PiG cuts no part of it (ADR-0046).
func TestAnInboxReachesTheSupervisorWhole(t *testing.T) {
	wide := strings.Repeat("\U0001F600", team.MaxInboxBytes/4)
	tall := strings.Repeat("x\n", team.MaxInboxLines-1) + "x"
	line := strings.Repeat("x", team.MaxInboxBytes/team.MaxInboxLines-1)
	full := strings.Repeat(line+"\n", team.MaxInboxLines-1) + line
	for name, text := range map[string]string{"wide": wide, "tall": tall, "full": full} {
		if len(text) > team.MaxInboxBytes || strings.Count(text, "\n")+1 > team.MaxInboxLines {
			t.Fatalf("%s: %d bytes and %d lines is past the bounds", name, len(text), strings.Count(text, "\n")+1)
		}
		f := newFixture(t)
		e := f.open(fauxModels(toolTurn(toolInbox, map[string]any{}, "i1"), answer("ok")))
		e.SetTeam(&fakeTeam{inboxResult: text})
		ctx := context.Background()
		id, err := e.CreateSession(ctx, "odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := toolResultOf(t, e, id, "inbox"); got != text {
			t.Errorf("%s: the answer came through with %d of its %d bytes, %d of its %d lines", name, len(got), len(text), strings.Count(got, "\n")+1, strings.Count(text, "\n")+1)
		}
		_ = e.Close(context.Background())
	}
}

// toolResultOf sends text to session id and is the text of the first tool
// result its run wrote.
func toolResultOf(t *testing.T, e *Engine, id int64, text string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Send(ctx, id, text, text); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ToolResult != nil {
			return entry.ToolResult.Text
		}
	}
	t.Fatal("no tool result")
	return ""
}

// withoutTool gives session id its tools less name, as a session made
// before that tool has them.
func withoutTool(t *testing.T, e *Engine, id int64, name string) {
	t.Helper()
	ctx := context.Background()
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var older []*durable.ToolRegistration
	for _, tool := range agent.Tools {
		if tool.Name != name {
			older = append(older, tool)
		}
	}
	if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: older})}); err != nil {
		t.Fatal(err)
	}
}

// The supervisor's standing session, started before team_inbox, gets it
// in the place a new one has it, after team_launch, and keeps every other
// tool where it is: from SetSupervisorTools, once, and from the idle
// refresh that gives a standing session its profile's tools (Reconfigure,
// ADR-0026), which is how pocketd gives it to a standing one after a
// restart (ADR-0046).
func TestAStandingSupervisorStartedBeforeTeamInboxGetsIt(t *testing.T) {
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

	withoutTool(t, e, id, toolInbox)
	if got := agentTools(t, e, id); slices.Contains(got, toolInbox) {
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

	withoutTool(t, e, id, toolInbox)
	if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != api.ReconfigureApplied {
		t.Fatalf("the refresh: %v %v", did, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("tools after the refresh %q, want %q", got, want)
	}
}
