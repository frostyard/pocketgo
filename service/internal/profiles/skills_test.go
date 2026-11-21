package profiles

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A schedule's action is a message or a gather, never both: a gather names
// a skill by its folder, distinct agents and a note (ADR-0033).
func TestAScheduleGathers(t *testing.T) {
	got, err := ParseSchedules([]byte(`{"schedules": [
		{"id": "evening-tue", "when": "every tue,fri 19:00", "profile": "supervisor", "gather": {"skill": "code-grooming", "agents": ["duncan", "vera"], "note": "Mind the CI."}},
		{"id": "evening", "when": "every day 19:00", "profile": "supervisor", "gather": {"skill": "proposals"}}
	]}`))
	if err != nil || len(got.Entries) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	first := got.Entries[0]
	if first.Problem != "" || first.Gather == nil || first.Gather.Skill != "code-grooming" || strings.Join(first.Gather.Agents, ",") != "duncan,vera" || first.Gather.Note != "Mind the CI." || first.Message != "" {
		t.Errorf("the first entry %+v %+v", first, first.Gather)
	}
	if second := got.Entries[1]; second.Problem != "" || second.Gather == nil || len(second.Gather.Agents) != 0 {
		t.Errorf("the second entry %+v", second)
	}
	base := `"id": "e", "when": "every day 19:00", "profile": "supervisor"`
	for name, tc := range map[string]struct{ entry, want string }{
		"both":            {base + `, "message": "m", "gather": {"skill": "s"}`, "two actions"},
		"no skill":        {base + `, "gather": {}`, "no skill's name"},
		"a bad skill":     {base + `, "gather": {"skill": "../s"}`, "no skill's name"},
		"another field":   {base + `, "gather": {"skill": "s", "kind": "x"}`, `the field "kind"`},
		"an agent twice":  {base + `, "gather": {"skill": "s", "agents": ["d", "d"]}`, "named twice"},
		"a bad agent":     {base + `, "gather": {"skill": "s", "agents": ["../d"]}`, "no agent's id"},
		"an agent number": {base + `, "gather": {"skill": "s", "agents": [3]}`, "is not text"},
		"a long note":     {base + `, "gather": {"skill": "s", "note": "` + strings.Repeat("n", MaxGatherNote+1) + `"}`, "more than"},
		"a gather list":   {base + `, "gather": ["s"]`, "no object"},
	} {
		got, err := ParseSchedules([]byte(`{"schedules": [{` + tc.entry + `}]}`))
		if err != nil || !strings.Contains(got.Entries[0].Problem, tc.want) {
			t.Errorf("%s: %+v %v, want %q", name, got.Entries, err, tc.want)
		}
	}
}

// A skill is <dir>/<name>/SKILL.md, followed through a link, a regular
// file of UTF-8 within its bound; one that is not there says so (ADR-0033).
func TestLoadSkill(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "code-grooming"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code-grooming", SkillFile), []byte("---\nname: code-grooming\n---\nFind dead code.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSkill(dir, "code-grooming")
	if err != nil || got.Name != "code-grooming" || !strings.Contains(got.Text, "Find dead code.") {
		t.Fatalf("%+v %v", got, err)
	}
	// A skill the owner's configuration links in is read through its link.
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "triage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "triage", SkillFile), []byte("Triage the issues."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "triage"), filepath.Join(dir, "triage")); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadSkill(dir, "triage"); err != nil || got.Text != "Triage the issues." {
		t.Fatalf("a linked skill: %+v %v", got, err)
	}
	if _, err := LoadSkill(dir, "missing"); !errors.Is(err, ErrNoSkill) {
		t.Fatalf("a missing skill: %v", err)
	}
	if _, err := LoadSkill("", "code-grooming"); !errors.Is(err, ErrNoSkill) {
		t.Fatalf("no skills folder: %v", err)
	}
	if _, err := LoadSkill(dir, "../code-grooming"); err == nil {
		t.Fatal("a path is a skill's name")
	}
	for name, data := range map[string][]byte{"big": []byte(strings.Repeat("x", MaxSkill+1)), "binary": {0xff, 0xfe}, "empty": {}} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, SkillFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSkill(dir, name); err == nil || errors.Is(err, ErrNoSkill) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "folder", SkillFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSkill(dir, "folder"); err == nil {
		t.Error("a folder is a skill's text")
	}
}

// A named pipe called SKILL.md is refused at once: opening it never waits
// for a writer, which would hold the schedules pass (Codex review of #57,
// round 1).
func TestASkillThatIsANamedPipeIsRefusedWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pipe"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(dir, "pipe", SkillFile)); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadSkill(dir, "pipe"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("a named pipe as a skill: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("LoadSkill waited on a named pipe")
	}
}
