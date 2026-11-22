package api

import (
	"context"
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// mcpSessions makes duncan's own session, a plain session and duncan's
// thread's specialist, and returns their ids.
func mcpSessions(t *testing.T, h *teamHarness) (own, plain, specialist int64) {
	t.Helper()
	ctx := context.Background()
	o, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "own", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
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
	return o.ID, p.ID, thread.Session
}

// At start, once the MCP servers answered, each session of a profile with
// a server that takes them gets its profile's MCP tools: the profile's own
// session and its thread's specialist; a plain session and another
// profile's do not (ADR-0063).
func TestStartMCPGivesTheSessionsThatTakeThemTheirProfilesMCPTools(t *testing.T) {
	h := newTeamHarness(t)
	own, plain, specialist := mcpSessions(t, h)
	h.engine.mcpProfiles = []string{"duncan"}
	h.server.StartMCP()
	h.server.passes.Wait()
	if h.engine.mcpConnects != 1 {
		t.Fatalf("ConnectMCP ran %d times, want once", h.engine.mcpConnects)
	}
	for id, want := range map[int64][]string{own: {"duncan"}, specialist: {"duncan"}, plain: nil, h.super.ID: nil} {
		if got := h.engine.mcpTools[id]; !slices.Equal(got, want) {
			t.Errorf("session %d given the MCP tools of %v, want %v", id, got, want)
		}
	}
}

// A session is given its profile's MCP tools once it is recorded, with no
// pass: one the engine made before a server listed new tools, and
// recorded after the pass read the sessions, gets them all the same
// (Codex review of #126, round 1).
func TestANewSessionGetsItsProfilesMCPToolsOnceRecorded(t *testing.T) {
	h := newTeamHarness(t)
	h.engine.mcpProfiles = []string{"duncan"}
	own, plain, specialist := mcpSessions(t, h)
	if h.engine.mcpConnects != 0 {
		t.Fatalf("a pass ran: %d connects", h.engine.mcpConnects)
	}
	for id, want := range map[int64][]string{own: {"duncan"}, specialist: {"duncan"}, plain: nil, h.super.ID: nil} {
		if got := h.engine.mcpTools[id]; !slices.Equal(got, want) {
			t.Errorf("session %d given the MCP tools of %v, want %v", id, got, want)
		}
	}
}

// When a server's tools change, the sessions of the profiles that name it
// get them, in a pass of their own, with no restart (Codex review of
// #126, round 1).
func TestChangedMCPToolsReachTheProfilesSessions(t *testing.T) {
	h := newTeamHarness(t)
	own, plain, specialist := mcpSessions(t, h)
	h.engine.mcpProfiles = []string{"duncan", "vera"}
	h.server.MCPToolsChanged("duncan")
	h.server.passes.Wait()
	if h.engine.mcpConnects != 0 {
		t.Fatalf("a change connected: %d", h.engine.mcpConnects)
	}
	for id, want := range map[int64][]string{own: {"duncan"}, specialist: {"duncan"}, plain: nil} {
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
