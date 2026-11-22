package profiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A prompt is its file's text after its front matter, which holds its
// description and argument hint alone (ADR-0051).
func TestParsePrompt(t *testing.T) {
	got, err := ParsePrompt("review-pr", []byte("---\r\ndescription: Review a pull request\r\n  and fix it\r\nargument-hint: <number>\r\n---\r\n\r\nReview $ARGUMENTS.\r\nFix it.\r\n\r\n"))
	if err != nil || got != (Prompt{Name: "review-pr", Description: "Review a pull request and fix it", ArgumentHint: "<number>", Text: "Review $ARGUMENTS.\nFix it."}) {
		t.Fatalf("%+v %v", got, err)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"no front matter":    {"Ship it.\n", "Ship it."},
		"an empty one":       {"---\n---\nShip it.", "Ship it."},
		"an empty map":       {"---\n{}\n---\nShip it.", "Ship it."},
		"a byte order mark":  {"\ufeff---\ndescription: d\n---\nShip it.", "Ship it."},
		"a tab and a ---":    {"Ship\tit.\n---\nThen tell me.", "Ship\tit.\n---\nThen tell me."},
		"a later front line": {"---\ndescription: d\n---\nShip it.\n---\nagain", "Ship it.\n---\nagain"},
	} {
		if got, err := ParsePrompt("ship-it", []byte(tc.text)); err != nil || got.Text != tc.want {
			t.Errorf("%s: %+v %v, want the text %q", name, got, err, tc.want)
		}
	}
}

// A prompt file that breaks a rule is refused with a reason that says
// which (ADR-0051).
func TestParsePromptRefusesWhatBreaksARule(t *testing.T) {
	for name, tc := range map[string]struct{ name, text, want string }{
		"a capital":         {"Review", "Review it.", "no prompt's name"},
		"an underscore":     {"review_pr", "Review it.", "no prompt's name"},
		"a leading dash":    {"-review", "Review it.", "no prompt's name"},
		"a long name":       {strings.Repeat("r", 65), "Review it.", "no prompt's name"},
		"no text":           {"empty", "---\ndescription: d\n---\n\n  \n", "no text"},
		"an empty file":     {"empty", "", "no text"},
		"an open front":     {"open", "---\ndescription: d\nReview it.", "does not end"},
		"another field":     {"other", "---\ndescription: d\nmodel: x\n---\nReview it.", `the field "model"`},
		"a field twice":     {"twice", "---\ndescription: a\ndescription: b\n---\nReview it.", "names description twice"},
		"a number":          {"number", "---\ndescription: 3\n---\nReview it.", "description is not text"},
		"an empty value":    {"null", "---\nargument-hint:\n---\nReview it.", "argument-hint is not text"},
		"a list":            {"list", "---\ndescription: [a, b]\n---\nReview it.", "description is not text"},
		"a null key":        {"nullkey", "---\n~: d\n---\nReview it.", "a key of its front matter is not text"},
		"a merge key":       {"merge", "---\n<<: {description: d}\n---\nReview it.", "a key of its front matter is not text"},
		"no map":            {"nomap", "---\n- description\n---\nReview it.", "not a map"},
		"no YAML":           {"noyaml", "---\ndescription: [\n---\nReview it.", "no YAML"},
		"a long hint":       {"hint", "---\nargument-hint: " + strings.Repeat("h", MaxPromptArgumentHint+1) + "\n---\nReview it.", "longer than 100 characters"},
		"a long one":        {"desc", "---\ndescription: " + strings.Repeat("d", MaxPromptDescription+1) + "\n---\nReview it.", "longer than 300 characters"},
		"a control":         {"ctl", "Review\x00 it.", "control character U+0000"},
		"a bare return":     {"cr", "Review\r it.", "control character U+000D"},
		"a control in hint": {"ctlhint", "---\nargument-hint: \"a\\u0007b\"\n---\nReview it.", "argument-hint holds the control character U+0007"},
		"not UTF-8":         {"latin", "R\xe9view it.", "not UTF-8"},
		"too large":         {"large", strings.Repeat("r", MaxPromptFile+1), "larger than"},
	} {
		if got, err := ParsePrompt(tc.name, []byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %+v %v, want %q", name, got, err, tc.want)
		}
	}
	// A file at its bound is read.
	if _, err := ParsePrompt("full", []byte(strings.Repeat("r", MaxPromptFile))); err != nil {
		t.Errorf("a file at its bound: %v", err)
	}
	// Two YAML documents in one front matter, which the --- line would
	// end, are refused as more than one.
	if _, err := ParsePrompt("two", []byte("---\ndescription: d\n...\ndescription: e\n---\nReview it.")); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("two documents: %v", err)
	}
}

