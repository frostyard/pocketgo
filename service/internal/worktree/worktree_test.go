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
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), gitExit(err), out)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitExit is git's own exit as a *GitExit, as a Git that ran git itself
// knows it; any other error as it is.
func gitExit(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return &GitExit{Status: exit.ExitCode()}
	}
	return err
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
	if lock, err := lockOf(ctx, tools, path); err != nil || lock != "" {
		t.Fatalf("an unlocked worktree reads as locked: %q, %v", lock, err)
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

// A worktree whose git folder was moved out of the repository's registry
// is not a worktree git lists: it is refused.
func TestAnOrphanedWorktreeIsRefused(t *testing.T) {
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
	orphan := filepath.Join(t.TempDir(), "orphan")
	if err := os.Rename(dir, orphan); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+orphan+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err == nil {
		t.Fatal("a worktree out of git's registry was taken")
	}
}

// A check that could not run says so, with its cause, apart from a
// worktree that is not as it must be: a probe that fails, or a check that
// runs out of time, never reads as "not a worktree" (Codex review round 22).
func TestACheckThatCouldNotRunKeepsItsCause(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("bwrap: cannot set up the sandbox")
	for name, tl := range map[string]Tools{
		"same": {Git: git, Kind: kind, Read: read, Same: func(context.Context, string, string) (bool, error) { return false, broken }},
		"read": {Git: git, Kind: kind, Same: same, Read: func(context.Context, string) (string, bool, error) { return "", false, broken }},
		"kind": {Git: git, Read: read, Same: same, Kind: func(_ context.Context, p string) (string, error) {
			if strings.HasSuffix(p, ".git") {
				return "", broken
			}
			return kind(ctx, p)
		}},
		"git": {Kind: kind, Read: read, Same: same, Git: func(ctx context.Context, dir string, args ...string) (string, error) {
			if dir == path {
				return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), broken)
			}
			return git(ctx, dir, args...)
		}},
	} {
		err := Usable(ctx, tl, checkout, path, "vera/fix-t1")
		if err == nil || !errors.Is(err, broken) || !strings.Contains(err.Error(), "could not check") ||
			strings.Contains(err.Error(), "not a worktree") || strings.Contains(err.Error(), "not the worktree") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A check out of time says it did not finish, also when the probe that
	// ran out answered no.
	done, cancel := context.WithCancel(ctx)
	late := Tools{Git: git, Kind: kind, Read: read, Same: func(context.Context, string, string) (bool, error) {
		cancel()
		return false, nil
	}}
	if err := Usable(done, late, checkout, path, "vera/fix-t1"); err == nil || !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "did not finish") || strings.Contains(err.Error(), "not a worktree") {
		t.Errorf("a check out of time: %v", err)
	}
	if err := Usable(done, tools, checkout, path, "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("a check begun out of time: %v", err)
	}
	// git that ran and said no is a negative, with git's word for it.
	if _, err := git(ctx, path, "checkout", "-q", "--detach"); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "not a worktree") ||
		!strings.Contains(err.Error(), "symbolic-ref") {
		t.Errorf("a detached worktree: %v", err)
	}
}

// The checkout is checked as the top of its repository also when the
// worktree is checked again: a checkout link pointed at a folder inside the
// repository since shares its common folder, and is refused (Codex review
// round 23).
func TestUsableRefusesACheckoutThatIsNoLongerItsRepositorysTop(t *testing.T) {
	ctx := context.Background()
	repository := repo(t)
	checkout := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repository, checkout); err != nil {
		t.Fatal(err)
	}
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatalf("the worktree as made: %v", err)
	}
	inside := filepath.Join(repository, "inside")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(checkout); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, checkout); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "not a checkout") {
		t.Fatalf("a checkout pointed inside its repository: %v", err)
	}
}

// A link at the worktree's path, left where git moved the worktree from,
// is refused: through it every other check would pass (Codex review round
// 23).
func TestALinkAtTheWorktreesPathIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if _, err := git(ctx, checkout, "worktree", "move", path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "not a worktree") {
		t.Fatalf("a link at the worktree's path: %v", err)
	}
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err == nil {
		t.Fatal("Add took a link at the worktree's path")
	}
}

// A branch lookup that could not tell makes nothing: no worktree add runs,
// and the lookup's error is kept (Codex review round 23).
func TestABranchLookupThatFailsMakesNothing(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path := checkout + "--vera-fix-t1"
	broken := errors.New("bwrap: cannot set up the sandbox")
	var calls []string
	tl := tools
	tl.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "for-each-ref" {
			return "", broken
		}
		return git(ctx, dir, args...)
	}
	if err := Add(ctx, tl, checkout, path, "vera/fix-t1"); err == nil || !errors.Is(err, broken) {
		t.Fatalf("a lookup that failed: %v", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "worktree add") {
			t.Fatalf("git %s ran after a lookup that failed", call)
		}
	}
	if k, _ := kind(ctx, path); k != "absent" {
		t.Fatalf("the worktree's path is %s", k)
	}
	// A branch that is there is found, and the worktree made on it.
	if _, err := git(ctx, checkout, "branch", "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	if exists, err := hasBranch(ctx, git, checkout, "vera/fix-t1"); err != nil || !exists {
		t.Fatalf("the branch: %v, %v", exists, err)
	}
	if exists, err := hasBranch(ctx, git, checkout, "vera/fix"); err != nil || exists {
		t.Fatalf("a prefix of the branch: %v, %v", exists, err)
	}
	if err := Add(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
}

// A relative backlink is resolved as written, not cleaned first: one
// through a folder that is not there does not name the worktree, however
// its ".." reads as a string (Codex review round 24).
func TestABacklinkIsResolvedAsWritten(t *testing.T) {
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
	rel, err := filepath.Rel(dir, filepath.Join(path, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(dir, "gitdir")
	// The backlink relative, as worktree.useRelativePaths writes it: good.
	if err := os.WriteFile(gitdir, []byte(rel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatalf("a relative backlink: %v", err)
	}
	// Through a folder that is not there: it names nothing.
	if err := os.WriteFile(gitdir, []byte("missing/../"+rel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "not the worktree git registered") {
		t.Fatalf("a backlink through a missing folder: %v", err)
	}
}
