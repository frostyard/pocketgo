package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session's new worktree takes no path or branch that is there already:
// its request did not make it (ADR-0027; Codex review of #46, round 2). A
// folder that is no checkout says so as Add does.
func TestFreeRefusesWhatIsThereAlready(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path, branch := checkout+"--owner-session-qlz6tr", "owner/session-qlz6tr"
	if err := Free(ctx, tools, checkout, path, branch); err != nil {
		t.Fatalf("a free name: %v", err)
	}
	if err := Add(ctx, tools, checkout, path, branch); err != nil {
		t.Fatal(err)
	}
	if err := Free(ctx, tools, checkout, path, branch); !errors.Is(err, ErrTaken) || Unsure(err) {
		t.Fatalf("a worktree there: %v", err)
	}
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := Free(ctx, tools, checkout, path, branch); !errors.Is(err, ErrTaken) || !strings.Contains(err.Error(), "the branch "+branch) {
		t.Fatalf("its branch kept: %v", err)
	}
	if err := Free(ctx, tools, checkout, path+"-x", "owner/other"); err != nil {
		t.Fatalf("another name: %v", err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Free(ctx, tools, plain, plain+"--owner-x", "owner/x"); err == nil || Unsure(err) || !strings.Contains(err.Error(), "is not a git checkout") {
		t.Fatalf("a folder that is no checkout: %v", err)
	}
}

// AddNew makes a worktree on a new branch, and takes no worktree or
// branch that is there, as one another pocketd made in a race (Codex
// review of #46, round 3).
func TestAddNewTakesNothingThatIsThere(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path, branch := checkout+"--owner-session-qlz6tr", "owner/session-qlz6tr"
	if err := AddNew(ctx, tools, checkout, path, branch); err != nil {
		t.Fatal(err)
	}
	if head, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != branch {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch); !errors.Is(err, ErrTaken) {
		t.Fatalf("its worktree there: %v", err)
	}
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch); err == nil || Unsure(err) || !strings.Contains(err.Error(), "on a new branch") {
		t.Fatalf("its branch there: %v", err)
	}
	if kind, _ := kind(ctx, path); kind != "absent" {
		t.Fatalf("a refused AddNew left %s", kind)
	}
}

// Top names the checkout as git does, through a link to a folder above it
// too, so a link moved since names another top (Codex review of #46,
// round 3).
func TestTopResolvesLinks(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(checkout), alias); err != nil {
		t.Fatal(err)
	}
	real, err := Top(ctx, tools, checkout)
	if err != nil {
		t.Fatal(err)
	}
	through, err := Top(ctx, tools, filepath.Join(alias, filepath.Base(checkout)))
	if err != nil || through != real {
		t.Fatalf("Top through a link = %q, %v; want %q", through, err, real)
	}
	other := repo(t)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(other), alias); err != nil {
		t.Fatal(err)
	}
	if moved, err := Top(ctx, tools, filepath.Join(alias, filepath.Base(other))); err != nil || moved == real {
		t.Fatalf("Top after the link moved = %q, %v", moved, err)
	}
}
