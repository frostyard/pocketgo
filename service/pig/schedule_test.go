package pig

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/store/storetest"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A private agent's schedule tool (ADR-0060).

// A private profile's session gets schedule after question, and no team
// tool; a team profile's session beside it gets no schedule: the
// supervisor keeps team_schedule as it was, and a specialist, an agent
// and a private profile's thread session get neither.
func TestOnlyAPrivateAgentsSessionGetsTheScheduleTool(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	chani := &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Private: true, Home: f.work, Tools: []string{"read"}}
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Tools: []string{"read"}}
	for name, tc := range map[string]struct {
		spec api.SessionSpec
		want []string
	}{
		"a private agent": {api.SessionSpec{Profile: chani}, []string{"read", toolQuestion, toolOwnSchedule}},
		"the supervisor": {api.SessionSpec{Profile: odrade},
			[]string{"read", toolDelegate, toolClose, toolSchedule, toolSend, toolGather, toolBoard, toolLaunch, toolInbox, toolMemory, toolQuestion}},
		"an agent":                      {api.SessionSpec{Profile: vera}, []string{"read", toolMemory}},
		"a thread's specialist":         {api.SessionSpec{Profile: vera, Thread: 3}, []string{"read", toolMessage, toolWorker, toolMemory}},
		"a private thread's specialist": {api.SessionSpec{Profile: chani, Thread: 4}, []string{"read"}},
		"a plain session":               {api.SessionSpec{}, []string{"read", "write", "edit", "bash"}},
	} {
		tc.spec.Cwd, tc.spec.Model = f.work, fauxRef
		id, err := e.CreateSession(ctx, name, tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, tc.want) {
			t.Errorf("%s: tools %q, want %q", name, got, tc.want)
		}
	}
}

// schedule reaches the team's Schedule, keyed by the calling session, its
// task and the call's id, and no team check runs before it: it is no team
// tool, and a refusal of every team tool leaves it working.
func TestTheScheduleToolCallsTheTeamWithNoTeamCheck(t *testing.T) {
	f := newFixture(t)
	args := map[string]any{"action": "add", "when": "every sun 9:00", "message": "Run the memory pulse."}
	e := f.open(fauxModels(toolTurn(toolOwnSchedule, args, "s1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{refuse: errors.New("chani is private"), scheduleResult: "Scheduled S1."}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Private: true, Home: f.work, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "pulse", "keep a weekly pulse"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	want := schedule.Call{Action: schedule.ActionAdd, When: "every sun 9:00", Message: "Run the memory pulse."}
	if len(fake.calls) != 1 || fake.calls[0].Session != id || fake.calls[0].CallID != "s1" || fake.calls[0].Task == 0 || len(fake.own) != 1 || fake.own[0] != want {
		t.Fatalf("calls %+v %+v", fake.calls, fake.own)
	}
	if len(fake.schedules) != 0 || len(fake.allowed) != 0 {
		t.Fatalf("team_schedule calls %+v, team checks %v", fake.schedules, fake.allowed)
	}
	if got := lastToolResult(t, e, id); got.IsError || got.Text != "Scheduled S1." {
		t.Fatalf("result %+v", got)
	}
}

// asMadeBefore takes tools and their extensions from session id, as a
// session made before them was.
func asMadeBefore(t *testing.T, e *Engine, id int64, tools []string, extensions ...string) {
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
	older := slices.DeleteFunc(slices.Clone(agent.Tools), func(r *durable.ToolRegistration) bool { return slices.Contains(tools, r.Name) })
	kept := slices.DeleteFunc(slices.Clone(agent.Extensions), func(x *durable.Extension) bool { return slices.Contains(extensions, x.Name) })
	if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: older}), Extensions: harness.SetTo(harness.ExtensionChange{List: kept})}); err != nil {
		t.Fatal(err)
	}
}

