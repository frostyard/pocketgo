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
	cmd.Env = gitEnv()
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
		cmd.Env = gitEnv()
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

// gitEnv is the test's environment without git's repository variables
// (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE and the rest) and without the
// developer's git configuration, so a fixture never works on another
// repository.
func gitEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}

// A name's claim is exclusive: once one owner claimed it, another is
// refused even while neither the branch nor the folder exists yet.
func TestAClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	if err := claim(ctx, git, checkout, "vera/fix-t1", "owner-a"); err != nil {
		t.Fatal(err)
	}
	if err := claim(ctx, git, checkout, "vera/fix-t1", "owner-b"); err == nil {
		t.Fatal("a second owner claimed the name")
	}
	if err := Add(ctx, git, checkout, checkout+"--vera-fix-t1", "vera/fix-t1", "owner-b"); err == nil {
		t.Fatal("a second owner made the worktree")
	}
	if err := claim(ctx, git, checkout, "vera/fix-t1", "owner-a"); err != nil {
		t.Fatalf("the owner again: %v", err)
	}
	if err := Add(ctx, git, checkout, checkout+"--vera-fix-t1", "vera/fix-t1", "owner-a"); err != nil {
		t.Fatalf("the owner makes it: %v", err)
	}
	if _, err := git(ctx, checkout, "rev-parse", "--verify", "--quiet", "refs/heads/pocketgo/threads/vera/fix-t1"); err == nil {
		t.Fatal("the claim is a branch")
	}
}

// A worktree a stopped try left half made (git's lock still on it) is
// refused, never handed to a specialist.
func TestAHalfMadeWorktreeIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, git, checkout, path, "vera/fix-t1", "owner-a"); err != nil {
		t.Fatal(err)
	}
	dir, err := git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "locked"), []byte("initializing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Add(ctx, git, checkout, path, "vera/fix-t1", "owner-a")
	if err == nil || !strings.Contains(err.Error(), "is locked (see "+filepath.Join(dir, "locked")+")") || strings.Contains(err.Error(), "--force") {
		t.Fatalf("a half-made worktree: %v", err)
	}
}

// A claim git itself refuses (here a ref named like a folder that holds
// the claim's) says git's reason, not that another thread holds it.
func TestAClaimGitRefusesSaysWhy(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	head, err := git(ctx, checkout, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, checkout, "update-ref", "refs/pocketgo/threads/vera", head); err != nil {
		t.Fatal(err)
	}
	err = claim(ctx, git, checkout, "vera/fix-t1", "owner-a")
	if err == nil || strings.Contains(err.Error(), "another thread") || !strings.Contains(err.Error(), "claim the branch") {
		t.Fatalf("claim: %v", err)
	}
}

// A worktree whose path holds a newline is found in git's list, unlocked.
func TestAWorktreeWithANewlineInItsPathIsFound(t *testing.T) {
	ctx := context.Background()
	parent := filepath.Join(t.TempDir(), "odd\nname")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(parent, "example")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", checkout},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, git, checkout, path, "vera/fix-t1", "owner-a"); err != nil {
		t.Fatal(err)
	}
	if why, locked := lockOf(ctx, git, path); locked {
		t.Fatalf("an unlocked worktree reads as locked: %s", why)
	}
	if err := Add(ctx, git, checkout, path, "vera/fix-t1", "owner-a"); err != nil {
		t.Fatalf("a retry: %v", err)
	}
}

// A new worktree whose post-checkout hook switched its branch is refused:
// the specialist never gets a worktree on another branch than its brief's.
func TestANewWorktreeIsCheckedAfterItsHooks(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	// The hook's own checkout runs it again: only the first one moves.
	hook := "#!/bin/sh\n[ \"$(git rev-parse --abbrev-ref HEAD)\" = elsewhere ] || git checkout -q -b elsewhere\n"
	if err := os.WriteFile(filepath.Join(checkout, ".git", "hooks", "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, checkout+"--vera-fix-t1", "vera/fix-t1", "owner-a"); err == nil || !strings.Contains(err.Error(), "not a worktree") {
		t.Fatalf("a worktree its hook moved: %v", err)
	}
}

// A tag of the branch's name does not hide the worktree's branch.
func TestAWorktreeBesideATagOfItsNameIsFound(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	if _, err := git(ctx, checkout, "tag", "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, git, checkout, checkout+"--vera-fix-t1", "vera/fix-t1", "owner-a"); err != nil {
		t.Fatalf("a worktree beside a tag of its branch's name: %v", err)
	}
}
