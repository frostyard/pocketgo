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
// the checkout's HEAD. A retry finds the worktree a first try made: a path
// that is a worktree of the same repository on that branch, or a branch
// that exists already, is used as it is. A path taken by anything else is
// refused.
func Add(ctx context.Context, git Git, checkout, path, branch string) error {
	if !filepath.IsAbs(checkout) || !filepath.IsAbs(path) {
		return errors.New("the checkout and the worktree must be absolute paths")
	}
	common, err := commonDir(ctx, git, checkout)
	if err != nil {
		return fmt.Errorf("%s is not a git checkout: %w", checkout, err)
	}
	if _, err := os.Lstat(path); err == nil {
		if !isWorktree(ctx, git, path, common, branch) {
			return fmt.Errorf("%s exists and is not a worktree of %s on the branch %s", path, checkout, branch)
		}
		return nil
	}
	if _, err := git(ctx, checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
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
