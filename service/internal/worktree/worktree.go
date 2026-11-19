// Package worktree makes the git worktree a thread works in (ADR-0014):
// next to the repository's checkout, on a new branch from its HEAD.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Git runs git with args in dir and returns its output, trimmed. pocketd
// runs it in the sandbox (ADR-0012), so a hook or a filter the checkout
// configures runs with an agent's rights, never with pocketd's.
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// Add makes a worktree of checkout at path on the new branch branch, from
// the checkout's HEAD, for owner (the thread's key). It marks the branch
// with owner in the checkout's git config (branch.<branch>.pocketgo-thread)
// before it makes anything, so a retry finds what a first try made: a path
// that is a worktree of the same repository on that branch, or the branch
// alone. A path or a branch of that name that owner did not make, as one
// left by an earlier data folder, is refused, never reused. checkout must
// be the top of its repository.
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
	mark := "branch." + branch + ".pocketgo-thread"
	marked, _ := git(ctx, checkout, "config", "--get", mark)
	_, pathErr := os.Lstat(path)
	_, branchErr := git(ctx, checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	pathExists, branchExists := pathErr == nil, branchErr == nil
	switch {
	case marked != owner && (pathExists || branchExists):
		return fmt.Errorf("%s or the branch %s exists already, and this thread did not make it", path, branch)
	case marked != owner:
		if _, err := git(ctx, checkout, "config", mark, owner); err != nil {
			return err
		}
	case pathExists:
		if !isWorktree(ctx, git, path, common, branch) {
			return fmt.Errorf("%s exists and is not a worktree of %s on the branch %s", path, checkout, branch)
		}
		return nil
	case branchExists:
		_, err := git(ctx, checkout, "worktree", "add", path, branch)
		return err
	}
	_, err = git(ctx, checkout, "worktree", "add", "-b", branch, path, "HEAD")
	return err
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
	head, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	return err == nil && head == branch
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
