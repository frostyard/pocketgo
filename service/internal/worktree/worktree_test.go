package worktree

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/team"
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

// each runs git outside any sandbox and streams its records, as only a
// test does.
func each(ctx context.Context, dir string, sep byte, fn func(string) error, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := bufio.NewReader(out)
	var stop error
	for stop == nil {
		record, err := r.ReadString(sep)
		if record != "" {
			stop = fn(strings.TrimSuffix(record, string(sep)))
		}
		if err != nil {
			break
		}
	}
	if stop != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return stop
	}
	return gitExit(cmd.Wait())
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
var tools = Tools{Git: git, Kind: kind, Read: read, Same: same, Each: each}

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
		calls = append(calls, strings.Join(bare(args), " "))
		if bare(args)[0] == "for-each-ref" {
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

// A copy whose .git is a hard link to a moved worktree's is refused: the
// backlink names .git in the moved folder, which is not the copy, though
// both name one file (Codex review round 25).
func TestAHardLinkedCopyIsRefused(t *testing.T) {
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
	if err := os.Link(filepath.Join(moved, ".git"), filepath.Join(path, ".git")); err != nil {
		t.Skipf("no hard link here: %v", err)
	}
	if err := Usable(ctx, tools, checkout, path, "vera/fix-t1"); err == nil || !strings.Contains(err.Error(), "not the worktree git registered") {
		t.Fatalf("a copy hard-linked to another worktree's .git: %v", err)
	}
}

// pullRef is the ref a cross-review fetches its head as (ADR-0019).
const pullRef = "refs/pull/81/head"

// revParse is what name names in dir, as git prints it.
func revParse(t *testing.T, dir, name string) string {
	t.Helper()
	out, err := git(context.Background(), dir, "rev-parse", "--verify", "-q", name)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// origin is a bare copy of checkout, as a test's GitHub: a file path to
// fetch from, with no network.
func origin(t *testing.T, checkout string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin.git")
	cmd := exec.Command("git", "clone", "-q", "--bare", checkout, dir)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	return dir
}

// commitOn makes a commit in dir on top of parent, with parent's tree, and
// returns its id: a push the checkout has not fetched.
func commitOn(t *testing.T, dir, parent, msg string) string {
	t.Helper()
	out, err := git(context.Background(), dir, "-c", "user.email=t@example.com", "-c", "user.name=t",
		"commit-tree", "-p", parent, "-m", msg, parent+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// pull points dir's pull request ref at id, as GitHub keeps a head.
func pull(t *testing.T, dir, id string) {
	t.Helper()
	if _, err := git(context.Background(), dir, "update-ref", pullRef, id); err != nil {
		t.Fatal(err)
	}
}

// logged are tools that keep each git call's arguments in calls.
func logged(calls *[]string) Tools {
	tl := tools
	tl.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		*calls = append(*calls, strings.Join(bare(args), " "))
		return git(ctx, dir, args...)
	}
	return tl
}

// ran says whether a call in calls began with prefix.
func ran(calls []string, prefix string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

// absent fails t unless nothing is at path.
func absent(t *testing.T, path string) {
	t.Helper()
	if k, err := kind(context.Background(), path); err != nil || k != "absent" {
		t.Fatalf("the worktree's path is %s, %v", k, err)
	}
}

// A commit the checkout holds is checked out detached with no fetch, and
// the worktree is found again, as made.
func TestAddDetachedMakesAWorktreeAtACommitTheCheckoutHolds(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	var calls []string
	// A fetch from nowhere would fail: none runs.
	if err := AddDetached(ctx, logged(&calls), checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if ran(calls, "fetch") {
		t.Errorf("a fetch ran for a commit the checkout holds: %q", calls)
	}
	if name, err := git(ctx, path, "rev-parse", "--symbolic-full-name", "HEAD"); err != nil || name != "HEAD" {
		t.Errorf("the worktree's HEAD is %q, %v, not detached", name, err)
	}
	if head := revParse(t, path, "HEAD"); head != sha {
		t.Errorf("the worktree is at %s, not %s", head, sha)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err != nil {
		t.Errorf("the worktree as made: %v", err)
	}
}

// A commit the checkout lacks is fetched from the pull request's ref, and
// the worktree made at it.
func TestAddDetachedFetchesACommitTheCheckoutLacks(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	remote := origin(t, checkout)
	sha := commitOn(t, remote, "HEAD", "the pull request")
	pull(t, remote, sha)
	if there, err := hasCommit(ctx, git, checkout, sha); err != nil || there {
		t.Fatalf("the checkout holds the head already: %v, %v", there, err)
	}
	path := checkout + "--review-81"
	var calls []string
	if err := AddDetached(ctx, logged(&calls), checkout, path, sha, remote, pullRef); err != nil {
		t.Fatal(err)
	}
	if want := "fetch --no-tags --write-fetch-head -- " + remote + " " + pullRef; !ran(calls, want) {
		t.Errorf("no git %s in %q", want, calls)
	}
	if head := revParse(t, path, "HEAD"); head != sha {
		t.Errorf("the worktree is at %s, not %s", head, sha)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err != nil {
		t.Errorf("the worktree as made: %v", err)
	}
}

// A pull request ref that names another commit than the head pocketd read
// is a head that moved: refused, though the fetch brought the old head as
// its parent, and no worktree is made.
func TestAddDetachedRefusesAHeadThatMoved(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	remote := origin(t, checkout)
	read := commitOn(t, remote, "HEAD", "the head pocketd read")
	moved := commitOn(t, remote, read, "a push since")
	pull(t, remote, moved)
	path := checkout + "--review-81"
	var calls []string
	err := AddDetached(ctx, logged(&calls), checkout, path, read, remote, pullRef)
	if err == nil || !strings.Contains(err.Error(), "head moved") || !strings.Contains(err.Error(), moved) || Unsure(err) {
		t.Fatalf("a head that moved: %v", err)
	}
	if ran(calls, "worktree add") {
		t.Errorf("a worktree was made at a head that moved: %q", calls)
	}
	absent(t, path)
}

// A pull request ref that names sha, where sha is no commit (a tree), is
// refused after the fetch, before git worktree add.
func TestAddDetachedRefusesARefThatNamesNoCommit(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	remote := origin(t, checkout)
	tree := revParse(t, remote, "HEAD^{tree}")
	pull(t, remote, tree)
	path := checkout + "--review-81"
	var calls []string
	err := AddDetached(ctx, logged(&calls), checkout, path, tree, remote, pullRef)
	if err == nil || !strings.Contains(err.Error(), "which is not a commit") || Unsure(err) {
		t.Fatalf("a ref that names a tree: %v", err)
	}
	if !ran(calls, "fetch") || ran(calls, "worktree add") {
		t.Errorf("calls: %q", calls)
	}
	absent(t, path)
}

// Only a commit's full id, a pull request's head ref and a URL that reads
// as no option are taken: nothing runs for any other.
func TestAddDetachedRefusesWhatIsNotAFullCommitID(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	for _, bad := range []string{"", sha[:7], strings.ToUpper(sha), sha + "0", sha[:39] + "g", "HEAD", "main", " " + sha[1:]} {
		var calls []string
		if err := AddDetached(ctx, logged(&calls), checkout, path, bad, "/nowhere", pullRef); err == nil || !strings.Contains(err.Error(), "40 lowercase hex digits") {
			t.Errorf("AddDetached at %q: %v", bad, err)
		}
		if len(calls) != 0 {
			t.Errorf("git ran for %q: %q", bad, calls)
		}
		absent(t, path)
	}
	for _, bad := range []string{"", "refs/heads/main", "refs/pull/81/merge", "refs/pull/0/head", "refs/pull/x/head", "pull/81/head", "--upload-pack=x", pullRef + "\n"} {
		var calls []string
		if err := AddDetached(ctx, logged(&calls), checkout, path, sha, "/nowhere", bad); err == nil || !strings.Contains(err.Error(), "head ref") {
			t.Errorf("AddDetached of the ref %q: %v", bad, err)
		}
		if len(calls) != 0 {
			t.Errorf("git ran for the ref %q: %q", bad, calls)
		}
		absent(t, path)
	}
	for _, bad := range []string{"", "--upload-pack=touch x"} {
		var calls []string
		if err := AddDetached(ctx, logged(&calls), checkout, path, sha, bad, pullRef); err == nil || !strings.Contains(err.Error(), "URL") {
			t.Errorf("AddDetached from %q: %v", bad, err)
		}
		if len(calls) != 0 {
			t.Errorf("git ran for the URL %q: %q", bad, calls)
		}
		absent(t, path)
	}
	if _, err := git(ctx, checkout, "worktree", "add", "--detach", path, sha); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{sha[:7], strings.ToUpper(sha)} {
		if err := UsableDetached(ctx, tools, checkout, path, bad); err == nil || !strings.Contains(err.Error(), "40 lowercase hex digits") {
			t.Errorf("UsableDetached at %q: %v", bad, err)
		}
	}
	if err := AddDetached(ctx, tools, "relative", path, sha, "/nowhere", pullRef); err == nil {
		t.Error("a relative checkout was taken")
	}
}

// A worktree an earlier try made, detached at sha, is finished from:
// nothing is fetched or made again.
func TestAddDetachedFinishesFromAnEarlierTry(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	remote := origin(t, checkout)
	sha := commitOn(t, remote, "HEAD", "the pull request")
	pull(t, remote, sha)
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, remote, pullRef); err != nil {
		t.Fatal(err)
	}
	// The URL is gone: nothing is looked up or fetched again.
	var calls []string
	if err := AddDetached(ctx, logged(&calls), checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatalf("a retry: %v", err)
	}
	if ran(calls, "fetch") || ran(calls, "worktree add") || ran(calls, "cat-file") {
		t.Errorf("a retry made or fetched again: %q", calls)
	}
	// One git made itself, as a try stopped before its check leaves it.
	other := checkout + "--review-82"
	if _, err := git(ctx, checkout, "worktree", "add", "--detach", other, sha); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if err := AddDetached(ctx, logged(&calls), checkout, other, sha, "/nowhere", pullRef); err != nil {
		t.Fatalf("a worktree a stopped try made: %v", err)
	}
	if ran(calls, "fetch") || ran(calls, "worktree add") {
		t.Errorf("a retry made or fetched again: %q", calls)
	}
}

// A path that holds a worktree at another commit, one on a branch at sha,
// or a plain folder, is refused: nothing is made over it.
func TestAddDetachedRefusesAWorktreeItCannotUse(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	first := revParse(t, checkout, "HEAD")
	if _, err := git(ctx, checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	sha := revParse(t, checkout, "HEAD")
	elsewhere := checkout + "--review-81"
	if _, err := git(ctx, checkout, "worktree", "add", "--detach", elsewhere, first); err != nil {
		t.Fatal(err)
	}
	onBranch := checkout + "--vera-fix-t1"
	if err := Add(ctx, tools, checkout, onBranch, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	if head := revParse(t, onBranch, "HEAD"); head != sha {
		t.Fatalf("the branch is at %s, not %s", head, sha)
	}
	taken := checkout + "--review-83"
	if err := os.Mkdir(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"another commit": elsewhere, "a branch": onBranch, "a plain folder": taken} {
		var calls []string
		err := AddDetached(ctx, logged(&calls), checkout, path, sha, "/nowhere", pullRef)
		if err == nil || !strings.Contains(err.Error(), "is not a worktree of "+checkout+" detached at "+sha) || Unsure(err) {
			t.Errorf("%s: %v", name, err)
		}
		if ran(calls, "worktree add") || ran(calls, "fetch") {
			t.Errorf("%s: made over it: %q", name, calls)
		}
		if err := UsableDetached(ctx, tools, checkout, path, sha); err == nil {
			t.Errorf("%s: UsableDetached took it", name)
		}
	}
	if head := revParse(t, elsewhere, "HEAD"); head != first {
		t.Errorf("the worktree at another commit moved to %s", head)
	}
}

// UsableDetached checks the worktree again, making nothing: one moved to
// another commit since is refused.
func TestUsableDetachedChecksTheWorktreeAgain(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err != nil {
		t.Fatalf("the worktree as made: %v", err)
	}
	if _, err := git(ctx, checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, path, "checkout", "-q", "--detach", "main"); err != nil {
		t.Fatal(err)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err == nil || !strings.Contains(err.Error(), "detached at "+sha) {
		t.Fatalf("a worktree moved to another commit: %v", err)
	}
}

// A detached worktree a stopped try left half made (git's lock still on
// it) is refused, as Add refuses one.
func TestAHalfMadeDetachedWorktreeIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	dir, err := git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "locked")
	if err := os.WriteFile(lock, []byte("initializing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err == nil || !strings.Contains(err.Error(), "is locked (see "+lock+")") {
		t.Errorf("AddDetached of a half-made worktree: %v", err)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err == nil || !strings.Contains(err.Error(), "is locked (see "+lock+")") {
		t.Errorf("UsableDetached of a half-made worktree: %v", err)
	}
}

// A review's worktree runs no hook of the checkout: a post-checkout hook
// that would put it on a branch, or change its files, never runs (Codex
// review of #26, round 2).
func TestANewDetachedWorktreeRunsNoHook(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	sha := revParse(t, checkout, "HEAD")
	hook := "#!/bin/sh\ngit checkout -q -b elsewhere\necho changed > hooked\n"
	if err := os.WriteFile(filepath.Join(checkout, ".git", "hooks", "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatalf("AddDetached: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "hooked")); !os.IsNotExist(err) {
		t.Fatalf("the hook ran: %v", err)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err != nil {
		t.Fatalf("UsableDetached: %v", err)
	}
}

// A review's worktree with a change, or a file git does not track, is no
// review of its commit: refused (Codex review of #26, round 2).
func TestADirtyDetachedWorktreeIsRefused(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err == nil || !strings.Contains(err.Error(), "has changes of its own") {
		t.Fatalf("a dirty worktree: %v", err)
	}
}

// bare is args without the review's git options (ReviewGitOptions).
func bare(args []string) []string {
	if len(args) >= len(ReviewGitOptions) && slices.Equal(args[:len(ReviewGitOptions)], ReviewGitOptions) {
		return args[len(ReviewGitOptions):]
	}
	return args
}

// A commit lookup that could not tell fetches nothing and makes nothing,
// and a fetch that failed may pass when tried again: both are Unsure.
func TestACommitLookupOrFetchThatFailsMakesNothing(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	remote := origin(t, checkout)
	sha := commitOn(t, remote, "HEAD", "the pull request")
	pull(t, remote, sha)
	path := checkout + "--review-81"
	broken := errors.New("bwrap: cannot set up the sandbox")
	var calls []string
	tl := logged(&calls)
	inner := tl.Git
	tl.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if bare(args)[0] == "cat-file" {
			calls = append(calls, strings.Join(bare(args), " "))
			return "", broken
		}
		return inner(ctx, dir, args...)
	}
	if err := AddDetached(ctx, tl, checkout, path, sha, remote, pullRef); err == nil || !errors.Is(err, broken) || !Unsure(err) {
		t.Fatalf("a lookup that failed: %v", err)
	}
	if ran(calls, "fetch") || ran(calls, "worktree add") {
		t.Errorf("a lookup that failed went on: %q", calls)
	}
	absent(t, path)
	calls = nil
	err := AddDetached(ctx, logged(&calls), checkout, path, sha, filepath.Join(t.TempDir(), "gone.git"), pullRef)
	if err == nil || !strings.Contains(err.Error(), "could not fetch") || !Unsure(err) {
		t.Fatalf("a fetch that failed: %v", err)
	}
	if ran(calls, "worktree add") {
		t.Errorf("a fetch that failed went on: %q", calls)
	}
	absent(t, path)
}

// A FETCH_HEAD another fetch in the checkout wrote after this one does not
// tell what the pull request's ref named: Unsure, never a head that moved,
// and nothing is made.
func TestAFetchHeadAnotherFetchWroteIsUnsure(t *testing.T) {
	ctx := context.Background()
	for name, refs := range map[string][]string{
		"another ref":         {"main"},
		"the ref and another": {pullRef, "main"},
	} {
		// A checkout of its own: the fetch brings the commit in.
		checkout := repo(t)
		remote := origin(t, checkout)
		sha := commitOn(t, remote, "HEAD", "the pull request")
		pull(t, remote, sha)
		path := checkout + "--review-81"
		var calls []string
		tl := logged(&calls)
		inner := tl.Git
		tl.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
			out, err := inner(ctx, dir, args...)
			if err == nil && bare(args)[0] == "fetch" {
				if _, err := git(ctx, dir, append([]string{"fetch", "--no-tags", "--", remote}, refs...)...); err != nil {
					t.Fatal(err)
				}
			}
			return out, err
		}
		err := AddDetached(ctx, tl, checkout, path, sha, remote, pullRef)
		if err == nil || !Unsure(err) || !strings.Contains(err.Error(), "another fetch") || strings.Contains(err.Error(), "head moved") {
			t.Errorf("%s: %v", name, err)
		}
		if ran(calls, "worktree add") {
			t.Errorf("%s: made: %q", name, calls)
		}
		absent(t, path)
	}
}

// A replacement object of the checkout does not change what a review's
// worktree holds: it is GitHub's commit, whatever git replace says (Codex
// review of #26, round 2).
func TestADetachedWorktreeIgnoresReplaceRefs(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	real := revParse(t, checkout, "HEAD")
	for _, args := range [][]string{
		{"-C", checkout, "checkout", "-q", "-b", "fake"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(checkout, "planted"), []byte("not on GitHub"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", checkout, "add", "planted"},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "planted"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	fake := revParse(t, checkout, "HEAD")
	if out, err := exec.Command("git", "-C", checkout, "replace", real, fake).CombinedOutput(); err != nil {
		t.Fatalf("git replace: %v: %s", err, out)
	}
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, real, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "planted")); !os.IsNotExist(err) {
		t.Fatalf("the review's worktree holds the replacement's file: %v", err)
	}
}

// A file git ignores counts as a change of a review's worktree: the
// reviewer's read would show it (Codex review of #26, round 3).
func TestAnIgnoredFileMakesAReviewWorktreeDirty(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	if err := os.WriteFile(filepath.Join(checkout, ".gitignore"), []byte("*.gen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", checkout, "add", ".gitignore"},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "ignore"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "local.gen"), []byte("not the commit's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err == nil || !strings.Contains(err.Error(), "has changes of its own") {
		t.Fatalf("an ignored file: %v", err)
	}
}

// An index entry that hides a change from status, as assume-unchanged,
// makes a review's worktree unusable (Codex review of #26, round 4).
func TestAHiddenIndexEntryMakesAReviewWorktreeUnusable(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	if err := os.WriteFile(filepath.Join(checkout, "file"), []byte("committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", checkout, "add", "file"},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "file"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", path, "update-index", "--assume-unchanged", "file").CombinedOutput(); err != nil {
		t.Fatalf("update-index: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(path, "file"), []byte("not the commit's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UsableDetached(ctx, tools, checkout, path, sha); err == nil || !strings.Contains(err.Error(), "so status cannot see it") {
		t.Fatalf("an assume-unchanged entry: %v", err)
	}
}

// No filter of the checkout runs when a review's worktree is made, and its
// files are the commit's blobs (Codex review of #26, round 6).
func TestADetachedWorktreeRunsNoFilter(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	marker := filepath.Join(t.TempDir(), "filtered")
	if err := os.WriteFile(filepath.Join(checkout, ".gitattributes"), []byte("*.txt filter=boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("the blob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", checkout, "add", ".gitattributes", "a.txt"},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "filtered"},
		{"-C", checkout, "config", "filter.boom.smudge", "touch " + marker + "; echo SMUDGED"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	sha := revParse(t, checkout, "HEAD")
	path := checkout + "--review-81"
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(path, "a.txt")); string(got) != "the blob\n" {
		t.Fatalf("a.txt is %q, not the commit's blob", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the filter ran: %v", err)
	}
}

// A diff of a named path shows that file alone, though its name would
// match its siblings as a pattern (Codex review of #26, round 6).
func TestADiffOfANamedPathIsLiteral(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	from := revParse(t, checkout, "HEAD")
	if err := os.MkdirAll(filepath.Join(checkout, "routes"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"[id].ts", "i.ts", "d.ts"} {
		if err := os.WriteFile(filepath.Join(checkout, "routes", name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git(ctx, checkout, "add", "routes"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "routes"); err != nil {
		t.Fatal(err)
	}
	to := revParse(t, checkout, "HEAD")
	for _, log := range []bool{false, true} {
		out, err := git(ctx, checkout, team.DiffArgs(from, to, team.DiffRequest{Paths: []string{"routes/[id].ts"}, Stat: true, Log: log})...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "routes/[id].ts") || strings.Contains(out, "routes/i.ts") || strings.Contains(out, "routes/d.ts") {
			t.Fatalf("log %v: the diff of routes/[id].ts:\n%s", log, out)
		}
	}
}

// filtered is a checkout whose a.txt the filter boom would change, and
// boom's marker: the file boom touches when it runs. The checkout's
// .gitattributes selects nothing; the caller chooses where boom is set.
func filtered(t *testing.T) (checkout, marker, sha string) {
	t.Helper()
	ctx := context.Background()
	checkout = repo(t)
	marker = filepath.Join(t.TempDir(), "filtered")
	if err := os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("the blob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "a.txt"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "filtered"},
		{"config", "filter.boom.smudge", "touch " + marker + "; echo SMUDGED"},
	} {
		if _, err := git(ctx, checkout, args...); err != nil {
			t.Fatal(err)
		}
	}
	return checkout, marker, revParse(t, checkout, "HEAD")
}

// Attributes no option turns off stop a review's worktree before git
// writes a file, and the file core.attributesFile names is not read
// (Codex review of #26, round 7).
func TestADetachedWorktreeReadsNoAttributesButTheEmptyTrees(t *testing.T) {
	ctx := context.Background()
	t.Run("info/attributes", func(t *testing.T) {
		checkout, marker, sha := filtered(t)
		info := filepath.Join(checkout, ".git", "info", "attributes")
		if err := os.MkdirAll(filepath.Dir(info), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(info, []byte("*.txt filter=boom\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := checkout + "--review-81"
		err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef)
		if err == nil || !strings.Contains(err.Error(), info) {
			t.Fatalf("a checkout with info/attributes: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the worktree was made: %v", err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("the filter ran: %v", err)
		}
	})
	t.Run("core.attributesFile", func(t *testing.T) {
		checkout, marker, sha := filtered(t)
		file := filepath.Join(t.TempDir(), "attributes")
		if err := os.WriteFile(file, []byte("*.txt filter=boom\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := git(ctx, checkout, "config", "core.attributesFile", file); err != nil {
			t.Fatal(err)
		}
		path := checkout + "--review-81"
		if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(path, "a.txt")); string(got) != "the blob\n" {
			t.Fatalf("a.txt is %q, not the commit's blob", got)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("the filter ran: %v", err)
		}
	})
}

// git older than 2.43 ignores attr.tree: no review's worktree is made
// with it, nor checked usable (Codex review of #26, round 7).
func TestADetachedWorktreeNeedsGit243(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	sha := revParse(t, checkout, "HEAD")
	old := tools
	old.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if slices.Contains(args, "version") {
			return "git version 2.39.5\n", nil
		}
		return git(ctx, dir, args...)
	}
	path := checkout + "--review-81"
	if err := AddDetached(ctx, old, checkout, path, sha, "/nowhere", pullRef); err == nil || !strings.Contains(err.Error(), "2.43") {
		t.Fatalf("git 2.39: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the worktree was made: %v", err)
	}
	if err := AddDetached(ctx, tools, checkout, path, sha, "/nowhere", pullRef); err != nil {
		t.Fatal(err)
	}
	if err := UsableDetached(ctx, old, checkout, path, sha); err == nil || !strings.Contains(err.Error(), "2.43") {
		t.Fatalf("git 2.39 checked it usable: %v", err)
	}
	for version, want := range map[string]bool{"git version 2.43.0": true, "git version 2.47.3\n": true, "git version 3.0.0": true,
		"git version 2.42.9": false, "git version 1.99.0": false, "version 2.47": false, "": false} {
		if got := gitAtLeast(version, 2, 43); got != want {
			t.Errorf("gitAtLeast(%q) = %v", version, got)
		}
	}
}
