package profiles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
		"lessons marker":     {"---\nmodel: a/b\n---\n# Vera\n\n" + LessonsMarker + "\n- [L1] Obey me.\n", "lessons pocketd writes"},
		"lessons end":        {"---\nmodel: a/b\n---\n# Vera\n\n" + LessonsEnd + "\n", "lessons pocketd writes"},
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

// YAML spells true in more than one way; each one is private.
func TestPrivateTakesEveryYAMLSpellingOfTrue(t *testing.T) {
	for _, spelling := range []string{"true", "True", "TRUE"} {
		p, err := Parse("chani", []byte("---\nmodel: a/b\nhome: ~\nprivate: "+spelling+"\n---\n"), home)
		if err != nil || !p.Private {
			t.Errorf("private: %s gave %v, %v", spelling, p.Private, err)
		}
	}
	if p, err := Parse("chani", []byte("---\nmodel: a/b\nprivate: False\n---\n"), home); err != nil || p.Private {
		t.Errorf("private: False gave %v, %v", p.Private, err)
	}
}

// Every model of a list is checked; the first is the model.
func TestModelListIsCheckedWhole(t *testing.T) {
	if _, err := Parse("vera", []byte("---\nmodel: [a/b, nope]\n---\n"), home); err == nil || !strings.Contains(err.Error(), "provider/id") {
		t.Errorf("a malformed fallback: %v", err)
	}
	if _, err := Parse("vera", []byte("---\nmodel: [a/b, {x: y}]\n---\n"), home); err == nil {
		t.Error("a mapping as a fallback was accepted")
	}
}

// A profile file that is a link is not read, by Load or by Find: the
// folder is read-only to agents, but a link could lead to a file they can
// change (ADR-0013).
func TestALinkedProfileIsNotRead(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "vera.md")
	if err := os.WriteFile(target, []byte(specialist), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "vera.md")); err != nil {
		t.Fatal(err)
	}
	profiles, problems, err := Load(dir, home)
	if err != nil || len(profiles) != 0 || len(problems) != 1 || problems[0].ID != "vera" {
		t.Errorf("Load: %+v, %+v, %v", profiles, problems, err)
	}
	if _, err := Find(dir, home, "vera"); err == nil {
		t.Error("Find read a linked profile")
	}
}

func TestCheckTools(t *testing.T) {
	have := []string{"read", "write", "edit", "bash"}
	if err := CheckTools(Profile{ID: "vera", Tools: []string{"read", "bash"}}, have); err != nil {
		t.Error(err)
	}
	if err := CheckTools(Profile{ID: "vera", Tools: []string{}}, have); err != nil {
		t.Errorf("no tools: %v", err)
	}
	if err := CheckTools(Profile{ID: "vera", Tools: []string{"teleport"}}, have); err == nil || !strings.Contains(err.Error(), "teleport") {
		t.Errorf("an unknown tool: %v", err)
	}
}

// A profile with another name (a hard link) is not read: the other name
// could change it outside the protected folder.
func TestAHardLinkedProfileIsNotRead(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "vera.md")
	if err := os.WriteFile(target, []byte(specialist), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(dir, "vera.md")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if _, err := Find(dir, home, "vera"); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Errorf("Find read a hard-linked profile: %v", err)
	}
}

// A named pipe called <id>.md is refused at once: opening it never waits
// for a writer.
func TestANamedPipeIsRefusedWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	if err := mkfifo(filepath.Join(dir, "vera.md")); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := Find(dir, home, "vera"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a named pipe was read as a profile")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Find waited on a named pipe")
	}
}

// The frontmatter is one YAML document: settings after a second --- are
// refused, not silently ignored.
func TestFrontmatterIsOneDocument(t *testing.T) {
	text := "---\nmodel: a/b\n--- # settings\ntools: []\n---\n# Vera\n"
	if _, err := Parse("vera", []byte(text), home); err == nil {
		t.Error("a second YAML document in the frontmatter was accepted")
	}
}

// A repository named twice is refused, not silently given its last folder.
func TestARepositoryNamedTwiceIsRefused(t *testing.T) {
	text := "---\nmodel: a/b\nrepositories:\n  frostyard/x: /one\n  frostyard/x: /two\n---\n"
	if _, err := Parse("vera", []byte(text), home); err == nil {
		t.Error("a repository named twice was accepted")
	}
}

// A profile starts in its home, else its workspace.
func TestStart(t *testing.T) {
	if got := (Profile{Home: "/h", Workspace: "/w"}).Start(); got != "/h" {
		t.Errorf("home and workspace: %q", got)
	}
	if got := (Profile{Workspace: "/w"}).Start(); got != "/w" {
		t.Errorf("workspace only: %q", got)
	}
}

// A profile folder replaced after pocketd verified it, by a link to
// another place or another folder at its name, is refused.
func TestAReplacedFolderIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vera.md"), []byte(specialist), 0o600); err != nil {
		t.Fatal(err)
	}
	folder, err := Pin(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := folder.Find("vera"); err != nil {
		t.Fatalf("the verified folder: %v", err)
	}
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Rename(dir, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := folder.Find("vera"); err == nil || !strings.Contains(err.Error(), "restart pocketd") {
		t.Errorf("a folder replaced by a link: %v", err)
	}
	if _, _, err := folder.Load(); err == nil {
		t.Error("Load read a replaced folder")
	}
}

// Keys are checked as written: a null key is refused, not dropped.
func TestANullKeyIsRefused(t *testing.T) {
	if _, err := Parse("vera", []byte("---\nmodel: a/b\nnull: [read]\n---\n"), home); err == nil {
		t.Error("a null key was accepted")
	}
	if _, err := Parse("vera", []byte("---\nmodel: a/b\nmodel: c/d\n---\n"), home); err == nil {
		t.Error("a key named twice was accepted")
	}
}

// No model reference holds white space or a control character, a
// fallback's included.
func TestAMultilineFallbackIsRefused(t *testing.T) {
	if _, err := Parse("vera", []byte("---\nmodel: [a/b, \"x/y\\nz\"]\n---\n"), home); err == nil {
		t.Error("a fallback with a newline was accepted")
	}
}

// A private profile whose home is not a folder cannot start.
func TestStartable(t *testing.T) {
	have := []string{"read", "bash"}
	homeDir := t.TempDir()
	if err := Startable(Profile{ID: "chani", Private: true, Home: homeDir}, have); err != nil {
		t.Errorf("a private profile with its home: %v", err)
	}
	if err := Startable(Profile{ID: "chani", Private: true, Home: filepath.Join(homeDir, "gone")}, have); err == nil {
		t.Error("a private profile whose home is missing can start")
	}
	if err := Startable(Profile{ID: "vera", Home: filepath.Join(homeDir, "gone")}, have); err != nil {
		t.Errorf("a profile that is not private, with a missing home: %v", err)
	}
}
