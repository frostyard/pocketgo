package worktree

import (
	"context"
	"errors"
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
	if err := Add(ctx, tools, checkout, path, "vera/fix-ci-t1"); err != nil {
		t.Fatal(err)
	}
	if head, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != "vera/fix-ci-t1" {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	// A retry finds it.
	if err := Add(ctx, tools, checkout, path, "vera/fix-ci-t1"); err != nil {
		t.Fatalf("a retry: %v", err)
	}
	// A retry after the folder went, the branch kept, adds it again.
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, tools, checkout, path, "vera/fix-ci-t1"); err != nil {
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
	if err := Add(ctx, tools, checkout, taken, "vera/taken-t2"); err == nil {
		t.Error("a folder that is not the worktree was taken")
	}
	if err := Add(ctx, tools, t.TempDir(), t.TempDir()+"--x", "vera/x"); err == nil {
		t.Error("a folder that is not a checkout was taken")
	}
	if err := Add(ctx, tools, "relative", "/x", "b"); err == nil {
		t.Error("a relative checkout was taken")
	}
	// A checkout of another repository on the same branch is not this
	// one's worktree.
	other := repo(t)
	if _, err := git(ctx, other, "checkout", "-q", "-b", "vera/other-t3"); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, tools, checkout, other, "vera/other-t3"); err == nil {
		t.Error("another repository's checkout was taken")
	}
	// A plain folder inside the checkout is not a worktree, though git
	// finds the checkout's HEAD from it.
	inside := filepath.Join(checkout, "sub")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, tools, checkout, inside, "main"); err == nil {
		t.Error("a folder inside the checkout was taken")
	}
	// A profile whose checkout is a folder inside a repository names no
	// checkout: no worktree of the repository around it is made.
	if err := Add(ctx, tools, inside, inside+"--vera-x-t4", "vera/x-t4"); err == nil || !strings.Contains(err.Error(), "inside the checkout") {
		t.Errorf("a checkout's subfolder: %v", err)
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

// A worktree a stopped try left half made (git's lock still on it) is
// refused, never handed to a specialist.
func TestAHalfMadeWorktreeIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	dir, err := git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "locked"), []byte("initializing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Add(ctx, tools, checkout, path, "vera/fix-t1")
	if err == nil || !strings.Contains(err.Error(), "is locked (see "+filepath.Join(dir, "locked")+")") || strings.Contains(err.Error(), "--force") {
		t.Fatalf("a half-made worktree: %v", err)
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
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	if why, locked := lockOf(ctx, tools, path); locked {
		t.Fatalf("an unlocked worktree reads as locked: %s", why)
	}
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
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
	if err := Add(ctx, tools, checkout, checkout+"--vera-fix-t1", "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "not a worktree") {
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
	if err := Add(ctx, tools, checkout, checkout+"--vera-fix-t1", "vera/fix-t1"); err != nil {
		t.Fatalf("a worktree beside a tag of its branch's name: %v", err)
	}
}

// kind looks outside any sandbox, as only a test does.
func kind(_ context.Context, path string) (string, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "absent", nil
	case err != nil:
		return "", err
	case info.Mode()&os.ModeSymlink != 0:
		return "link", nil
	case info.Mode().IsRegular():
		return "file", nil
	case info.IsDir():
		return "dir", nil
	}
	return "other", nil
}

// same compares outside any sandbox, as only a test does.
func same(_ context.Context, a, b string) (bool, error) {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib), nil
}

// read reads outside any sandbox, as only a test does.
func read(_ context.Context, path string) (string, bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false, nil
	}
	data, err := os.ReadFile(path)
	if len(data) > 4096 {
		data = data[:4096]
	}
	return string(data), err == nil, err
}

// tools are a test's Tools, outside any sandbox.
var tools = Tools{Git: git, Kind: kind, Read: read, Same: same}

// A worktree whose branch lost its ref (an unborn branch) has no commit to
// start from: it is refused.
func TestAWorktreeWithoutACommitIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, path, "checkout", "-q", "--orphan", "vera/fix-t1-orphan"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, path, "symbolic-ref", "HEAD", "refs/heads/vera/fix-t1-gone"); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1-gone"); err == nil {
		t.Fatal("a worktree on a branch with no commit was taken")
	}
}

// A copy of a worktree, its .git file pointing at a worktree git
// registered elsewhere, is refused.
func TestACopiedWorktreeIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	moved := checkout + "--moved"
	if _, err := git(ctx, checkout, "worktree", "move", path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(moved, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "not the worktree git registered") {
		t.Fatalf("a copied worktree: %v", err)
	}
}

// A backlink git wrote relative to the worktree's git folder
// (worktree.useRelativePaths) names the worktree as an absolute one does.
func TestARelativeBacklinkIsRegistered(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	dir, err := git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(dir, filepath.Join(path, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gitdir"), []byte(relative+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatalf("a relative backlink: %v", err)
	}
}

// A copy whose .git is a link to the registered worktree's .git file is
// not that worktree: its index is the other's.
func TestALinkedCopyIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	moved := checkout + "--moved"
	if _, err := git(ctx, checkout, "worktree", "move", path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(moved, ".git"), filepath.Join(path, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err == nil {
		t.Fatal("a copy linked to another worktree's .git was taken")
	}
}
