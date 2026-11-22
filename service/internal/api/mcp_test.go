package api

import (
	"context"
	"slices"
	"testing"
	"time"

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

// A plan's lead takes no MCP tool, as the work database records its
// thread, however its session's spec was made again: the record of its
// session takes any it has, and so does every pass (Codex review of #126,
// round 2).
func TestAPlansLeadTakesNoMCPTool(t *testing.T) {
	h := newTeamHarness(t)
	h.engine.mcpProfiles = []string{"duncan"}
	ctx := context.Background()
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "plan"},
		team.Delegation{Agent: "duncan", Task: "Plan the signing of the ISO.", Title: "Signing", Plan: &team.PlanRef{Start: true}}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !threadRole(thread).Plan {
		t.Fatalf("thread %+v is no plan's", thread)
	}
	h.server.StartMCP()
	h.server.passes.Wait()
	if got := h.engine.mcpTools[thread.Session]; !slices.Equal(got, []string{"", ""}) {
		t.Fatalf("the lead was given the MCP tools of %q", got)
	}
}

// A pass that failed for a session is tried again later, waiting longer
// each time, with no new listing to start it (Codex review of #126,
// round 3).
func TestAFailedMCPPassIsTriedAgain(t *testing.T) {
	h := newTeamHarness(t)
	own, _, _ := mcpSessions(t, h)
	h.engine.mcpProfiles = []string{"duncan"}
	h.engine.mcpFails = 3
	var waits []time.Duration
	var runs []func()
	h.server.mcpTimer = func(d time.Duration, f func()) func() {
		waits, runs = append(waits, d), append(runs, f)
		return func() {}
	}
	h.server.MCPToolsChanged("duncan")
	h.server.passes.Wait()
	if len(runs) != 1 || len(h.engine.mcpTools[own]) != 0 {
		t.Fatalf("retries %v, tools %v", waits, h.engine.mcpTools[own])
	}
	runs[0]()
	h.server.passes.Wait()
	if len(runs) != 2 || !slices.Equal(waits, []time.Duration{mcpRetryFirst, 2 * mcpRetryFirst}) {
		t.Fatalf("retries %v", waits)
	}
	runs[1]()
	h.server.passes.Wait()
	if got := h.engine.mcpTools[own]; len(got) == 0 || got[len(got)-1] != "duncan" || len(runs) != 2 {
		t.Fatalf("tools %v after %d retries", got, len(runs))
	}
}

// A new session's MCP tools are written under mcpMu, as a pass writes
// them: the two never write one session's tools past one another (Codex
// review of #126, round 2).
func TestANewSessionsMCPToolsAreWrittenUnderTheLockOfThePasses(t *testing.T) {
	h := newTeamHarness(t)
	h.engine.mcpProfiles = []string{"duncan"}
	held := true
	h.engine.onSetMCPTools = func() {
		if h.server.mcpMu.TryLock() {
			held = false
			h.server.mcpMu.Unlock()
		}
	}
	if _, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "own", Profile: "duncan"}); err != nil {
		t.Fatal(err)
	}
	if !held || len(h.engine.mcpTools) != 1 {
		t.Fatalf("a new session's MCP tools were written outside mcpMu (held %v, %v)", held, h.engine.mcpTools)
	}
}

// fakeMCPTimers holds the retries the server schedules, to run by hand.
type fakeMCPTimers struct {
	waits   []time.Duration
	runs    []func()
	stopped int
}

func (f *fakeMCPTimers) timer(d time.Duration, run func()) func() {
	f.waits, f.runs = append(f.waits, d), append(f.runs, run)
	return func() { f.stopped++ }
}

// A new session whose MCP tools could not be written has its profile's
// pass tried again later: no other pass may come (Codex review of #126,
// round 4).
func TestANewSessionsFailedMCPToolsAreTriedAgain(t *testing.T) {
	h := newTeamHarness(t)
	h.engine.mcpProfiles = []string{"duncan"}
	h.engine.mcpFails = 1
	timers := &fakeMCPTimers{}
	h.server.mcpTimer = timers.timer
	own, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "own", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(timers.runs) != 1 || len(h.engine.mcpTools[own.ID]) != 0 {
		t.Fatalf("retries %v, tools %v", timers.waits, h.engine.mcpTools[own.ID])
	}
	timers.runs[0]()
	h.server.passes.Wait()
	if got := h.engine.mcpTools[own.ID]; !slices.Equal(got, []string{"duncan"}) {
		t.Fatalf("tools %v", got)
	}
}

// A scope waits on one retry at most, however many passes fail, and a
// pass that succeeds ends the retry it waits on (Codex review of #126,
// round 4).
func TestMCPRetriesAreOneAScope(t *testing.T) {
	h := newTeamHarness(t)
	mcpSessions(t, h)
	h.engine.mcpProfiles = []string{"duncan"}
	h.engine.mcpFails = 4
	timers := &fakeMCPTimers{}
	h.server.mcpTimer = timers.timer
	h.server.MCPToolsChanged("duncan")
	h.server.passes.Wait()
	h.server.MCPToolsChanged("duncan")
	h.server.passes.Wait()
	if len(timers.runs) != 1 {
		t.Fatalf("%d retries for one scope", len(timers.runs))
	}
	h.server.MCPToolsChanged("duncan")
	h.server.passes.Wait()
	h.server.sessionMu.Lock()
	pending := len(h.server.mcpRetries)
	h.server.sessionMu.Unlock()
	if pending != 0 || timers.stopped != 1 {
		t.Fatalf("after a pass that succeeded: %d pending, %d stopped", pending, timers.stopped)
	}
}
