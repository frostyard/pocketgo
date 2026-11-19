// Package worktree makes the git worktree a thread works in (ADR-0014):
// next to the repository's checkout, on a new branch from its HEAD.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Git runs git with args in dir and returns its output, trimmed. pocketd
// runs it in the sandbox (ADR-0012), so a hook or a filter the checkout
// configures runs with an agent's rights, never with pocketd's.
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// Exists says whether path names an entry, a dangling link included, as a
// command in the sandbox sees it: pocketd looks at no checkout path with
// its own rights.
type Exists func(ctx context.Context, path string) (bool, error)

// Read is the first 4 KiB of the regular file at path as a command in the
// sandbox reads it; ok is false when path is no regular file there.
type Read func(ctx context.Context, path string) (text string, ok bool, err error)

// Tools are what Add and Usable run: git, an existence check and a short
// read, all in the sandbox.
type Tools struct {
	Git    Git
	Exists Exists
	Read   Read
}

// Add makes a worktree of checkout at path on branch, from the checkout's
// HEAD. The path's and the branch's names end with the thread's random
// owner (team.Slug), so nothing else makes them: a path or a branch of the
// name is what an earlier try of the same thread made, and a retry finishes
// from it. Every worktree it hands over is finished: of the repository, on
// branch with a commit, registered by git at path, and not locked.
// checkout must be the top of its repository.
func Add(ctx context.Context, t Tools, checkout, path, branch string) error {
	git := t.Git
	if !filepath.IsAbs(checkout) || !filepath.IsAbs(path) {
		return errors.New("the checkout and the worktree must be absolute paths")
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
	exists, err := t.Exists(ctx, path)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := git(ctx, checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			_, err = git(ctx, checkout, "worktree", "add", path, branch)
			if err != nil {
				return err
			}
		} else if _, err := git(ctx, checkout, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
			return err
		}
	}
	return finished(ctx, t, checkout, path, common, branch)
}

// Usable checks the worktree at path of checkout as a specialist gets it
// now (finished), without making anything: pocketd checks it again when
// the specialist's session starts.
func Usable(ctx context.Context, t Tools, checkout, path, branch string) error {
	common, err := commonDir(ctx, t.Git, checkout)
	if err != nil {
		return fmt.Errorf("%s is not a git checkout: %w", checkout, err)
	}
	return finished(ctx, t, checkout, path, common, branch)
}

// finished checks the worktree at path as a specialist gets it: a worktree
// of the repository, on branch, and not locked. A new one is checked too:
// the checkout's post-checkout hook runs in git worktree add, and may
// switch its branch or lock it.
func finished(ctx context.Context, t Tools, checkout, path, common, branch string) error {
	if !isWorktree(ctx, t.Git, path, common, branch) {
		return fmt.Errorf("%s exists and is not a worktree of %s on the branch %s", path, checkout, branch)
	}
	// git registered this folder as the worktree, not a copy of one made
	// elsewhere: the worktree's git folder names it back (its gitdir file).
	if !registered(ctx, t, path) {
		return fmt.Errorf("%s is not the worktree git registered: a copy, or a moved one", path)
	}
	if why, locked := lockOf(ctx, t, path); locked {
		return fmt.Errorf("the worktree %s is locked (%s), as git leaves one a stopped try did not finish: look at it before you unlock or remove it, then delegate again", path, why)
	}
	return nil
}

// lockOf says whether the worktree at path is locked: git worktree add
// locks a worktree until it has checked out every file, so a stopped try
// leaves it locked, and so does an owner who locked a finished one on
// purpose. pocketd uses neither, and never removes either: the owner
// looks. git names the worktree's own git folder, which holds its lock
// file "locked", so no list of every worktree is read, and a command in the sandbox says
// whether that name exists, a dangling link included, without reading it.
// When they cannot tell, the worktree counts as locked.
func lockOf(ctx context.Context, t Tools, path string) (string, bool) {
	// The worktree's git folder, and its lock's name joined to it: git's
	// own --git-path resolves a link at that name, which would look at the
	// link's target, not at the lock.
	dir, err := t.Git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil || !filepath.IsAbs(dir) {
		return "git cannot name its lock", true
	}
	lock := filepath.Join(dir, "locked")
	exists, err := t.Exists(ctx, lock)
	if err != nil {
		return "its lock cannot be looked at", true
	}
	if !exists {
		return "", false
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
	if err != nil || head != "refs/heads/"+branch {
		return false
	}
	// A branch with no commit (its ref gone) would start a history of its
	// own: the worktree's HEAD must name a commit.
	_, err = git(ctx, path, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	return err == nil
}

// registered says whether git registered path as its worktree: the
// worktree's git folder, as git names it from path, holds a gitdir file
// that names path's .git back, read in the sandbox.
func registered(ctx context.Context, t Tools, path string) bool {
	dir, err := t.Git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil || !filepath.IsAbs(dir) {
		return false
	}
	text, ok, err := t.Read(ctx, filepath.Join(dir, "gitdir"))
	if err != nil || !ok {
		return false
	}
	// git writes the backlink absolute, or relative to the git folder
	// (worktree.useRelativePaths): both name path's .git.
	back := strings.TrimRight(text, "\n")
	if !filepath.IsAbs(back) {
		back = filepath.Join(dir, back)
	}
	return samePath(back, filepath.Join(path, ".git"))
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
