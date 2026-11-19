package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs git outside any sandbox, as only a test does.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// repo is a git checkout with one commit, in a temporary folder.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := filepath.Join(t.TempDir(), "example")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestAddMakesAndFindsTheWorktree(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-ci-t1"
	if err := Add(ctx, git, checkout, path, "vera/fix-ci-t1"); err != nil {
		t.Fatal(err)
	}
	if head, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != "vera/fix-ci-t1" {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	// A retry finds it.
	if err := Add(ctx, git, checkout, path, "vera/fix-ci-t1"); err != nil {
		t.Fatalf("a retry: %v", err)
	}
	// A retry after the folder went, the branch kept, adds it again.
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, path, "vera/fix-ci-t1"); err != nil {
		t.Fatalf("a retry with the branch only: %v", err)
	}
}

func TestAddRefusesWhatItCannotUse(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	taken := checkout + "--vera-taken-t2"
	if err := os.Mkdir(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, taken, "vera/taken-t2"); err == nil {
		t.Error("a folder that is not the worktree was taken")
	}
	if err := Add(ctx, git, t.TempDir(), t.TempDir()+"--x", "vera/x"); err == nil {
		t.Error("a folder that is not a checkout was taken")
	}
	if err := Add(ctx, git, "relative", "/x", "b"); err == nil {
		t.Error("a relative checkout was taken")
	}
	// A checkout of another repository on the same branch is not this
	// one's worktree.
	other := repo(t)
	if _, err := git(ctx, other, "checkout", "-q", "-b", "vera/other-t3"); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, other, "vera/other-t3"); err == nil {
		t.Error("another repository's checkout was taken")
	}
	// A plain folder inside the checkout is not a worktree, though git
	// finds the checkout's HEAD from it.
	inside := filepath.Join(checkout, "sub")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, inside, "main"); err == nil {
		t.Error("a folder inside the checkout was taken")
	}
}
