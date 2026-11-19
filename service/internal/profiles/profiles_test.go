package profiles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

const home = "/home/team"

const specialist = `---
model: openai/gpt-6-astra
role: agent
repositories:
  frostyard/example: ~/src/example
  frostyard/other: /srv/other
home: ~/agents/vera
mcp: [notes, notes-lodge]
tools: [read, bash, read]
---
# Vera

Vera keeps the example repositories tidy.
`

func TestParseReadsEveryField(t *testing.T) {
	p, err := Parse("vera", []byte(specialist), home)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "vera" || p.Name != "Vera" || p.Role != RoleAgent || p.Private {
		t.Errorf("identity %+v", p)
	}
	if p.Model != (proto.ModelRef{Provider: "openai", ID: "gpt-6-astra"}) {
		t.Errorf("model %+v", p.Model)
	}
	if p.Home != "/home/team/agents/vera" || p.Workspace != "" {
		t.Errorf("folders %q %q", p.Home, p.Workspace)
	}
	if p.Repositories["frostyard/example"] != "/home/team/src/example" || p.Repositories["frostyard/other"] != "/srv/other" {
		t.Errorf("repositories %v", p.Repositories)
	}
	if !slices.Equal(p.MCP, []string{"notes", "notes-lodge"}) || !slices.Equal(p.Tools, []string{"read", "bash"}) {
		t.Errorf("mcp %v tools %v", p.MCP, p.Tools)
	}
	if !strings.HasPrefix(p.Identity, "# Vera\n\nVera keeps") {
		t.Errorf("identity %q", p.Identity)
	}
	if want := []string{"/home/team/agents/vera", "/home/team/src/example", "/srv/other"}; !slices.Equal(p.Folders(), want) {
		t.Errorf("folders %q, want %q", p.Folders(), want)
	}
}

func TestParseDefaultsAndForms(t *testing.T) {
	// A bare ~ is YAML's null, and still the home; a model list's first
	// entry is the model; no role is an agent; no heading names it by id.
	p, err := Parse("lone", []byte("---\nmodel: [anthropic/claude-opus-5-5, openai/gpt-6-astra]\nhome: ~\nprivate: true\n---\nNo heading.\n"), home)
	if err != nil {
		t.Fatal(err)
	}
	if p.Home != home || p.Model.ID != "claude-opus-5-5" || p.Role != RoleAgent || p.Name != "lone" || !p.Private || p.Tools != nil {
		t.Errorf("%+v", p)
	}
	if p, err := Parse("crlf", []byte("---\r\nrole: supervisor\r\nmodel: anthropic/x\r\n---\r\n# Boss\r\n"), home); err != nil || p.Role != RoleSupervisor || p.Name != "Boss" {
		t.Errorf("CRLF profile %+v, %v", p, err)
	}
}

func TestParseRefusesWhatBreaksARule(t *testing.T) {
	for name, tc := range map[string]struct{ text, want string }{
		"no frontmatter":     {"# Vera\n", "frontmatter"},
		"unterminated":       {"---\nmodel: a/b\n", "does not end"},
		"no model":           {"---\nrole: agent\n---\n", "model"},
		"bad model":          {"---\nmodel: gpt-6\n---\n", "provider/id"},
		"bad role":           {"---\nmodel: a/b\nrole: boss\n---\n", "supervisor, clerk or agent"},
		"relative home":      {"---\nmodel: a/b\nhome: agents/vera\n---\n", "absolute"},
		"unknown key":        {"---\nmodel: a/b\nshoe: 9\n---\n", "not a profile field"},
		"private as text":    {"---\nmodel: a/b\nprivate: \"yes\"\n---\n", "true or false"},
		"private no home":    {"---\nmodel: a/b\nprivate: true\n---\n", "has none"},
		"tools not a list":   {"---\nmodel: a/b\ntools: bash\n---\n", "not a list"},
		"bad tool name":      {"---\nmodel: a/b\ntools: [Bash!]\n---\n", "tool name"},
		"bad repository":     {"---\nmodel: a/b\nrepositories:\n  example: ~/src\n---\n", "owner/name"},
		"repositories list":  {"---\nmodel: a/b\nrepositories: [a/b]\n---\n", "map"},
		"mcp as text":        {"---\nmodel: a/b\nmcp: notes\n---\n", "not a list"},
		"bad mcp name":       {"---\nmodel: a/b\nmcp: [\"no spaces\"]\n---\n", "MCP server name"},
		"empty model list":   {"---\nmodel: []\n---\n", "empty"},
		"model with a space": {"---\nmodel: a/b c\n---\n", "provider/id"},
	} {
		if _, err := Parse("vera", []byte(tc.text), home); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
	if _, err := Parse("Vera", []byte(specialist), home); err == nil {
		t.Error("an id with a capital letter was accepted")
	}
}

// Only <id>.md files are profiles: backups and other files are not, and an
// invalid profile is a problem, not an error of the whole folder.
func TestLoadReadsOnlyProfiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("vera.md", specialist)
	write("vera.md.bak-20261118-200400", "---\nmodel: broken\n---\n")
	write("vera.md.bak", "not a profile")
	write("notes.txt", "not a profile")
	write("broken.md", "---\nmodel: broken\n---\n")
	write(".hidden.md", specialist)
	if err := os.Mkdir(filepath.Join(dir, "sub.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	profiles, problems, err := Load(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].ID != "vera" {
		t.Errorf("profiles %+v", profiles)
	}
	if len(problems) != 1 || problems[0].ID != "broken" || !strings.Contains(problems[0].Err, "provider/id") {
		t.Errorf("problems %+v", problems)
	}
	if p, err := Find(dir, home, "vera"); err != nil || p.Name != "Vera" {
		t.Errorf("Find(vera) = %+v, %v", p, err)
	}
	for _, id := range []string{"missing", "../vera", "vera.md.bak"} {
		if _, err := Find(dir, home, id); err == nil {
			t.Errorf("Find(%q) found a profile", id)
		}
	}
}
