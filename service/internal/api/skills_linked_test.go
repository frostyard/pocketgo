package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A skill with another name (a hard link) is no skill, to team_gather and
// to a schedule, whose time it misses with why, never kept due: it could
// change through that name, outside the read-only skills folder, as one
// an agent made before the folder was read-only could (ADR-0035; Codex
// review of #63, round 1).
func TestASkillWithAnotherNameIsNoSkill(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming", "agents": ["duncan"]}}]}`)
	ctx := context.Background()
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.engine.mu.Lock()
	h.engine.readErr = fmt.Errorf("read SKILL.md: %w", ErrManyNames)
	h.engine.mu.Unlock()
	if _, err := h.server.TeamGather(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "g1"}, team.Gather{Skill: "grooming"}); err == nil || !strings.Contains(err.Error(), "more than one name") {
		t.Fatalf("team_gather of a skill with another name: %v", err)
	}
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	if row := h.scheduleRow("evening"); row.LastOutcome != store.OutcomeMissed || !strings.Contains(row.LastWhy, "more than one name") {
		t.Fatalf("the time %+v", row)
	}
}
