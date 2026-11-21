package uploads

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

var at = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// testFolder is an uploads folder in ~/.local/share of a home of its own,
// its files named at a fixed time with a fixed random part.
func testFolder(t *testing.T) (*Folder, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "share"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := New(Dir(home), func() time.Time { return at })
	f.random = func() string { return "0a1b2c3d" }
	return f, home
}

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

// An upload is kept in its session's folder, mode 0600 in folders of mode
// 0700, read back as it was, and removed on request; a cut-off upload
// leaves nothing.
func TestStoreReadAndRemove(t *testing.T) {
	f, home := testFolder(t)
	file, size, err := f.Store(7, "shot.png", strings.NewReader("pixels"))
	if err != nil || size != 6 || file != "mwoxfuo0-0a1b2c3d-shot.png" {
		t.Fatalf("Store = %q, %d, %v", file, size, err)
	}
	path := f.Path(7, file)
	if want := filepath.Join(home, ".local", "share", "pocketgo-uploads", "7", file); path != want {
		t.Fatalf("Path = %q, want %q", path, want)
	}
	for p, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700 | fs.ModeDir, filepath.Dir(filepath.Dir(path)): 0o700 | fs.ModeDir} {
		if info, err := os.Lstat(p); err != nil || info.Mode() != mode {
			t.Errorf("%s: %v, %v (want %v)", p, info.Mode(), err, mode)
		}
	}
	if data, err := f.Read(7, file, 6); err != nil || string(data) != "pixels" {
		t.Fatalf("Read = %q, %v", data, err)
	}
	if _, err := f.Read(7, file, 5); !errors.Is(err, ErrGrown) {
		t.Fatalf("a file larger than its upload read: %v", err)
	}
	if err := f.Remove(7, file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the removed file is there: %v", err)
	}
	if err := f.Remove(7, file); err != nil {
		t.Fatalf("removing a file that is gone: %v", err)
	}
	if _, err := f.Read(7, "../7/"+file, 6); err == nil {
		t.Fatal("a name with a path read")
	}

	// A body that fails partway leaves no file.
	f.random = func() string { return "ffffffff" }
	if _, _, err := f.Store(7, "cut.bin", io.MultiReader(strings.NewReader("half"), failing{})); err == nil {
		t.Fatal("a cut-off upload was stored")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 0 {
		t.Fatalf("a cut-off upload left %v, %v", entries, err)
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("the connection went") }

// An agent can write where uploads live, so pocketd follows no link it
// planted there: not as the uploads folder, not as a session's folder,
// not at a new file's name, and not out of the session's folder when it
// reads. Nothing is written where the links point (ADR-0029).
func TestALinkAnAgentPlantedIsNeverFollowed(t *testing.T) {
	f, home := testFolder(t)
	data := filepath.Join(home, ".local", "state", "pocketgo")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(data, "auth.json")
	if err := os.WriteFile(secret, []byte(`{"anthropic":"sk-secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	untouched := func(what string) {
		t.Helper()
		entries, err := os.ReadDir(data)
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s: the data folder holds %v, %v", what, entries, err)
		}
		if b, err := os.ReadFile(secret); err != nil || string(b) != `{"anthropic":"sk-secret-value"}` {
			t.Fatalf("%s: the sign-ins changed: %q, %v", what, b, err)
		}
	}

	// The uploads folder is a link to the data folder, absolute or a
	// sibling of it inside ~/.local/share.
	share := filepath.Dir(f.Base())
	if err := os.Symlink(data, f.Base()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Store(7, "x.txt", strings.NewReader("x")); err == nil {
		t.Fatal("stored through a link as the uploads folder")
	}
	untouched("uploads folder linked")
	_ = os.Remove(f.Base())
	inside := filepath.Join(share, "systemd")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("systemd", f.Base()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Store(7, "x.txt", strings.NewReader("x")); !errors.Is(err, ErrNotItsFolder) {
		t.Fatalf("stored through a link inside ~/.local/share: %v", err)
	}
	if entries, _ := os.ReadDir(inside); len(entries) != 0 {
		t.Fatalf("the linked folder got %v", entries)
	}
	_ = os.Remove(f.Base())

	// A session's folder is a link.
	if err := os.MkdirAll(f.Base(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(data, filepath.Join(f.Base(), "7")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Store(7, "x.txt", strings.NewReader("x")); err == nil {
		t.Fatal("stored through a link as the session's folder")
	}
	if _, err := f.Read(7, "auth.json", 1<<20); err == nil {
		t.Fatal("read through a link as the session's folder")
	}
	untouched("session folder linked")
	_ = os.Remove(filepath.Join(f.Base(), "7"))

	// A link waits at the name the next file takes.
	if err := os.MkdirAll(filepath.Join(f.Base(), "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	next := f.Path(7, FileName(at, "0a1b2c3d", "x.txt"))
	if err := os.Symlink(filepath.Join(data, "planted.txt"), next); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Store(7, "x.txt", strings.NewReader("x")); err == nil {
		t.Fatal("stored through a link at the new file's name")
	}
	untouched("file name linked")
	if info, err := os.Lstat(next); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("the planted link was replaced: %v, %v", info, err)
	}

	// A file that is a link out of the session's folder, absolute or
	// relative, is not read.
	for _, target := range []string{secret, filepath.Join("..", "..", "..", "state", "pocketgo", "auth.json")} {
		_ = os.Remove(next)
		if err := os.Symlink(target, next); err != nil {
			t.Fatal(err)
		}
		if b, err := f.Read(7, filepath.Base(next), 1<<20); err == nil || bytes.Contains(b, []byte("sk-secret")) {
			t.Fatalf("read the sign-ins through %s: %q, %v", target, b, err)
		}
	}
	untouched("file linked out")
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

// A link at an upload's name is not read, even one to another upload of
// the session's folder: the model would get that file's bytes under this
// one's name (Codex review of #49, round 1).
func TestALinkToAnotherUploadIsNotRead(t *testing.T) {
	f, _ := testFolder(t)
	other, _, err := f.Store(7, "other.png", strings.NewReader("other bytes"))
	if err != nil {
		t.Fatal(err)
	}
	f.random = func() string { return "ffffffff" }
	mine, _, err := f.Store(7, "mine.png", strings.NewReader("my bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.Path(7, mine)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, f.Path(7, mine)); err != nil {
		t.Fatal(err)
	}
	if b, err := f.Read(7, mine, 100); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("read %q through a link to another upload: %v", b, err)
	}
}

// A folder of the uploads folder open to other users, made so before
// pocketd came, is the service user's alone again once pocketd uses it
// (Codex review of #49, round 1).
func TestAnOpenFolderIsMadePrivate(t *testing.T) {
	f, _ := testFolder(t)
	for _, dir := range []string{f.Base(), filepath.Join(f.Base(), "7")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.Store(7, "a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{f.Base(), filepath.Join(f.Base(), "7")} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s is %v, %v", dir, info.Mode(), err)
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
