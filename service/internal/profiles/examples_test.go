package profiles

import (
	"path/filepath"
	"slices"
	"testing"
)

// The repository's example profiles, worker list and schedules, which the
// setup guide tells a new owner to copy, are ones pocketd takes.
func TestTheExampleProfilesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "examples", "profiles")
	profiles, problems, err := Load(dir, "/home/owner")
	if err != nil || len(problems) != 0 {
		t.Fatalf("load example profiles: problems=%+v, error=%v", problems, err)
	}
	var ids []string
	byID := make(map[string]Profile)
	for _, p := range profiles {
		ids = append(ids, p.ID)
		byID[p.ID] = p
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"builder", "supervisor"}) {
		t.Fatalf("example profile ids = %v; want builder and supervisor", ids)
	}
	if p := byID["supervisor"]; p.Role != RoleSupervisor || !p.Standing {
		t.Errorf("example supervisor: role=%s standing=%t; want supervisor and standing", p.Role, p.Standing)
	}
	if byID["builder"].Role == RoleSupervisor {
		t.Error("example builder has the supervisor role")
	}

	workers, err := LoadWorkers(dir)
	if err != nil {
		t.Fatalf("load example workers: %v", err)
	}
	if workers.Default {
		t.Error("example workers use the default list instead of workers.json")
	}
	var models []string
	for _, m := range workers.Models {
		models = append(models, m.String())
	}
	wantModels := []string{"deepseek/deepseek-flash", "anthropic/claude-haiku-5-5"}
	if !slices.Equal(models, wantModels) {
		t.Errorf("example worker models = %v; want %v", models, wantModels)
	}

	schedules, err := LoadSchedules(dir)
	if err != nil || len(schedules.Entries) != 1 {
		t.Fatalf("load example schedules: entries=%+v, error=%v; want one entry", schedules.Entries, err)
	}
	entry := schedules.Entries[0]
	if entry.ID != "morning" || entry.Problem != "" {
		t.Errorf("example schedule: id=%q problem=%q; want morning with no problem", entry.ID, entry.Problem)
	}
	p, ok := byID[entry.Profile]
	if !ok || !p.Standing {
		t.Errorf("example schedule profile %q must name a valid standing profile", entry.Profile)
	}
	if entry.Gather != nil && p.Role != RoleSupervisor {
		t.Errorf("example gather profile %q must be a supervisor", entry.Profile)
	}
}
