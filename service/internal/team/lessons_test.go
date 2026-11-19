package team

import (
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A lesson is one line with nothing hidden: the owner approves every
// character the agent reads.
func TestLessonTextIsOneVisibleLine(t *testing.T) {
	for in, want := range map[string]string{
		"  Run the tests\n\tbefore a PR.  ": "Run the tests before a PR.",
		"Use\u200b red\u2028text":           "Use red text",
		"A\u202eB\ufe0fC\u2800D\U000E0100E": "ABCDE",
		"x\u0000y\u001bz":                   "xyz",
		"keep é, 日本 and ✓":                  "keep é, 日本 and ✓",
		"a \u200b b":                        "a b",
	} {
		got, err := LessonText(in)
		if err != nil || got != want {
			t.Errorf("LessonText(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// U+0378 is unassigned: it shows nothing, and goes.
	if got, err := LessonText("a\u0378b"); err != nil || got != "ab" {
		t.Errorf("an unassigned code point: %q, %v", got, err)
	}
	for _, in := range []string{"", "  \n\t", "\u200b\u200c", "\u0378"} {
		if _, err := LessonText(in); err == nil {
			t.Errorf("LessonText(%q) took a lesson with no text", in)
		}
	}
}

// A lesson is MaxLesson characters at most, counted once flattened, and
// MaxLesson is the protocol's.
func TestALessonIsAtMostMaxLessonCharacters(t *testing.T) {
	if MaxLesson != proto.MaxLesson {
		t.Fatalf("MaxLesson %d, the protocol's %d", MaxLesson, proto.MaxLesson)
	}
	if _, err := LessonText(strings.Repeat("é", MaxLesson)); err != nil {
		t.Errorf("%d characters: %v", MaxLesson, err)
	}
	if _, err := LessonText(strings.Repeat("é", MaxLesson+1)); err == nil || !strings.Contains(err.Error(), "301") {
		t.Errorf("%d characters: %v", MaxLesson+1, err)
	}
	// White space collapsed first: the flat text is what counts.
	if _, err := LessonText(strings.Repeat("a  ", 150)); err != nil {
		t.Errorf("300 characters once flattened: %v", err)
	}
}

func TestTheLessonLimits(t *testing.T) {
	if err := CheckPending(MaxPending - 1); err != nil {
		t.Error(err)
	}
	if err := CheckPending(MaxPending); err == nil {
		t.Error("a writer's 21st waiting lesson was taken")
	}
	if err := CheckRoom(MaxLessons - 1); err != nil {
		t.Error(err)
	}
	if err := CheckRoom(MaxLessons); err == nil {
		t.Error("an agent's 41st lesson in force was taken")
	}
}

func TestCheckMemoryKnowsEachAction(t *testing.T) {
	for _, m := range []Memory{
		{Action: MemoryAdd, Text: "x"},
		{Action: MemoryAdd, Text: "x", Agent: "vera"},
		{Action: MemoryUpdate, ID: "L1", Text: "x"},
		{Action: MemoryRemove, ID: "L1"},
		{Action: MemoryList},
		{Action: MemoryList, Agent: "vera"},
	} {
		if err := CheckMemory(m); err != nil {
			t.Errorf("%+v: %v", m, err)
		}
	}
	for _, m := range []Memory{
		{Action: "forget", ID: "L1"},
		{Action: MemoryAdd},
		{Action: MemoryAdd, Text: "  "},
		{Action: MemoryAdd, Text: "x", ID: "L1"},
		{Action: MemoryUpdate, Text: "x"},
		{Action: MemoryUpdate, ID: "L1"},
		{Action: MemoryRemove},
		{Action: MemoryRemove, ID: "L1", Text: "x"},
		{Action: MemoryList, ID: "L1"},
		{Action: MemoryUpdate, ID: "L1", Text: "x", Agent: "vera"},
		{Action: MemoryRemove, ID: "L1", Agent: "vera"},
	} {
		if err := CheckMemory(m); err == nil {
			t.Errorf("%+v was taken", m)
		}
	}
}

func TestLessonIDReadsEachForm(t *testing.T) {
	for in, want := range map[string]int64{"L3": 3, "[L3]": 3, " 3 ": 3, "l12": 12} {
		if got, err := LessonID(in); err != nil || got != want {
			t.Errorf("LessonID(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "L", "L0", "L-1", "A3", "L3x"} {
		if _, err := LessonID(in); err == nil {
			t.Errorf("LessonID(%q) named a lesson", in)
		}
	}
}

// The section starts with the marker pocketd owns, lists the lessons in
// force as [L<id>], and tells a supervisor it may keep another's.
func TestTheLessonsSection(t *testing.T) {
	text := LessonsSection("Vera", profiles.RoleAgent, []Lesson{{ID: 2, Text: "Run the tests."}, {ID: 5, Text: "Ask\nbefore a rewrite."}})
	if !strings.HasPrefix(text, "\n\n"+profiles.LessonsMarker+"\n") {
		t.Fatalf("no marker first:\n%s", text)
	}
	for _, want := range []string{"- [L2] Run the tests.\n", "- [L5] Ask before a rewrite.\n", "team_memory", "Inbox"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "As supervisor") {
		t.Error("an agent was told it keeps others' lessons")
	}
	if none := LessonsSection("Odrade", profiles.RoleSupervisor, nil); !strings.Contains(none, "none yet") || !strings.Contains(none, "As supervisor") {
		t.Errorf("a supervisor's section:\n%s", none)
	}
	first := LessonsSection("Vera", profiles.RoleAgent, []Lesson{{ID: 2, Text: "a"}})
	if again := LessonsSection("Vera", profiles.RoleAgent, []Lesson{{ID: 2, Text: "a"}}); again != first {
		t.Error("the same lessons gave two texts")
	}
}

func TestListTextSaysWhatWaits(t *testing.T) {
	text := ListText("Vera", []Lesson{
		{ID: 1, Text: "a"},
		{ID: 2, Text: "b", Pending: true},
		{ID: 3, Text: "c", Forget: true},
		{ID: 4, Text: "d", ChangeText: "e"},
	})
	for _, want := range []string{"[L1] a\n", "[L2] b (waiting", "[L3] c (forgetting", "[L4] d (new text waits for the owner's approval: e)"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	if got := ListText("Vera", nil); got != "No lessons for Vera yet." {
		t.Errorf("empty list: %q", got)
	}
}

// Only a section pocketd wrote is taken off instructions: one that ends
// them, from the last start line before its end line. Text that only
// holds the start line, as an older session's identity may, stays (Codex
// review of #18, round 2).
func TestWithoutLessonsTakesOnlyTheSectionPocketdWrote(t *testing.T) {
	section := LessonsSection("Vera", profiles.RoleAgent, []Lesson{{ID: 1, Text: "a"}})
	if !strings.HasSuffix(section, "\n"+profiles.LessonsEnd+"\n") || !strings.Contains(section, "If you have the team_memory tool") {
		t.Fatalf("section:\n%s", section)
	}
	base := "You are Vera.\n\n" + profiles.LessonsMarker + " and more, from an old identity.\n\nKeep it."
	if got, had := WithoutLessons(base); had || got != base {
		t.Fatalf("instructions with only a look-alike heading lost %q", strings.TrimPrefix(base, got))
	}
	if got, had := WithoutLessons(base + section); !had || got != base {
		t.Fatalf("the section was not taken off: %q, %v", got, had)
	}
	// A heading of the old identity that is the very start line stays too:
	// the section starts at the last one.
	exact := "You are Vera.\n\n" + profiles.LessonsMarker + "\nKeep this."
	if got, had := WithoutLessons(exact + section); !had || got != exact {
		t.Fatalf("an old heading equal to the start line: %q, %v", got, had)
	}
}
