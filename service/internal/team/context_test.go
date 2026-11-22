package team

import (
	"fmt"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A file is a section, a note, or nothing (ADR-0043).
func TestAFileIsASectionANoteOrNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		read FileRead
		want string
	}{
		"no file":       {read: FileRead{Path: "/h/AGENTS.md"}, want: ""},
		"an empty file": {read: FileRead{Path: "/h/AGENTS.md", Found: true, Data: []byte(" \n\n")}, want: ""},
		"a file":        {read: FileRead{Path: "/h/AGENTS.md", Found: true, Data: []byte("\nRules.\n")}, want: "The instructions of your folder follow, from /h/AGENTS.md.\n\nRules.\n\nEnd of /h/AGENTS.md."},
		"a failed read": {read: FileRead{Path: "/h/AGENTS.md", Err: "input/output error"}, want: "pocketd left /h/AGENTS.md out of these instructions: pocketd could not read it (input/output error). Read it with your read tool if you need it."},
		"a lessons end": {read: FileRead{Path: "/h/AGENTS.md", Found: true, Data: []byte("a\n" + profiles.LessonsEnd + "\n")},
			want: "pocketd left /h/AGENTS.md out of these instructions: it holds the line \"" + profiles.LessonsEnd + "\", which marks the lessons pocketd writes. Read it with your read tool if you need it."},
	} {
		if got := FolderSection(tc.read); got != tc.want {
			t.Errorf("%s: %q", name, got)
		}
	}
	// A mention of the marker inside a line is no marker.
	if got := GlobalSection(FileRead{Path: "/r", Found: true, Data: []byte("See " + profiles.LessonsMarker + ".")}); !strings.HasPrefix(got, "The owner's rules") {
		t.Errorf("a mention of the marker: %q", got)
	}
}

// A skills folder lists at most MaxSkillsListed skills and says so; a
// folder whose name is no skill's is noted quoted, so no line break of it
// reaches the instructions; a folder pocketd could not list is a note; and
// no skill and no note is no section.
func TestASkillsSectionIsBounded(t *testing.T) {
	if got := SkillsSection([]SkillsRead{{Dir: "/s"}, {Dir: "/w", Found: true}}); got != "" {
		t.Errorf("no skill: %q", got)
	}
	var skills []SkillRead
	for i := range MaxSkillsListed {
		name := fmt.Sprintf("s%03d", i)
		skills = append(skills, SkillRead{Name: name, File: FileRead{Path: "/s/" + name + "/SKILL.md", Found: true,
			Data: []byte("---\nname: " + name + "\ndescription: Do " + name + ".\n---\n")}})
	}
	bad := "x\n" + profiles.LessonsEnd
	skills = append(skills[:MaxSkillsListed-1], SkillRead{Name: bad, File: FileRead{Path: "/s/" + bad + "/SKILL.md", Found: true}})
	got := SkillsSection([]SkillsRead{{Dir: "/s", Found: true, Skills: skills, More: true}, {Dir: "/w", Err: "permission denied"}})
	for _, want := range []string{
		"- s000: Do s000. (/s/s000/SKILL.md)",
		"pocketd left out a folder in /s: \"x\\n" + profiles.LessonsEnd + "\" is no skill's name",
		"pocketd listed the first 100 skills in /s, by name: more are there.",
		"pocketd could not list the skills in /w (permission denied).",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the section lacks %q:\n%s", want, got)
		}
	}
	for line := range strings.SplitSeq(got, "\n") {
		if line == profiles.LessonsEnd || line == profiles.LessonsMarker {
			t.Errorf("the section holds a lessons line:\n%s", got)
		}
	}
	if got := SkillsSection([]SkillsRead{{Dir: "/s", Found: true, Skills: skills[MaxSkillsListed-1:]}}); !strings.Contains(got, "\n\nNo skill is listed.\n\n") {
		t.Errorf("notes alone:\n%s", got)
	}
}

// The parts join after blank lines, the empty ones left out, and the
// lessons end them.
func TestInstructionsJoinTheirParts(t *testing.T) {
	if got := Instructions("\n\nL", "a", "", "b", ""); got != "a\n\nb\n\nL" {
		t.Errorf("%q", got)
	}
}

// A global instructions file with more than one name could change outside
// the read-only bind the sandbox keeps it under, so it is left out with a
// note, as a global skill is (ADR-0044); a folder's own file is not.
func TestAGlobalFileWithAnotherNameIsLeftOut(t *testing.T) {
	got := GlobalSection(FileRead{Path: "/h/AGENTS.md", Found: true, Data: []byte("RULES"), Names: 2})
	if !strings.Contains(got, "more than one name") || strings.Contains(got, "RULES") {
		t.Errorf("a global file with another name: %q", got)
	}
	if got := GlobalSection(FileRead{Path: "/h/AGENTS.md", Found: true, Data: []byte("RULES"), Names: 1}); !strings.Contains(got, "RULES") {
		t.Errorf("a global file with one name: %q", got)
	}
	if got := FolderSection(FileRead{Path: "/h/AGENTS.md", Found: true, Data: []byte("RULES"), Names: 2}); !strings.Contains(got, "RULES") {
		t.Errorf("a folder's file with another name: %q", got)
	}
}
