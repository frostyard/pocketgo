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
