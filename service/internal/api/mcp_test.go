package api

import (
	"context"
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// At start, once the MCP servers answered, each session of a profile with
// a server that takes them gets its profile's MCP tools: the profile's own
// session and its thread's specialist; a plain session, another profile's
// and a thread's worker do not (ADR-0063).
func TestStartMCPGivesTheSessionsThatTakeThemTheirProfilesMCPTools(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.engine.mcpProfiles = []string{"duncan"}
	own, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "own", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	h.server.StartMCP()
	h.server.passes.Wait()
	if h.engine.mcpConnects != 1 {
		t.Fatalf("ConnectMCP ran %d times, want once", h.engine.mcpConnects)
	}
	for id, want := range map[int64][]string{own.ID: {"duncan"}, thread.Session: {"duncan"}, plain.ID: nil, h.super.ID: nil} {
		if got := h.engine.mcpTools[id]; !slices.Equal(got, want) {
			t.Errorf("session %d given the MCP tools of %v, want %v", id, got, want)
		}
	}
}

// Without a profile that has an MCP server, nothing connects and no
// session changes.
func TestStartMCPWithNoServerDoesNothing(t *testing.T) {
	h := newTeamHarness(t)
	h.server.StartMCP()
	h.server.passes.Wait()
	if h.engine.mcpConnects != 0 || len(h.engine.mcpTools) != 0 {
		t.Fatalf("connects %d, sessions %v", h.engine.mcpConnects, h.engine.mcpTools)
	}
}
