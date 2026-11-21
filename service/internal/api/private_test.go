package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Every team tool's call from a private profile's session is refused
// before it runs (ADR-0042), the profile read as it is now; a team
// profile's session, and a plain one, are left to each tool's checks.
func TestATeamCallFromAPrivateSessionIsRefused(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.home, Model: proto.ModelRef{Provider: "faux", ID: "faux-1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{h.super.ID, duncan, plain.ID, 999} {
		if err := h.server.TeamAllows(ctx, id); err != nil {
			t.Fatalf("session %d refused: %v", id, err)
		}
	}
	// duncan becomes private: its session made before is refused at once.
	if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = h.server.TeamAllows(ctx, duncan)
	if err == nil || !strings.HasPrefix(err.Error(), "duncan is private, and team tools are not a private agent's") {
		t.Fatalf("a private agent's call: %v", err)
	}
	if err := h.server.TeamAllows(ctx, h.super.ID); err != nil {
		t.Fatalf("the supervisor refused: %v", err)
	}
}

// A supervisor keeps and lists no lessons for a private agent, whose
// sessions show none (ADR-0042); a team agent's stay its to keep.
func TestASupervisorKeepsNoLessonsForAPrivateAgent(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(h.profileDir, "chani.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Chani\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, action := range []string{team.MemoryAdd, team.MemoryList} {
		m := team.Memory{Action: action, Agent: "chani"}
		if action == team.MemoryAdd {
			m.Text = "Keep the notes short."
		}
		_, err := h.server.TeamMemory(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "m" + string(rune('1'+i))}, m)
		if err == nil || !strings.Contains(err.Error(), "chani is private") {
			t.Fatalf("%s for a private agent: %v", action, err)
		}
	}
	if rows, err := h.store.LessonsOf(ctx, "chani"); err != nil || len(rows) != 0 {
		t.Fatalf("chani's lessons %+v, %v", rows, err)
	}
	if text, err := h.server.TeamMemory(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "m3"}, team.Memory{Action: team.MemoryAdd, Agent: "duncan", Text: "Water at dawn."}); err != nil || text != team.SavedText(1, "Duncan") {
		t.Fatalf("a team agent's lesson: %q, %v", text, err)
	}
}

// A private supervisor's older session gets none of the supervisor's later
// team tools at start (ADR-0042); a team supervisor's does.
func TestAPrivateSupervisorGetsNoLaterTeamTools(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	for name, text := range map[string]string{"leto.md": leto, "alia.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Alia\n"} {
		if err := os.WriteFile(filepath.Join(h.dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	older, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	private, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "alia", Profile: "alia"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.scheduleTool[older.ID] || h.engine.scheduleTool[private.ID] {
		t.Fatalf("the later supervisor tools went to %v", h.engine.scheduleTool)
	}
}