// A private agent's session made before the schedule tool gets it, and
// its extension, in the place a new session has it, once. One made before
// the question tool too ends with question and schedule in that order,
// whichever upgrade runs first, and one of no tool at all (tools: [])
// gets the tool alone.
func TestSetScheduleToolGivesItToAnOlderPrivateSession(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	chani := &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Private: true, Home: f.work, Tools: []string{"read", "bash"}}
	want := []string{"read", "bash", toolQuestion, toolOwnSchedule}
	for _, way := range []string{"schedule only", "schedule first", "question first"} {
		id, err := e.CreateSession(ctx, way, api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
		if err != nil {
			t.Fatal(err)
		}
		if way == "schedule only" {
			asMadeBefore(t, e, id, []string{toolOwnSchedule}, e.scheduleExt.Name)
		} else {
			asMadeBefore(t, e, id, []string{toolOwnSchedule, toolQuestion}, e.scheduleExt.Name, e.questionExt.Name)
		}
		if got := agentTools(t, e, id); slices.Contains(got, toolOwnSchedule) {
			t.Fatalf("%s: the older session's tools %q", way, got)
		}
		upgrades := []func(context.Context, int64) (bool, error){e.SetScheduleTool, e.SetQuestionTool}
		switch way {
		case "schedule only":
			upgrades = upgrades[:1]
		case "question first":
			slices.Reverse(upgrades)
		}
		for _, upgrade := range upgrades {
			if changed, err := upgrade(ctx, id); err != nil || !changed {
				t.Fatalf("%s: the upgrade: %v, %v", way, changed, err)
			}
		}
		if got := agentTools(t, e, id); !slices.Equal(got, want) {
			t.Fatalf("%s: tools %q, want %q", way, got, want)
		}
		conversation, _ := e.conversation(ctx, id)
		agent, _ := conversation.Agent(ctx)
		if !slices.Contains(extensionNames(agent.Extensions), e.scheduleExt.Name) {
			t.Fatalf("%s: extensions %q", way, extensionNames(agent.Extensions))
		}
		if changed, err := e.SetScheduleTool(ctx, id); err != nil || changed {
			t.Fatalf("%s: again: %v, %v", way, changed, err)
		}
	}
	none := &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Private: true, Home: f.work, Tools: []string{}}
	id, err := e.CreateSession(ctx, "no tools", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: none})
	if err != nil {
		t.Fatal(err)
	}
	asMadeBefore(t, e, id, []string{toolQuestion, toolOwnSchedule}, e.scheduleExt.Name)
	if changed, err := e.SetScheduleTool(ctx, id); err != nil || !changed {
		t.Fatalf("no tools: %v, %v", changed, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, []string{toolOwnSchedule}) {
		t.Fatalf("no tools: %q, want the schedule tool alone", got)
	}
}

// A standing session whose profile became private takes schedule with
// its profile at its refresh and at Start Over, and gives up
// team_schedule; once a team supervisor's again, it takes team_schedule
// back and gives up schedule.
func TestAStandingSessionTakesTheScheduleToolWithItsProfile(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Home: f.work, Identity: "# Odrade", Standing: true, Tools: []string{"read"}}
	private := *odrade
	private.Private = true
	for _, way := range []string{"refresh", "start over"} {
		id, err := e.CreateSession(ctx, "standing:odrade:"+way, api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade})
		if err != nil {
			t.Fatal(err)
		}
		take := func(p *profiles.Profile, key string) {
			t.Helper()
			spec := api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: p}
			var did api.Reconfigured
			if way == "refresh" {
				did, err = e.Reconfigure(ctx, id, spec)
			} else {
				did, err = e.StartOver(ctx, id, key, &spec)
			}
			if err != nil || did != api.ReconfigureApplied {
				t.Fatalf("%s: %v, %v", way, did, err)
			}
		}
		take(&private, "over-1")
		if got := agentTools(t, e, id); !slices.Equal(got, []string{"read", toolQuestion, toolOwnSchedule}) {
			t.Fatalf("%s: a private standing session's tools %q", way, got)
		}
		take(odrade, "over-2")
		if got := agentTools(t, e, id); !slices.Contains(got, toolSchedule) || slices.Contains(got, toolOwnSchedule) {
			t.Fatalf("%s: a team supervisor's standing session's tools %q", way, got)
		}
	}
}

