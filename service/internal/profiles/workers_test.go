package profiles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The owner's worker list is checked in code: an ordered list of 1 to 8
// distinct models and nothing else (ADR-0022).
func TestParseWorkers(t *testing.T) {
	got, err := ParseWorkers([]byte(`{"models": ["halogen/qwen", "deepseek/deepseek-flash"]}`))
	want := []proto.ModelRef{{Provider: "halogen", ID: "qwen"}, {Provider: "deepseek", ID: "deepseek-flash"}}
	if err != nil || !slices.Equal(got.Models, want) || got.Default {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.String() != "halogen/qwen, deepseek/deepseek-flash" {
		t.Errorf("String() = %q", got.String())
	}
	nine := `{"models": ["a/1", "a/2", "a/3", "a/4", "a/5", "a/6", "a/7", "a/8", "a/9"]}`
	for name, tc := range map[string]struct{ text, want string }{
		"no models":     {`{}`, "names no models"},
		"an empty list": {`{"models": []}`, "names 0 models"},
		"too many":      {nine, "names 9 models"},
		"a bad entry":   {`{"models": ["flash"]}`, "not provider/id"},
		"a space":       {`{"models": ["deepseek/deepseek flash"]}`, "not provider/id"},
		"named twice":   {`{"models": ["a/1", "a/1"]}`, "named twice"},
		"another field": {`{"models": ["a/1"], "default": "a/1"}`, `the field "default"`},
		"not an object": {`["a/1"]`, "no object"},
		"a number":      {`{"models": [1]}`, "not {"},
		"trailing text": {`{"models": ["a/1"]} {}`, "text follows"},
		"null":          {`{"models": null}`, "names no models"},
		"unended":       {`{"models": ["a/1"]`, "not {"},
		// A decoder would take these: a name in another case, and the
		// last of a key named twice (Codex review of #33).
		"another case": {`{"models": ["flash"], "Models": ["a/1"]}`, `the field "Models"`},
		"in capitals":  {`{"MODELS": ["a/1"]}`, `the field "MODELS"`},
		"twice":        {`{"models": ["flash"], "models": ["a/1"]}`, "names models twice"},
	} {
		if _, err := ParseWorkers([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if _, err := ParseWorkers([]byte(`{"models": ["a/1"], "x": "` + strings.Repeat("x", maxProfileSize) + `"}`)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file past the bound: %v", err)
	}
}

// No file is the default list; a file in the folder is read only as a
// regular file of its own, as a profile is.
func TestLoadWorkers(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadWorkers(dir)
	if err != nil || !got.Default || !slices.Equal(got.Models, DefaultWorkers) {
		t.Fatalf("no file: %+v, %v", got, err)
	}
	path := filepath.Join(dir, WorkersFile)
	if err := os.WriteFile(path, []byte(`{"models": ["deepseek/deepseek-v4-pro"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (Folder{Dir: dir}).Workers(); err != nil || got.Default || len(got.Models) != 1 || got.Models[0].ID != "deepseek-v4-pro" {
		t.Fatalf("the file: %+v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"models": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkers(dir); err == nil || !strings.Contains(err.Error(), WorkersFile) {
		t.Fatalf("an invalid file starts no worker: %v", err)
	}
	// A link, to a list outside the protected folder, is no list.
	outside := filepath.Join(t.TempDir(), "list.json")
	if err := os.WriteFile(outside, []byte(`{"models": ["a/1"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadWorkers(dir); err == nil {
		t.Fatalf("a link was read: %+v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkers(dir); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("a hard link: %v", err)
	}
}
