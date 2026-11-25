package pig

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/store/storetest"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Private agents (ADR-0042): a private profile's sessions are the owner's
// alone. Each test sets a private profile next to a team profile, which
// stays as it was.

// sessionInstructionsOf are session id's instructions as PiG keeps them.
func sessionInstructionsOf(t *testing.T, e *Engine, id int64) string {
	t.Helper()
	conversation, err := e.conversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if agent.Instructions == nil {
		return ""
	}
	return *agent.Instructions
}

// A private profile's session has the private preamble, its profile, and
// nothing of the team: no roster, no lessons section, no team tool, even
// when its spec carries them. A team profile's session beside it keeps
// the team's preamble, its lessons and team_memory.
func TestAPrivateProfilesSessionHasNoTeam(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	section := team.LessonsSection([]team.Lesson{{ID: 1, Text: "Ask before merging."}})
	chani := &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Private: true, Home: f.work,
		Identity: "# Chani\n\nShe keeps the owner's notes.", Tools: []string{"read", "bash"}}
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Identity: "# Vera\n\nShe tidies the example repository.",
		Tools: []string{"read", "bash"}}
	private, err := e.CreateSession(ctx, "chani", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani, Roster: "Delegate to vera.", Lessons: section})
	if err != nil {
		t.Fatal(err)
	}
	want := instructions(f.work) + "\n\nYou are Chani, the owner's personal agent: you work for the owner directly, in sessions the owner starts. " +
		"You have no supervisor, no team and no desk. Your profile follows.\n\n# Chani\n\nShe keeps the owner's notes."
	if got := sessionInstructionsOf(t, e, private); got != want {
		t.Fatalf("a private agent's instructions:\n%s", got)
	}
	if got := agentTools(t, e, private); !slices.Equal(got, []string{"read", "bash", toolQuestion, toolOwnSchedule}) {
		t.Fatalf("a private agent's tools %q, want its profile's, question (ADR-0048) and schedule (ADR-0060)", got)
	}
	teamID, err := e.CreateSession(ctx, "vera", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: vera, Lessons: section})
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionInstructionsOf(t, e, teamID); got != instructions(f.work)+"\n\nYou are Vera, the team's agent. Your profile follows.\n\n"+vera.Identity+section {
		t.Fatalf("a team agent's instructions:\n%s", got)
	}
	if got := agentTools(t, e, teamID); !slices.Equal(got, []string{"read", "bash", toolWork, toolMemory}) {
		t.Fatalf("a team agent's tools %q", got)
	}
	// A private supervisor, or a private profile with every tool, has no
	// team tool either.
	for name, p := range map[string]*profiles.Profile{
		"a private supervisor":         {ID: "leto", Name: "Leto", Role: profiles.RoleSupervisor, Private: true, Home: f.work},
		"a private agent of all tools": {ID: "alia", Name: "Alia", Role: profiles.RoleAgent, Private: true, Home: f.work},
	} {
		id, err := e.CreateSession(ctx, name, api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: p, Roster: "Delegate to vera."})
		if err != nil {
			t.Fatal(err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, []string{"read", "write", "edit", "bash", toolQuestion, toolOwnSchedule}) {
			t.Errorf("%s: tools %q", name, got)
		}
		if got := sessionInstructionsOf(t, e, id); strings.Contains(got, "Delegate to vera.") || strings.Contains(got, "the team's") {
			t.Errorf("%s: instructions\n%s", name, got)
		}
	}
}

