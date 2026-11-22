package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A private agent's schedule tool (ADR-0060): team_schedule's engine and
// limits, under a name of its own, for a private profile's session.

// chaniProfile is a private agent's profile with no standing session.
const chaniProfile = "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Chani\n"

// In a private agent's session, schedule add "every sun 9:00" sends that
// same session "[scheduled] <message>" at its time, and on the next
// Sunday again; an add is recorded once per call and answers as it did,
// naming schedule; list shows the session's own and no other session's;
// cancel ends it; the 50 a session hold it too.
func TestAPrivateAgentKeepsItsOwnSchedules(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	if err := os.WriteFile(filepath.Join(h.dir, "chani.md"), []byte(chaniProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	chani := session.ID
	h.server.StartSchedules()
	h.runSchedulePasses()
	call := TeamCall{Session: chani, Task: 1, CallID: "s1"}
	add := schedule.Call{Action: schedule.ActionAdd, When: "every sun 9:00", Message: "Run the memory pulse."}
	text, err := h.server.Schedule(ctx, call, add)
	if err != nil || !strings.HasPrefix(text, "Scheduled S1: ") || !strings.HasSuffix(text, `starts with "[scheduled]"; cancel it with schedule cancel S1.`) {
		t.Fatalf("add: %q %v", text, err)
	}
	h.clock.Advance(time.Hour)
	if again, err := h.server.Schedule(ctx, call, add); err != nil || again != text {
		t.Fatalf("again: %q %v", again, err)
	}
	// The supervisor's own stay its own, and keep their tool's name.
	if got, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "o1"}, schedule.Call{Action: schedule.ActionAdd, When: "in 30m", Message: "Look at the desk."}); err != nil ||
		!strings.HasSuffix(got, "cancel it with team_schedule cancel S2.") {
		t.Fatalf("the supervisor's add: %q %v", got, err)
	}
	list, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 2, CallID: "l1"}, schedule.Call{Action: schedule.ActionList})
	if err != nil || !strings.HasPrefix(list, "S1: ") || !strings.Contains(list, "next Sun 22 Nov 09:00 UTC · Run the memory pulse.") || strings.Contains(list, "Look at the desk.") {
		t.Fatalf("list: %q %v", list, err)
	}
	h.at(time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	if got := h.sentTo(chani, "schedule:"); len(got) != 1 || got[0] != "[scheduled] Run the memory pulse." {
		t.Fatalf("sent on Sunday %q", got)
	}
	h.at(time.Date(2026, 11, 29, 9, 0, 0, 0, time.UTC))
	if got := h.sentTo(chani, "schedule:"); len(got) != 2 || got[1] != "[scheduled] Run the memory pulse." {
		t.Fatalf("sent on the next Sunday %q", got)
	}
	cancel := schedule.Call{Action: schedule.ActionCancel, ID: "S1"}
	if got, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 3, CallID: "c1"}, cancel); err != nil || got != "Cancelled S1: it sends nothing more." {
		t.Fatalf("cancel: %q %v", got, err)
	}
	// Another session's schedule is none of this one's.
	if _, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 3, CallID: "c2"}, schedule.Call{Action: schedule.ActionCancel, ID: "S2"}); err == nil {
		t.Fatal("a private agent cancelled the supervisor's schedule")
	}
	h.at(time.Date(2026, 12, 6, 9, 0, 0, 0, time.UTC))
	if got := h.sentTo(chani, "schedule:"); len(got) != 2 {
		t.Fatalf("sent after the cancel %q", got)
	}
	for i := range schedule.MaxPerSession {
		if _, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 4, CallID: "a" + string(rune('A'+i))}, schedule.Call{Action: schedule.ActionAdd, When: "in 1h", Message: "x"}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 4, CallID: "over"}, schedule.Call{Action: schedule.ActionAdd, When: "in 1h", Message: "x"}); err == nil ||
		err.Error() != "a session keeps at most 50 schedules that go out: cancel one first" {
		t.Fatalf("the 51st: %v", err)
	}
}