// Over HTTP, with pocketd's own team and the real engine: in a private
// agent's session, schedule add "every sun 9:00" sends that same session
// "[scheduled] <message>" at its time; a question in that run is refused,
// since nobody watches it (ADR-0048); list and cancel work. The team
// profiles beside it keep their tools: the supervisor team_schedule, an
// agent neither.
func TestAPrivateScheduleEndToEndOverHTTP(t *testing.T) {
	ctx := context.Background()
	dataDir, profileDir, home := t.TempDir(), t.TempDir(), t.TempDir()
	for name, text := range map[string]string{
		"odrade.md": "---\nmodel: faux/faux-1\nrole: supervisor\nhome: ~\ntools: [read]\n---\n# Odrade\n",
		"chani.md":  "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\ntools: [read]\n---\n# Chani\n",
		"duncan.md": "---\nmodel: faux/faux-1\nhome: ~\ntools: [read]\n---\n# Duncan\n",
	} {
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := storetest.Open(t, filepath.Join(dataDir, "work.sqlite"), nil)
	defer func() { _ = s.Close() }()
	_, token, err := auth.CreateDevice(ctx, s, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := events.NewHub(1024, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Saturday noon, the day before the pulse's first time.
	var clockMu sync.Mutex
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	server := api.New(s, hub, "e2e", clock, nil)
	server.SetZone(time.UTC)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	server.SetProfiles(profiles.Folder{Dir: profileDir, Home: home})
	pulse := map[string]any{"action": "add", "when": "every sun 9:00", "message": "Run the memory pulse."}
	engine, err := open(ctx, Options{
		Protected: []string{dataDir},
		DataDir:   dataDir, Approver: manager, NeedsApproval: approvals.NeedsApproval, Unsandboxed: true,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	}, fauxModels(
		toolTurn(toolOwnSchedule, pulse, "s1"), answer("The pulse is set."),
		toolTurn(toolQuestion, map[string]any{"question": "Prune the notes?"}, "q1"), answer("Pulse done."),
		toolTurn(toolOwnSchedule, map[string]any{"action": "list"}, "l1"),
		toolTurn(toolOwnSchedule, map[string]any{"action": "cancel", "id": "S1"}, "c1"), answer("Cancelled."),
	))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	server.SetEngine(engine)
	engine.SetTeam(server)
	engine.Resume()
	defer server.Stop()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, _ := proto.NewClient(httpServer.URL, token, nil)

	sessions := map[string]int64{}
	for _, profile := range []string{"odrade", "chani", "duncan"} {
		session, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: profile})
		if err != nil {
			t.Fatal(err)
		}
		sessions[profile] = session.ID
	}
	chani := sessions["chani"]
	if got := agentTools(t, engine, chani); !slices.Equal(got, []string{"read", toolQuestion, toolOwnSchedule}) {
		t.Fatalf("a private agent's tools %q", got)
	}
	if got := agentTools(t, engine, sessions["odrade"]); !slices.Contains(got, toolSchedule) || slices.Contains(got, toolOwnSchedule) {
		t.Fatalf("the supervisor's tools %q", got)
	}
	if got := agentTools(t, engine, sessions["duncan"]); slices.Contains(got, toolSchedule) || slices.Contains(got, toolOwnSchedule) {
		t.Fatalf("an agent's tools %q", got)
	}

	if _, err := client.Send(ctx, chani, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "Keep the weekly memory pulse."}); err != nil {
		t.Fatal(err)
	}
	if err := engine.WaitIdle(ctx, chani); err != nil {
		t.Fatal(err)
	}
	if got := lastToolResult(t, engine, chani); got.IsError || !strings.HasPrefix(got.Text, "Scheduled S1: ") ||
		!strings.HasSuffix(got.Text, "; cancel it with schedule cancel S1.") {
		t.Fatalf("the add's result %+v", got)
	}
	list, err := client.Schedules(ctx)
	if err != nil || len(list.Schedules) != 1 || list.Schedules[0].Session != chani || list.Schedules[0].Source != proto.ScheduleSession ||
		list.Schedules[0].Next == nil || !list.Schedules[0].Next.Equal(time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("the schedules %+v, %v", list, err)
	}

	// Sunday 9:00: the schedules pass files the pulse once, then the
	// delivery pass places it in the same session. The test waits for the
	// pass's schedules event, which follows the filing, and runs the
	// delivery pass itself: no timer of the server's decides when the
	// message goes (Codex review of #120, round 1).
	_, _, stream, cancel := hub.Subscribe(0)
	defer cancel()
	clockMu.Lock()
	now = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	clockMu.Unlock()
	server.StartSchedules()
	deadline := time.After(20 * time.Second)
	for filed := false; !filed; {
		select {
		case e := <-stream:
			if e.Type != proto.EventSchedules {
				continue
			}
			list, err := client.Schedules(ctx)
			if err != nil {
				t.Fatal(err)
			}
			filed = len(list.Schedules) == 1 && list.Schedules[0].Runs == 1
		case <-deadline:
			t.Fatal("the schedules pass filed no pulse")
		}
	}
	if err := server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.WaitIdle(ctx, chani); err != nil {
		t.Fatal(err)
	}
	entries, _, err := engine.Transcript(ctx, chani)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e proto.Entry) bool {
		return e.Kind == proto.EntryUser && e.Text == "[scheduled] Run the memory pulse."
	}) {
		t.Fatal("the scheduled message is not in the session")
	}
	if got := lastToolResult(t, engine, chani); !got.IsError || got.Text != team.ErrNobodyWatching.Error() {
		t.Fatalf("a scheduled run's question %+v", got)
	}

	if _, err := client.Send(ctx, chani, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "List the pulse, then cancel it."}); err != nil {
		t.Fatal(err)
	}
	if err := engine.WaitIdle(ctx, chani); err != nil {
		t.Fatal(err)
	}
	entries, _, err = engine.Transcript(ctx, chani)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, entry := range entries {
		if entry.ToolResult != nil {
			results = append(results, entry.ToolResult.Text)
		}
	}
	if n := len(results); n < 2 || !strings.HasPrefix(results[n-2], "S1: ") || !strings.Contains(results[n-2], "Run the memory pulse.") ||
		results[n-1] != "Cancelled S1: it sends nothing more." {
		t.Fatalf("list and cancel %q", results)
	}
	if row, err := s.Schedule(ctx, 1); err != nil || row.SessionID != chani || row.State != store.ScheduleCancelled || row.Runs != 1 {
		t.Fatalf("the schedule after its cancel %+v, %v", row, err)
	}
}
