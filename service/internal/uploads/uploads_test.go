package uploads

import (
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

var at = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// A name is kept as pi-pocket keeps it: its last path element, each run
// of other characters one "_", trimmed, at most 120 bytes, and "upload"
// before one that is empty or starts with a dot. No comma survives.
func TestSafeName(t *testing.T) {
	for name, want := range map[string]string{
		"shot.png":                      "shot.png",
		"report (1).pdf":                "report (1).pdf",
		"../../etc/passwd":              "passwd",
		"dir/sub/":                      "sub",
		"a, b; c.txt":                   "a_ b_ c.txt",
		"Bildschirmfoto äöü 1.png":      "Bildschirmfoto _ 1.png",
		"日本語.txt":                       "_.txt",
		"  spaced  ":                    "spaced",
		"":                              "upload",
		".bashrc":                       "upload.bashrc",
		"a\nb\r\x00c":                   "a_b_c",
		strings.Repeat("x", 130):        strings.Repeat("x", 120),
		strings.Repeat("y", 119) + " z": strings.Repeat("y", 119),
	} {
		if got := SafeName(name); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", name, got, want)
		}
	}
	if got := FileName(at, "0a1b2c3d", "a/b.png"); got != "mwoxfuo0-0a1b2c3d-b.png" {
		t.Errorf("FileName = %q", got)
	}
}

// An upload's type is the declared one, without parameters and in
// lowercase, unless it is none, application/octet-stream or no type; then
// its extension's in pocketd's own table; else application/octet-stream.
func TestMime(t *testing.T) {
	for _, c := range []struct{ name, declared, want string }{
		{"a.png", "image/png", "image/png"},
		{"a.bin", "Text/Plain; charset=utf-8", "text/plain"},
		{"a.png", "", "image/png"},
		{"a.JPG", "application/octet-stream", "image/jpeg"},
		{"a.pdf", "not a type", "application/pdf"},
		{"a.txt", "text/plain, image/png", "text/plain"},
		{"a.unknown", "", "application/octet-stream"},
		{"noext", "garbage)", "application/octet-stream"},
	} {
		if got := Mime(c.name, c.declared); got != c.want {
			t.Errorf("Mime(%q, %q) = %q, want %q", c.name, c.declared, got, c.want)
		}
	}
}

// A safe name is its own safe name, at most 120 bytes, so a file keeps the
// name its record names, as a message's line must (Codex review of #49,
// round 1: a dot-prefixed name cut twice lost its end).
func TestASafeNameIsItsOwn(t *testing.T) {
	for _, name := range []string{".bashrc", "." + strings.Repeat("x", 130), strings.Repeat("y", 200), "", "  ", "a/b/.c", ". (1)", "x" + strings.Repeat(" ", 130) + "y"} {
		safe := SafeName(name)
		if SafeName(safe) != safe || len(safe) > maxName || len(safe) > proto.MaxAttachedName || safe == "" {
			t.Errorf("SafeName(%q) = %q, and again %q", name, safe, SafeName(safe))
		}
		if file := FileName(at, "0a1b2c3d", name); !strings.HasSuffix(file, "-"+safe) {
			t.Errorf("FileName(%q) = %q, which does not end in -%q", name, file, safe)
		}
	}
}

// A declared type longer than a message's line takes is not kept: the
// name's type is, so the desktop reads every file of the message (Codex
// review of #49, round 2).
func TestATypeLongerThanALineTakesIsNotKept(t *testing.T) {
	long := "text/" + strings.Repeat("x", proto.MaxMimeType)
	if got := Mime("notes.md", long); got != "text/markdown" {
		t.Fatalf("Mime kept %d bytes: %q", len(got), got)
	}
	at := "text/" + strings.Repeat("x", proto.MaxMimeType-len("text/"))
	if got := Mime("notes.md", at); got != at {
		t.Fatalf("Mime refused a type of %d bytes", len(at))
	}
}
