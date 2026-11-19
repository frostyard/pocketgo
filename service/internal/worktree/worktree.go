// Package worktree makes the git worktree a thread works in (ADR-0014):
// next to the repository's checkout, on a new branch from its HEAD.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Git runs git with args in dir and returns its output, trimmed. pocketd
// runs it in the sandbox (ADR-0012), so a hook or a filter the checkout
// configures runs with an agent's rights, never with pocketd's.
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// Add makes a worktree of checkout at path on the new branch branch, from
// the checkout's HEAD, for owner (the thread's random identity). Before it
// makes anything it claims the branch's name for owner, atomically
// (claim), so a retry finds what a first try made: a path that is a
// finished worktree of the same repository on that branch, or the branch
// alone. A name another owner claimed, or a path or a branch of that name
// that no claim covers, is refused, never reused. A worktree a stopped try
// left half made is refused too. checkout must be the top of its
// repository.
func Add(ctx context.Context, git Git, checkout, path, branch, owner string) error {
	if !filepath.IsAbs(checkout) || !filepath.IsAbs(path) {
		return errors.New("the checkout and the worktree must be absolute paths")
	}
	if owner == "" {
		return errors.New("a worktree needs its owner")
	}
	top, err := git(ctx, checkout, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%s is not a git checkout: %w", checkout, err)
	}
	if !samePath(top, checkout) {
		return fmt.Errorf("%s is a folder inside the checkout %s, not a checkout", checkout, top)
	}
	common, err := commonDir(ctx, git, checkout)
	if err != nil {
		return fmt.Errorf("%s is not a git checkout: %w", checkout, err)
	}
	_, pathErr := os.Lstat(path)
	_, branchErr := git(ctx, checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	pathExists, branchExists := pathErr == nil, branchErr == nil
	holder := claimHolder(ctx, git, checkout, branch)
	switch {
	case holder != "" && holder != owner, holder == "" && (pathExists || branchExists):
		return fmt.Errorf("%s or the branch %s exists already, and this thread did not make it", path, branch)
	case holder == "":
		if err := claim(ctx, git, checkout, branch, owner); err != nil {
			return err
		}
	case pathExists:
		return finished(ctx, git, checkout, path, common, branch)
	case branchExists:
		if _, err := git(ctx, checkout, "worktree", "add", path, branch); err != nil {
			return err
		}
		return finished(ctx, git, checkout, path, common, branch)
	}
	if _, err := git(ctx, checkout, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return err
	}
	return finished(ctx, git, checkout, path, common, branch)
}

// Usable checks the worktree at path of checkout as a specialist gets it
// now (finished), without making anything: pocketd checks it again when
// the specialist's session starts.
func Usable(ctx context.Context, git Git, checkout, path, branch string) error {
	common, err := commonDir(ctx, git, checkout)
	if err != nil {
		return fmt.Errorf("%s is not a git checkout: %w", checkout, err)
	}
	return finished(ctx, git, checkout, path, common, branch)
}

// finished checks the worktree at path as a specialist gets it: a worktree
// of the repository, on branch, and not locked. A new one is checked too:
// the checkout's post-checkout hook runs in git worktree add, and may
// switch its branch or lock it.
func finished(ctx context.Context, git Git, checkout, path, common, branch string) error {
	if !isWorktree(ctx, git, path, common, branch) {
		return fmt.Errorf("%s exists and is not a worktree of %s on the branch %s", path, checkout, branch)
	}
	if why, locked := lockOf(ctx, git, path); locked {
		return fmt.Errorf("the worktree %s is locked (%s), as git leaves one a stopped try did not finish: look at it before you unlock or remove it, then delegate again", path, why)
	}
	return nil
}

// claimRef is the ref that claims branch's name for a thread: a commit on
// the empty tree whose message is the owner. It is outside refs/heads, so
// it is never a branch and is not pushed.
func claimRef(branch string) string { return "refs/pocketgo/threads/" + branch }

// claim claims branch's name for owner: git creates claimRef only where it
// does not exist (update-ref with an empty old value), so of two tries,
// across processes, one wins. A claim that loses to another owner fails.
func claim(ctx context.Context, git Git, checkout, branch, owner string) error {
	tree, err := git(ctx, checkout, "hash-object", "-t", "tree", "-w", "/dev/null")
	if err != nil {
		return err
	}
	commit, err := git(ctx, checkout, "-c", "user.name=pocketgo", "-c", "user.email=pocketgo@localhost", "commit-tree", tree, "-m", owner)
	if err != nil {
		return err
	}
	if _, err := git(ctx, checkout, "update-ref", claimRef(branch), commit, ""); err != nil {
		switch holder := claimHolder(ctx, git, checkout, branch); holder {
		case owner:
			return nil
		case "":
			return fmt.Errorf("claim the branch %s: %w", branch, err) // git's own failure, as a lock or a hook
		}
		return fmt.Errorf("the branch %s is claimed by another thread", branch)
	}
	return nil
}

// claimHolder is the owner that claimed branch's name, "" for none.
func claimHolder(ctx context.Context, git Git, checkout, branch string) string {
	holder, err := git(ctx, checkout, "show", "-s", "--format=%B", claimRef(branch))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(holder) // an owner is hex: its message's blank lines go
}

// lockOf says whether the worktree at path is locked: git worktree add
// locks a worktree until it has checked out every file, so a stopped try
// leaves it locked, and so does an owner who locked a finished one on
// purpose. pocketd uses neither, and never removes either: the owner
// looks. git, in the sandbox, names the worktree's own lock file (its
// git path "locked"), so no list of every worktree is read; pocketd only
// looks whether that name exists, without reading or following it. When
// git cannot tell, the worktree counts as locked.
func lockOf(ctx context.Context, git Git, path string) (string, bool) {
	lock, err := git(ctx, path, "rev-parse", "--path-format=absolute", "--git-path", "locked")
	if err != nil || !filepath.IsAbs(lock) {
		return "git cannot name its lock", true
	}
	if _, err := os.Lstat(lock); errors.Is(err, os.ErrNotExist) {
		return "", false
	} else if err != nil {
		return "its lock cannot be looked at", true
	}
	return "see " + lock, true
}

// isWorktree says whether path is the top of a worktree of the repository
// whose common git folder is common, on branch.
func isWorktree(ctx context.Context, git Git, path, common, branch string) bool {
	top, err := git(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(top, path) {
		return false
	}
	other, err := commonDir(ctx, git, path)
	if err != nil || !samePath(other, common) {
		return false
	}
	// The full ref: an abbreviation may carry heads/ where a tag has the
	// branch's name, and a detached HEAD has none.
	head, err := git(ctx, path, "symbolic-ref", "-q", "HEAD")
	return err == nil && head == "refs/heads/"+branch
}

// commonDir is the git folder the checkout's worktrees share, absolute.
func commonDir(ctx context.Context, git Git, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return out, nil
}

// samePath compares two paths after resolving their links.
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