// schedule checks its caller again with the profile as it is now and what
// the session works as: a private agent's or a supervisor's session,
// working as no thread's specialist, review, worker, consult or gather
// session; any other is refused and keeps nothing. team_schedule stays
// the supervisor's alone, and every team tool refuses a private agent.
func TestOnlyAPrivateAgentOrTheSupervisorKeepsSchedules(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(h.profileDir, "chani.md"), []byte(chaniProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	chani := session.ID
	duncan := duncanSession(t, h, "d1")
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.home, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "d1"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	add := schedule.Call{Action: schedule.ActionAdd, When: "every sun 9:00", Message: "Run the memory pulse."}
	refused := func(name string, session int64, call string) {
		t.Helper()
		if _, err := h.server.Schedule(ctx, TeamCall{Session: session, Task: 1, CallID: call}, add); err == nil ||
			!strings.HasPrefix(err.Error(), "schedule is a private agent's and the supervisor's alone") {
			t.Errorf("%s: %v", name, err)
		}
		if rows, err := h.store.SessionSchedules(ctx, session); err != nil || len(rows) != 0 {
			t.Errorf("%s kept %+v, %v", name, rows, err)
		}
	}
	allowed := func(name string, session int64, call string) {
		t.Helper()
		if _, err := h.server.Schedule(ctx, TeamCall{Session: session, Task: 1, CallID: call}, add); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	refused("an agent", duncan, "a1")
	refused("a plain session", plain.ID, "a2")
	refused("a thread's specialist", thread.Session, "a3")
	allowed("a private agent", chani, "a4")
	allowed("the supervisor", h.super.ID, "a5")

	// team_schedule stays the supervisor's alone, and a private agent's
	// session never reaches it: every team tool refuses it first.
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: chani, Task: 1, CallID: "t1"}, add); err == nil || !strings.Contains(err.Error(), "only a supervisor keeps schedules") {
		t.Errorf("team_schedule for a private agent: %v", err)
	}
	if err := h.server.TeamAllows(ctx, chani); err == nil {
		t.Error("a team tool runs for a private agent")
	}

	// duncan becomes private: its plain session keeps schedules, its
	// thread's session still does not.
	if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed("a session made private", duncan, "b1")
	refused("a private profile's thread", thread.Session, "b2")

	// chani becomes a team agent: a new add is refused, and an add run
	// again answers as it did.
	if err := os.WriteFile(filepath.Join(h.profileDir, "chani.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Chani\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 1, CallID: "c1"}, add); err == nil {
		t.Error("a team agent's add is taken")
	}
	if text, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 1, CallID: "a4"}, add); err != nil || !strings.HasPrefix(text, "Scheduled ") {
		t.Errorf("an add run again: %q %v", text, err)
	}

	// A profile pocketd cannot read now refuses the call, and keeps
	// nothing: a later call tries again.
	if err := os.WriteFile(filepath.Join(h.profileDir, "chani.md"), []byte(chaniProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	flaky := &unreadableProfiles{Folder: profiles.Folder{Dir: h.profileDir, Home: h.home}, fail: map[string]bool{"chani": true}}
	h.server.SetProfiles(flaky)
	before, err := h.store.SessionSchedules(ctx, chani)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.Schedule(ctx, TeamCall{Session: chani, Task: 1, CallID: "u1"}, add); err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("an unreadable profile: %v", err)
	}
	if after, err := h.store.SessionSchedules(ctx, chani); err != nil || len(after) != len(before) {
		t.Errorf("an unreadable profile kept %d, had %d, %v", len(after), len(before), err)
	}
}