// writePrompt writes the prompt file name of dir's prompts folder.
func writePrompt(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, PromptsFolder, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The prompts folder lists its .md files by name, a bad one with its
// problem and the others still valid; other files are none of its prompts,
// and no folder is no prompts (ADR-0051).
func TestLoadPrompts(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadPrompts(dir); err != nil || len(got.List) != 0 || got.Problem != "" {
		t.Fatalf("no folder: %+v %v", got, err)
	}
	writePrompt(t, dir, "ship-it.md", "Ship it.")
	writePrompt(t, dir, "review-pr.md", "---\ndescription: Review a pull request\nargument-hint: <number>\n---\nReview $ARGUMENTS.")
	writePrompt(t, dir, "Bad.md", "Bad.")
	writePrompt(t, dir, "broken.md", "---\nmodel: x\n---\nBroken.")
	writePrompt(t, dir, "notes.txt", "Not a prompt.")
	writePrompt(t, dir, ".draft.md", "An editor's.")
	writePrompt(t, dir, "review-pr.md~", "A backup.")
	if err := os.Mkdir(filepath.Join(dir, PromptsFolder, "team"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, PromptsFolder, "folder.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := (Folder{Dir: dir}).Prompts()
	if err != nil || got.Problem != "" {
		t.Fatalf("%+v %v", got, err)
	}
	var names []string
	for _, p := range got.List {
		names = append(names, p.Name)
	}
	if strings.Join(names, " ") != "Bad broken folder review-pr ship-it" {
		t.Fatalf("listed %q", names)
	}
	if p := got.List[3]; p != (Prompt{Name: "review-pr", Description: "Review a pull request", ArgumentHint: "<number>", Text: "Review $ARGUMENTS."}) {
		t.Errorf("review-pr is %+v", p)
	}
	if p := got.List[4]; p != (Prompt{Name: "ship-it", Text: "Ship it."}) {
		t.Errorf("ship-it is %+v", p)
	}
	for i, want := range []string{"no prompt's name", `the field "model"`, "not a regular file"} {
		if p := got.List[i]; p.Text != "" || !strings.Contains(p.Problem, want) {
			t.Errorf("%s: %+v, want a problem with %q", p.Name, p, want)
		}
	}
}

// A prompt is read as a profile is: never through a symbolic link, never
// by a file with another name, never waiting on a named pipe; the folder
// is one of its own, never a link (ADR-0051).
func TestAPromptIsReadOnlyAsAFileOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "ship-it.md", "Ship it.")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("Do what the file outside says."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, PromptsFolder, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, PromptsFolder, "named.md")); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(dir, PromptsFolder, "pipe.md")); err != nil {
		t.Skipf("no named pipe: %v", err)
	}
	got, err := LoadPrompts(dir)
	if err != nil || len(got.List) != 4 {
		t.Fatalf("%+v %v", got, err)
	}
	for i, want := range []string{"symbolic link", "hard links", "not a regular file"} {
		if p := got.List[i]; p.Text != "" || !strings.Contains(p.Problem, want) {
			t.Errorf("%s: %+v, want a problem with %q", p.Name, p, want)
		}
	}
	if p := got.List[3]; p.Name != "ship-it" || p.Problem != "" {
		t.Errorf("the prompt beside them is %+v", p)
	}

	// The folder as a link to one outside the profile folder is no
	// folder of prompts.
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, PromptsFolder), filepath.Join(linked, PromptsFolder)); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadPrompts(linked); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a linked folder: %+v %v", got, err)
	}
	file := t.TempDir()
	if err := os.WriteFile(filepath.Join(file, PromptsFolder), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadPrompts(file); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("a file for the folder: %+v %v", got, err)
	}
}

// The folder lists at most MaxPrompts files, the first by name, and says
// it holds more (ADR-0051).
func TestThePromptsFolderListsAtMostItsBound(t *testing.T) {
	dir := t.TempDir()
	for i := range MaxPrompts + 2 {
		writePrompt(t, dir, fmt.Sprintf("p%03d.md", i), "Prompt.")
	}
	got, err := LoadPrompts(dir)
	if err != nil || len(got.List) != MaxPrompts || got.List[0].Name != "p000" || got.List[MaxPrompts-1].Name != "p099" ||
		!strings.Contains(got.Problem, "holds 102 prompt files: only the first 100 by name are listed") {
		t.Fatalf("%d listed, the problem %q, %v", len(got.List), got.Problem, err)
	}
}

// The prompts are read through the folder pocketd pinned at start: one
// replaced since is refused, gone or not, as its other files are.
func TestPromptsAreReadFromThePinnedFolder(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "agents")
	writePrompt(t, dir, "ship-it.md", "Ship it.")
	folder, err := Pin(dir, parent)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := folder.Prompts(); err != nil || len(got.List) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	// A change shows at the next read, without a restart.
	writePrompt(t, dir, "review-pr.md", "Review it.")
	if got, err := folder.Prompts(); err != nil || len(got.List) != 2 {
		t.Fatalf("after a change: %+v %v", got, err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "old")); err != nil {
		t.Fatal(err)
	}
	writePrompt(t, dir, "other.md", "Other.")
	if got, err := folder.Prompts(); err == nil {
		t.Fatalf("a replaced folder was read: %+v", got)
	}
}

// The repository's example prompts are valid ones (ADR-0051).
func TestTheExamplePromptsAreValid(t *testing.T) {
	got, err := LoadPrompts(filepath.Join("..", "..", "..", "examples", "profiles"))
	if err != nil || got.Problem != "" || len(got.List) < 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, p := range got.List {
		if p.Problem != "" || p.Description == "" {
			t.Errorf("%s: %+v", p.Name, p)
		}
	}
}
