//go:build unix

package uploads

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

// An upload is kept in its session's folder, mode 0600 in folders of mode
// 0700, and read back as it was; a cut-off upload leaves nothing, not even
// a file without its name.
func TestStoreAndRead(t *testing.T) {
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
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
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

// A named pipe at a file's name is no file: pocketd neither waits on it
// nor reads it, and a folder there is no file either.
func TestOnlyARegularFileIsRead(t *testing.T) {
	f, _ := testFolder(t)
	if _, _, err := f.Store(7, "a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	pipe := f.Path(7, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	if _, err := f.Read(7, "pipe", 10); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("a named pipe read: %v", err)
	}
	if err := os.Mkdir(f.Path(7, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(7, "dir", 10); err == nil {
		t.Fatal("a folder read as a file")
	}
}

// A named pipe an agent put where a folder of the uploads folder goes is
// refused at once: pocketd opens each folder as a folder, never waiting on
// what stands there (Codex review of #49, round 3).
func TestAPipeIsNeverWaitedOn(t *testing.T) {
	f, _ := testFolder(t)
	if err := syscall.Mkfifo(f.Base(), 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	if _, _, err := f.Store(7, "x.txt", strings.NewReader("x")); !errors.Is(err, ErrNotItsFolder) {
		t.Fatalf("a pipe as the uploads folder: %v", err)
	}
	if err := os.Remove(f.Base()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.Base(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(f.Base(), "7"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Store(7, "x.txt", strings.NewReader("x")); !errors.Is(err, ErrNotItsFolder) {
		t.Fatalf("a pipe as the session's folder: %v", err)
	}
	if _, err := f.Read(7, "x.txt", 1); !errors.Is(err, ErrNotItsFolder) {
		t.Fatalf("a read through a pipe as the session's folder: %v", err)
	}
}

// What tells a file pocketd made apart holds while its name holds that
// file, and no longer once an agent put another there: pocketd finishes
// only its own (Codex review of #49, round 3).
func TestAFileIsToldApartByWhatItIs(t *testing.T) {
	f, _ := testFolder(t)
	staged, err := f.Stage(7, strings.NewReader("mine"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	file := f.NewFile("a.txt")
	if ours, err := f.Holds(7, file, staged.Identity); err != nil || ours {
		t.Fatalf("a name that holds nothing holds the file: %v, %v", ours, err)
	}
	if err := staged.Link(file); err != nil {
		t.Fatal(err)
	}
	if ours, err := f.Holds(7, file, staged.Identity); err != nil || !ours {
		t.Fatalf("the file named is not told as pocketd's: %v, %v", ours, err)
	}
	if err := os.Remove(f.Path(7, file)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path(7, file), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ours, err := f.Holds(7, file, staged.Identity); err != nil || ours {
		t.Fatalf("an agent's file of the same bytes is told as pocketd's: %v, %v", ours, err)
	}
}