// A standing session whose profile became private takes the private
// preamble at its refresh and at Start Over, and loses its roster, its
// lessons section and its team tools, which its profile's change alone
// would have kept.
func TestAStandingSessionMadePrivateLeavesTheTeam(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	section := team.LessonsSection([]team.Lesson{{ID: 1, Text: "Ask before merging."}})
	chani := &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleSupervisor, Home: f.work, Identity: "# Chani\nShe keeps the notes.", Standing: true}
	private := *chani
	private.Private = true
	want := instructions(f.work) + "\n\n" + team.PrivateIntro("Chani") + "\n\n" + chani.Identity
	for _, way := range []string{"refresh", "start over"} {
		id, err := e.CreateSession(ctx, "standing:chani:"+way, api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani, Roster: "Delegate to vera.", Lessons: section})
		if err != nil {
			t.Fatal(err)
		}
		if got := agentTools(t, e, id); !slices.Contains(got, toolMemory) || !slices.Contains(got, toolDelegate) {
			t.Fatalf("%s: a team supervisor's tools %q", way, got)
		}
		spec := api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &private, Roster: "Delegate to vera."}
		var did api.Reconfigured
		if way == "refresh" {
			did, err = e.Reconfigure(ctx, id, spec)
		} else {
			did, err = e.StartOver(ctx, id, "over-1", &spec)
		}
		if err != nil || did != api.ReconfigureApplied {
			t.Fatalf("%s: %v, %v", way, did, err)
		}
		if got := sessionInstructionsOf(t, e, id); got != want {
			t.Fatalf("%s: instructions\n%s", way, got)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, []string{"read", "write", "edit", "bash", toolQuestion, toolOwnSchedule}) {
			t.Fatalf("%s: tools %q", way, got)
		}
		// The same private profile again changes nothing: no lessons
		// section comes back.
		if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != api.ReconfigureSame {
			t.Fatalf("%s: the same again: %v %v", way, did, err)
		}
	}
}

// A standing session whose profile had a tool filter, and became private
// with none, gets every coding tool at its refresh and at Start Over, and
// no team tool: a standing session takes a tool list only when it is one,
// so a private profile's list is explicit (cross-review of #84, round 1).
func TestAStandingSessionMadePrivateWithoutAFilterGetsEveryTool(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	chani := &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleSupervisor, Home: f.work, Identity: "# Chani", Standing: true, Tools: []string{"read"}}
	private := *chani
	private.Private, private.Tools = true, nil
	for _, way := range []string{"refresh", "start over"} {
		id, err := e.CreateSession(ctx, "standing:chani:"+way, api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani, Roster: "Delegate to vera."})
		if err != nil {
			t.Fatal(err)
		}
		if got := agentTools(t, e, id); slices.Contains(got, "bash") || !slices.Contains(got, toolDelegate) {
			t.Fatalf("%s: a filtered team supervisor's tools %q", way, got)
		}
		spec := api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &private}
		var did api.Reconfigured
		if way == "refresh" {
			did, err = e.Reconfigure(ctx, id, spec)
		} else {
			did, err = e.StartOver(ctx, id, "over-1", &spec)
		}
		if err != nil || did != api.ReconfigureApplied {
			t.Fatalf("%s: %v, %v", way, did, err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, []string{"read", "write", "edit", "bash", toolQuestion, toolOwnSchedule}) {
			t.Fatalf("%s: tools %q, want every coding tool and no team tool", way, got)
		}
	}
}

// The team refuses a team tool's call before the tool runs: the agent
// reads the refusal, and the tool's work never starts.
func TestARefusedTeamCallNeverRuns(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolMemory, map[string]any{"action": "add", "text": "Run the tests."}, "m1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{refuse: errors.New("chani is private")}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "remember", "remember"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := lastToolResult(t, e, id); !got.IsError || got.Text != "chani is private" {
		t.Fatalf("result %+v", got)
	}
	if len(fake.memory) != 0 || !slices.Equal(fake.allowed, []int64{id}) {
		t.Fatalf("memory %+v, asked about %v", fake.memory, fake.allowed)
	}
}

// lastToolResult is session id's last tool result.
func lastToolResult(t *testing.T, e *Engine, id int64) *proto.ToolResult {
	t.Helper()
	entries, _, err := e.Transcript(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ToolResult != nil {
			return entries[i].ToolResult
		}
	}
	t.Fatal("no tool result")
	return nil
}

