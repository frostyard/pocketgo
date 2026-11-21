//go:build unix

package worktree

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

// A named pipe at a mark's place is no mark, and no mark is made over it:
// the trusted mkdir refuses every entry that is there (Codex review of #46,
// round 7).
func TestAMarkRefusesANamedPipe(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--owner-p"
	if _, err := git(ctx, checkout, "worktree", "add", "-b", "owner/p", path, "HEAD"); err != nil {
		t.Fatal(err)
	}
	place, err := markOf(ctx, tools, path, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(place, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mark(ctx, tools, path, token); !errors.Is(err, ErrTaken) {
		t.Fatalf("a named pipe at the mark's place: %v", err)
	}
	if err := Marked(ctx, tools, path, token); !errors.Is(err, ErrTaken) {
		t.Fatalf("a named pipe passed for a mark: %v", err)
	}
}
