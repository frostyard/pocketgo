//go:build unix

package uploads

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

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
