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

// git runs git outside any sandbox, as only a test does, without the
// developer's git configuration.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
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
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestAddMakesAndFindsTheWorktree(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-ci-t1"
	if err := Add(ctx, git, checkout, path, "vera/fix-ci-t1", "T1"); err != nil {
		t.Fatal(err)
	}
	if head, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != "vera/fix-ci-t1" {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	// A retry finds it.
	if err := Add(ctx, git, checkout, path, "vera/fix-ci-t1", "T1"); err != nil {
		t.Fatalf("a retry: %v", err)
	}
	// A retry after the folder went, the branch kept, adds it again.
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, path, "vera/fix-ci-t1", "T1"); err != nil {
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
	if err := Add(ctx, git, checkout, taken, "vera/taken-t2", "T1"); err == nil {
		t.Error("a folder that is not the worktree was taken")
	}
	if err := Add(ctx, git, t.TempDir(), t.TempDir()+"--x", "vera/x", "T1"); err == nil {
		t.Error("a folder that is not a checkout was taken")
	}
	if err := Add(ctx, git, "relative", "/x", "b", "T1"); err == nil {
		t.Error("a relative checkout was taken")
	}
	// A checkout of another repository on the same branch is not this
	// one's worktree.
	other := repo(t)
	if _, err := git(ctx, other, "checkout", "-q", "-b", "vera/other-t3"); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, other, "vera/other-t3", "T1"); err == nil {
		t.Error("another repository's checkout was taken")
	}
	// A plain folder inside the checkout is not a worktree, though git
	// finds the checkout's HEAD from it.
	inside := filepath.Join(checkout, "sub")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, inside, "main", "T1"); err == nil {
		t.Error("a folder inside the checkout was taken")
	}
	// A profile whose checkout is a folder inside a repository names no
	// checkout: no worktree of the repository around it is made.
	if err := Add(ctx, git, inside, inside+"--vera-x-t4", "vera/x-t4", "T4"); err == nil || !strings.Contains(err.Error(), "inside the checkout") {
		t.Errorf("a checkout's subfolder: %v", err)
	}
}

// A branch or a folder of the generated name that this thread did not make,
// as one an earlier data folder left, is refused, never reused; the thread
// that made them finds them again.
func TestAddRefusesWhatAnotherThreadMade(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	if _, err := git(ctx, checkout, "branch", "vera/fix-ci-t1"); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, checkout+"--vera-fix-ci-t1", "vera/fix-ci-t1", "team:1:5:c1"); err == nil || !strings.Contains(err.Error(), "did not make it") {
		t.Fatalf("an old branch: %v", err)
	}
	if _, err := os.Lstat(checkout + "--vera-fix-ci-t1"); err == nil {
		t.Fatal("a refused worktree was made")
	}
	// Another thread's worktree, from another data folder, keeps its owner.
	path := checkout + "--vera-docs-t2"
	if err := Add(ctx, git, checkout, path, "vera/docs-t2", "team:1:5:c2"); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, path, "vera/docs-t2", "team:1:5:other"); err == nil {
		t.Fatal("another thread took the worktree")
	}
	if err := Add(ctx, git, checkout, path, "vera/docs-t2", "team:1:5:c2"); err != nil {
		t.Fatalf("its own thread: %v", err)
	}
}
