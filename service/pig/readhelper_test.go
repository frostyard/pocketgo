package pig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ReadHelper checks the descriptor it opened, and reads from it alone: a
// regular file up to its bound, and none for a named pipe with data
// waiting, a folder, a device or no file (Codex review of #57, round 4).
func TestTheReadHelperChecksWhatItOpened(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(regular, []byte("Find dead code."), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(args ...string) (string, int) {
		var out strings.Builder
		code := RunReadHelper(args, &out)
		return out.String(), code
	}
	if out, code := read(regular, "4"); code != 0 || out != "file\nFind" {
		t.Fatalf("a regular file: %q %d", out, code)
	}
	for name, path := range map[string]string{"a folder": dir, "a device": os.DevNull, "no file": filepath.Join(dir, "missing")} {
		if out, code := read(path, "100"); code != 0 || out != "none\n" {
			t.Errorf("%s: %q %d", name, out, code)
		}
	}
	if _, code := read(regular); code != 2 {
		t.Errorf("one argument: %d", code)
	}
	if _, code := read(regular, "-1"); code != 2 {
		t.Errorf("a negative bound: %d", code)
	}
	pipe := filepath.Join(dir, "pipe")
	if err := mkfifo(pipe); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	// The test holds both ends, with data waiting, so no open waits: the
	// helper reads none of it.
	ends, err := os.OpenFile(pipe, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ends.Close() }()
	if _, err := ends.WriteString("sk-secret-value"); err != nil {
		t.Fatal(err)
	}
	if out, code := read(pipe, "100"); code != 0 || out != "none\n" {
		t.Fatalf("a named pipe: %q %d", out, code)
	}
	f, err := openNonblocking(regular)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if on, err := nonblocking(f); err != nil {
		t.Skipf("no descriptor flags here: %v", err)
	} else if !on {
		t.Error("the helper's open waits for a writer: its descriptor has no O_NONBLOCK")
	}
}

// Engine.ReadUpTo reads through pocketd's own ReadHelper, run as a command
// in the sandbox, never a check and a read apart (Codex review of #57,
// round 4).
func TestReadUpToRunsTheReadHelper(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "pocketd")
	script := "#!/bin/sh\necho file\nprintf '%s|%s|%s' \"$1\" \"$2\" \"$3\"\n"
	if err := os.WriteFile(self, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Unsandboxed: true, Self: self}, commands: map[string]string{"PATH": os.Getenv("PATH")}}
	data, ok, err := e.ReadUpTo(context.Background(), "/skills/grooming/SKILL.md", 100)
	if err != nil || !ok || string(data) != ReadHelper+"|/skills/grooming/SKILL.md|101" {
		t.Fatalf("the read: %q %v %v", data, ok, err)
	}
}
