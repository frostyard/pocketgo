//go:build unix

package state

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A named pipe is no file to attach: FileSource neither waits on it nor
// reads it.
func TestANamedPipeIsNoFileToAttach(t *testing.T) {
	pipe := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	if _, _, err := FileSource(pipe).Open(); err == nil {
		t.Fatal("a named pipe attached")
	}
}