// Over HTTP, with pocketd's own team: a private profile's new session has
// the private preamble, no lessons section and no team_memory, and a
// lesson approved for it reaches none of its sessions. A team profile's
// session made before its profile became private is refused team_memory
// at once, and loses its lessons section at the next refresh.
func TestAPrivateProfileEndToEndOverHTTP(t *testing.T) {
	ctx := context.Background()
	dataDir, profileDir, home := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("chani.md", "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Chani\n\nShe keeps the owner's notes.\n")
	write("duncan.md", "---\nmodel: faux/faux-1\nhome: ~\n---\n# Duncan\n\nHe tends the garden.\n")
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
	server := api.New(s, hub, "e2e", nil, nil)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	server.SetProfiles(profiles.Folder{Dir: profileDir, Home: home})
	engine, err := open(ctx, Options{
		Protected: []string{dataDir},
		DataDir:   dataDir, Approver: manager, NeedsApproval: approvals.NeedsApproval, Unsandboxed: true,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	}, fauxModels(toolTurn(toolMemory, map[string]any{"action": "add", "text": "Water the roses at dawn."}, "m1"), answer("I cannot keep it.")))
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

	chani, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	duncan, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	private := instructions(home) + "\n\n" + team.PrivateIntro("Chani") + "\n\n# Chani\n\nShe keeps the owner's notes."
	if got := sessionInstructionsOf(t, engine, chani.ID); got != private {
		t.Fatalf("a private agent's instructions:\n%s", got)
	}
	if got := agentTools(t, engine, chani.ID); slices.Contains(got, toolMemory) {
		t.Fatalf("a private agent's tools %q", got)
	}
	if got := sessionInstructionsOf(t, engine, duncan.ID); !strings.Contains(got, "You are Duncan, the team's agent.") || !strings.Contains(got, profiles.LessonsMarker) {
		t.Fatalf("a team agent's instructions:\n%s", got)
	}
	if got := agentTools(t, engine, duncan.ID); !slices.Contains(got, toolMemory) {
		t.Fatalf("a team agent's tools %q", got)
	}

	// A lesson for the private agent, kept before it was private, reaches
	// none of its sessions once approved; the team agent's does.
	for _, agent := range []string{"chani", "duncan"} {
		row, _, err := s.AddLesson(ctx, store.LessonRow{Key: "lesson:" + agent, Agent: agent, Text: "Look twice.", Writer: "odrade"}, team.CheckPending)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.DecideLesson(ctx, row.ID, proto.DecideLesson{RequestID: proto.NewRequestID(), Decision: proto.LessonApprove, Text: row.Text}); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sessionInstructionsOf(t, engine, chani.ID); got != private {
		t.Fatalf("a private agent's instructions after a lesson's approval:\n%s", got)
	}
	if got := sessionInstructionsOf(t, engine, duncan.ID); !strings.Contains(got, "] Look twice.") {
		t.Fatalf("a team agent's instructions after a lesson's approval:\n%s", got)
	}

	// The team agent becomes private: its older session still lists
	// team_memory, and the call is refused before it keeps anything.
	write("duncan.md", "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n\nHe tends the garden.\n")
	if _, err := client.Send(ctx, duncan.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "Remember the roses."}); err != nil {
		t.Fatal(err)
	}
	if err := engine.WaitIdle(ctx, duncan.ID); err != nil {
		t.Fatal(err)
	}
	if got := lastToolResult(t, engine, duncan.ID); !got.IsError || !strings.Contains(got.Text, "duncan is private, and team tools are not a private agent's") {
		t.Fatalf("team_memory in a private agent's older session: %+v", got)
	}
	lessons, err := client.Lessons(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lessons {
		if strings.Contains(l.Text, "roses") {
			t.Fatalf("a private agent kept a lesson: %+v", l)
		}
	}
	// pocketd's next start refreshes every session's lessons: the private
	// agent's older session loses its section and keeps its identity, as
	// it was written when the session was a team agent's, team_work's
	// guide included (ADR-0073), whose tool every call refuses now.
	server.RefreshAllLessons()
	if err := server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sessionInstructionsOf(t, engine, duncan.ID); strings.Contains(got, profiles.LessonsMarker) || !strings.HasSuffix(got, "He tends the garden.\n\n"+team.WorkGuide) {
		t.Fatalf("a private agent's older session after a refresh:\n%s", got)
	}
}
