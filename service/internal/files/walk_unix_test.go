//go:build unix

package files

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"golang.org/x/sys/unix"
)

// fixture makes a home and an outside sibling, using only temporary folders.
func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "home")
	outside := filepath.Join(base, "outside")
	for _, p := range []string{root, outside, filepath.Join(root, "sub"), filepath.Join(root, "notes"), filepath.Join(root, ".git")} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{"AGENTS.md": "hello", "README.md": "readme", ".env": "hidden", "server.key": "key", "api-token.txt": "withheld", "b.md": "b", "A.md": "A", "a.md": "a", "ten": "0123456789"} {
		put(t, filepath.Join(root, name), data)
	}
	put(t, filepath.Join(outside, "secret.txt"), "outside")
	for name, target := range map[string]string{"CLAUDE.md": "AGENTS.md", "sub/up": "../AGENTS.md", "abs": filepath.Join(outside, "secret.txt"), "rel": "../outside/secret.txt", "dir": outside, "notes.md": ".env", "keyfile": "server.key", "a": "b", "b": "a", "d": "missing", "out": "/"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

// put writes a fixture file.
func put(t *testing.T, p, data string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadFollowsALinkInsideTheHome checks canonical paths through links.
func TestReadFollowsALinkInsideTheHome(t *testing.T) {
	root, _ := fixture(t)
	for _, p := range []string{"CLAUDE.md", "sub/up"} {
		f, err := Read(root, p, 100)
		if err != nil || string(f.Data) != "hello" || f.Path != "AGENTS.md" {
			t.Fatalf("%s: %+v %v", p, f, err)
		}
	}
}

// TestALinkOutOfTheHomeIsRefused checks both operations and escape forms.
func TestALinkOutOfTheHomeIsRefused(t *testing.T) {
	root, _ := fixture(t)
	// Validation precedes walking, so the literal secret name is refused first.
	_, want := proto.CheckFilePath("dir/secret.txt")
	if _, err := Read(root, "dir/secret.txt", 100); err == nil || err.Error() != want.Error() {
		t.Fatalf("secret path validation: %v", err)
	}
	if _, err := List(root, "dir/secret.txt", 100, 100); err == nil || err.Error() != want.Error() {
		t.Fatalf("secret path validation: %v", err)
	}
	for _, p := range []string{"abs", "rel", "dir", "dir/public.txt"} {
		if _, err := Read(root, p, 100); !errors.Is(err, ErrOutside) {
			t.Fatalf("Read %s: %v", p, err)
		}
		if _, err := List(root, p, 100, 100); !errors.Is(err, ErrOutside) {
			t.Fatalf("List %s: %v", p, err)
		}
	}
}

// TestDotDotThatStaysInsideIsCleaned checks validation precedes walking.
func TestDotDotThatStaysInsideIsCleaned(t *testing.T) {
	root, _ := fixture(t)
	if f, err := Read(root, "a/../AGENTS.md", 100); err != nil || string(f.Data) != "hello" {
		t.Fatalf("%+v %v", f, err)
	}
	_, want := proto.CheckFilePath("../x")
	for _, read := range []bool{false, true} {
		var err error
		if read {
			_, err = Read(root, "../x", 100)
		} else {
			_, err = List(root, "../x", 100, 100)
		}
		if !errors.Is(err, ErrBadPath) || err.Error() != want.Error() {
			t.Fatal(err)
		}
	}
}

// TestALinkToAHiddenOrWithheldNameIsRefused checks real target names.
func TestALinkToAHiddenOrWithheldNameIsRefused(t *testing.T) {
	root, _ := fixture(t)
	for _, p := range []string{"notes.md", "keyfile"} {
		if _, err := Read(root, p, 100); !errors.Is(err, ErrHidden) {
			t.Fatal(err)
		}
	}
}

// TestALinkLoopIsRefused bounds cyclic link traversal.
func TestALinkLoopIsRefused(t *testing.T) {
	root, _ := fixture(t)
	if _, err := Read(root, "a", 100); !errors.Is(err, ErrOutside) {
		t.Fatal(err)
	}
}

// TestADanglingLinkIsNotFound distinguishes missing targets.
func TestADanglingLinkIsNotFound(t *testing.T) {
	root, _ := fixture(t)
	if _, err := Read(root, "d", 100); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// TestListHidesDotEntriesAndMarksWithheldOnes checks metadata and ordering.
func TestListHidesDotEntriesAndMarksWithheldOnes(t *testing.T) {
	root, _ := fixture(t)
	if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(root, "token-link")); err != nil {
		t.Fatal(err)
	}
	f, err := List(root, "", 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]Entry{}
	names := []string{}
	for _, e := range f.Entries {
		entries[e.Name] = e
		if strings.HasPrefix(e.Name, ".") {
			t.Fatal(e)
		}
		if e.Name == "A.md" || e.Name == "a.md" || e.Name == "b.md" {
			names = append(names, e.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"A.md", "a.md", "b.md"}) {
		t.Fatal(names)
	}
	for _, tt := range []struct {
		name, kind     string
		link, withheld bool
		size           int64
	}{{"api-token.txt", KindFile, false, true, 8}, {"CLAUDE.md", KindFile, true, false, 6}, {"out", KindOther, true, false, 0}, {"fifo", KindOther, false, false, 0}, {"notes", KindFolder, false, false, 0}, {"token-link", KindOther, true, true, 0}} {
		e := entries[tt.name]
		if e.Kind != tt.kind || e.Link != tt.link || e.Withheld != tt.withheld || e.Size != tt.size || e.Modified.IsZero() {
			t.Fatalf("%s: %+v", tt.name, e)
		}
	}
}

// TestListBoundsItsEntries checks both limits independently.
func TestListBoundsItsEntries(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		put(t, filepath.Join(root, name), "")
	}
	f, err := List(root, "", 3, 100)
	if err != nil || len(f.Entries) != 3 || f.More != 4 || f.Partial {
		t.Fatalf("%+v %v", f, err)
	}
	for i, name := range []string{"a", "b", "c"} {
		if f.Entries[i].Name != name {
			t.Fatal(f)
		}
	}
	f, err = List(root, "", 3, 5)
	if err != nil || !f.Partial || f.More+len(f.Entries) != 5 {
		t.Fatalf("%+v %v", f, err)
	}
	empty := t.TempDir()
	f, err = List(empty, "", 3, 5)
	if err != nil || f.Entries == nil || len(f.Entries) != 0 {
		t.Fatalf("%+v %v", f, err)
	}
}

// TestReadOfAFolderPipeOrLargeFile checks nonblocking rejection and bounds.
func TestReadOfAFolderPipeOrLargeFile(t *testing.T) {
	root, _ := fixture(t)
	for _, p := range []string{"notes", "fifo"} {
		if _, err := Read(root, p, 100); !errors.Is(err, ErrNotFile) {
			t.Fatal(err)
		}
	}
	f, err := Read(root, "ten", 9)
	if err != nil || !f.Over || f.Data != nil || f.Size != 10 {
		t.Fatalf("%+v %v", f, err)
	}
	f, err = Read(root, "ten", 10)
	if err != nil || f.Over || string(f.Data) != "0123456789" {
		t.Fatalf("%+v %v", f, err)
	}
}

// TestListOfAFileIsNotAFolder checks the listing's type refusal.
func TestListOfAFileIsNotAFolder(t *testing.T) {
	root, _ := fixture(t)
	if _, err := List(root, "README.md", 10, 10); !errors.Is(err, ErrNotFolder) {
		t.Fatal(err)
	}
}

// TestAMissingRootIsNotFound checks root open errors in both operations.
func TestAMissingRootIsNotFound(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := List(root, "", 10, 10); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := Read(root, "x", 10); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// TestNoDescriptorLeaks repeats successful and failing paths under an fd count.
func TestNoDescriptorLeaks(t *testing.T) {
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"inside", TestReadFollowsALinkInsideTheHome}, {"outside", TestALinkOutOfTheHomeIsRefused}, {"clean", TestDotDotThatStaysInsideIsCleaned}, {"hidden", TestALinkToAHiddenOrWithheldNameIsRefused}, {"loop", TestALinkLoopIsRefused}, {"dangling", TestADanglingLinkIsNotFound}, {"list", TestListHidesDotEntriesAndMarksWithheldOnes}, {"bounds", TestListBoundsItsEntries}, {"read", TestReadOfAFolderPipeOrLargeFile}, {"not folder", TestListOfAFileIsNotAFolder}, {"missing", TestAMissingRootIsNotFound},
	} {
		t.Run(test.name, test.run)
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("fd count: %d -> %d", len(before), len(after))
	}
}