// At start, a private agent's session made before the schedule tool gets
// it, but a standing one, which takes it with its profile, and a thread's
// session of a profile made private; a supervisor's, an agent's and a
// plain session do not (ADR-0060).
func TestOlderPrivateSessionsGetTheScheduleTool(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{big}
	for name, text := range map[string]string{
		"chani.md": chaniProfile,
		"alia.md":  "---\nmodel: faux/faux-big\nhome: ~\nprivate: true\nsession: standing\ncompactAt: 300000\n---\n# Alia\n",
	} {
		if err := os.WriteFile(filepath.Join(h.profileDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	chani, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	alia, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "alia", Profile: "alia"})
	if err != nil || !alia.Standing {
		t.Fatalf("a private standing session %+v, %v", alia, err)
	}
	duncan := duncanSession(t, h, "d1")
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.home, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "d1"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// duncan becomes private after its thread started.
	if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for name, tc := range map[string]struct {
		id   int64
		want bool
	}{
		"a private agent's":          {chani.ID, true},
		"a session made private":     {duncan, true},
		"a private standing one":     {alia.ID, false},
		"a private profile's thread": {thread.Session, false},
		"the supervisor's":           {h.super.ID, false},
		"a plain session":            {plain.ID, false},
	} {
		if h.engine.ownScheduleTool[tc.id] != tc.want {
			t.Errorf("%s session given the schedule tool: %v, want %v", name, h.engine.ownScheduleTool[tc.id], tc.want)
		}
	}
	if h.engine.scheduleTool[chani.ID] {
		t.Error("a private agent's session was given the supervisor's later tools")
	}
}

// A private agent's profile that cannot be read at start gets its older
// session the schedule tool once it can, by a later schedules pass, as a
// supervisor's later tools are tried again.
func TestAPrivateScheduleToolUpgradeIsTriedAgain(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	if err := os.WriteFile(filepath.Join(h.dir, "chani.md"), []byte(chaniProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	older, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	flaky := &unreadableProfiles{Folder: profiles.Folder{Dir: h.dir, Home: h.home}, fail: map[string]bool{"chani": true}}
	h.server.SetProfiles(flaky)
	if err := h.server.UpgradeTeamTools(ctx); err == nil {
		t.Fatal("an upgrade with an unreadable profile succeeded")
	}
	h.engine.mu.Lock()
	upgraded := h.engine.ownScheduleTool[older.ID]
	h.engine.mu.Unlock()
	if upgraded {
		t.Fatal("upgraded with no profile read")
	}
	flaky.mu.Lock()
	flaky.fail = nil
	flaky.mu.Unlock()
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.ownScheduleTool[older.ID] {
		t.Fatalf("not upgraded once the profile reads: %v", h.engine.ownScheduleTool)
	}
	h.server.scheduleMu.Lock()
	defer h.server.scheduleMu.Unlock()
	if h.server.toolsDue {
		t.Fatal("the upgrade is still due once it went")
	}
}

// A schedule a private agent's session keeps lists as a session's own in
// GET /v1/schedules, with the session it goes to.
func TestAPrivateAgentsScheduleIsListed(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	if err := os.WriteFile(filepath.Join(h.dir, "chani.md"), []byte(chaniProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chani, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.Schedule(ctx, TeamCall{Session: chani.ID, Task: 1, CallID: "s1"}, schedule.Call{Action: schedule.ActionAdd, When: "every sun 9:00", Message: "Run the memory pulse."}); err != nil {
		t.Fatal(err)
	}
	list, err := h.client.Schedules(ctx)
	if err != nil || len(list.Schedules) != 1 {
		t.Fatalf("the list %+v, %v", list, err)
	}
	if s := list.Schedules[0]; s.ID != "S1" || s.Source != proto.ScheduleSession || s.Session != chani.ID || s.Message != "Run the memory pulse." || s.Repeat == "" {
		t.Fatalf("a private agent's schedule %+v", s)
	}
	if row := h.scheduleRow("S1"); row.State != store.ScheduleActive || row.SessionID != chani.ID {
		t.Fatalf("its record %+v", row)
	}
}
