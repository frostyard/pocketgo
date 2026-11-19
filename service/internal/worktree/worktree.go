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
// configures runs with an agent's rights, never with pocketd's. An error
// that wraps a *GitExit is git's own answer; any other says git did not
// run or was stopped (the sandbox around it failed, say).
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// GitExit is git's own exit with a status other than 0: git ran and
// answered. Only a Git that knows git itself ran returns one.
type GitExit struct{ Status int }

func (e *GitExit) Error() string { return fmt.Sprintf("git exited with status %d", e.Status) }

// unsure marks a check that could not tell: a probe that failed, git that
// did not run, a deadline that passed. The worktree may be fine.
type unsure struct{ error }

func (u unsure) Unwrap() error { return u.error }

// Unsure says whether err is a check that could not tell, as opposed to a
// worktree or a checkout that is not as it must be: the first may pass
// when it is tried again, the second does not.
func Unsure(err error) bool {
	var u unsure
	return errors.As(err, &u)
}

// Kind is what path names, as a command in the sandbox sees it, without
// following a link at path itself: "absent", "link", "file", "dir" or
// "other". pocketd looks at no checkout path with its own rights.
type Kind func(ctx context.Context, path string) (string, error)

// Same says whether two paths name one entry, as a command in the sandbox
// resolves them (the shell's test -ef), bound by ctx.
type Same func(ctx context.Context, a, b string) (bool, error)

// Read is the first 4 KiB of the regular file at path as a command in the
// sandbox reads it; ok is false when path is no regular file there.
type Read func(ctx context.Context, path string) (text string, ok bool, err error)

// Tools are what Add and Usable run: git, a look at an entry, a short
// read and a path comparison, all in the sandbox and bound by ctx.
type Tools struct {
	Git  Git
	Kind Kind
	Read Read
	Same Same
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
	common, err := checkoutCommon(ctx, t, checkout)
	if err != nil {
		return err
	}
	kind, err := t.Kind(ctx, path)
	if err != nil {
		return unsure{fmt.Errorf("look at %s: %w", path, err)}
	}
	if kind == "absent" {
		// A lookup that could not tell makes nothing: only a branch git
		// says is not there is made.
		exists, err := hasBranch(ctx, git, checkout, branch)
		if err != nil {
			return explain(ctx, checkout, err, "")
		}
		if exists {
			_, err = git(ctx, checkout, "worktree", "add", path, branch)
		} else {
			_, err = git(ctx, checkout, "worktree", "add", "-b", branch, path, "HEAD")
		}
		if err != nil {
			return explain(ctx, path, no(err), "git could not make the worktree "+path)
		}
	}
	return finished(ctx, t, checkout, path, common, branch)
}

// Usable checks the worktree at path of checkout as a specialist gets it
// now (finished), and the checkout as Add does, without making anything:
// pocketd checks it again when the specialist's session starts.
func Usable(ctx context.Context, t Tools, checkout, path, branch string) error {
	common, err := checkoutCommon(ctx, t, checkout)
	if err != nil {
		return err
	}
	return finished(ctx, t, checkout, path, common, branch)
}

// checkoutCommon checks that checkout is the top of its repository, as a
// command in the sandbox sees it, and returns the repository's common git
// folder: a folder inside a checkout shares its common folder, so that
// alone does not tell.
func checkoutCommon(ctx context.Context, t Tools, checkout string) (string, error) {
	top, err := t.Git(ctx, checkout, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", explain(ctx, checkout, no(err), checkout+" is not a git checkout")
	}
	if err := isSame(ctx, t, top, checkout); err != nil {
		return "", explain(ctx, checkout, err, fmt.Sprintf("%s is a folder inside the checkout %s, not a checkout", checkout, top))
	}
	common, err := commonDir(ctx, t.Git, checkout)
	if err != nil {
		return "", explain(ctx, checkout, no(err), checkout+" is not a git checkout")
	}
	return common, nil
}

// hasBranch says whether checkout's repository has branch. git lists the
// refs of the name, none for a missing branch, so any error is a lookup
// that could not tell, never a missing branch.
func hasBranch(ctx context.Context, git Git, checkout, branch string) (bool, error) {
	ref := "refs/heads/" + branch
	out, err := git(ctx, checkout, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return false, fmt.Errorf("look up the branch %s: %w", branch, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == ref {
			return true, nil
		}
	}
	return false, nil
}

// finished checks the worktree at path as a specialist gets it: a worktree
// of the repository, on branch, and not locked. A new one is checked too:
// the checkout's post-checkout hook runs in git worktree add, and may
// switch its branch or lock it. A check that could not run (a probe that
// failed, git that did not start, a timeout) says so with its cause, apart
// from a worktree that is not as it must be: that one may be fine.
func finished(ctx context.Context, t Tools, checkout, path, common, branch string) error {
	if err := isWorktree(ctx, t, path, common, branch); err != nil {
		return explain(ctx, path, err, fmt.Sprintf("%s exists and is not a worktree of %s on the branch %s", path, checkout, branch))
	}
	// git registered this folder as the worktree, not a copy of one made
	// elsewhere: the worktree's git folder names it back (its gitdir file).
	if err := registered(ctx, t, path, common); err != nil {
		return explain(ctx, path, err, fmt.Sprintf("%s is not the worktree git registered: a copy, or a moved one", path))
	}
	lock, err := lockOf(ctx, t, path)
	if err != nil {
		return explain(ctx, path, err, "")
	}
	if lock != "" {
		return fmt.Errorf("the worktree %s is locked (see %s), as git leaves one a stopped try did not finish: look at it before you unlock or remove it, then delegate again", path, lock)
	}
	return nil
}

// negative is a check's answer that the worktree is not as it must be,
// with git's error when git gave that answer. Any other error of a check
// says the check could not run.
type negative struct{ cause error }

func (n negative) Error() string {
	if n.cause == nil {
		return "no"
	}
	return n.cause.Error()
}

func (n negative) Unwrap() error { return n.cause }

// no is a negative answer: none (cause nil), or git's that ran and said
// no (a *GitExit). git that did not run, or was stopped, is a check that
// could not run.
func no(cause error) error {
	var exit *GitExit
	if cause != nil && !errors.As(cause, &exit) {
		return cause
	}
	return negative{cause}
}

// explain words a failed check of path: a negative answer as what, with
// git's word for it, and a check that could not run, or ran out of time,
// as one that could not tell.
func explain(ctx context.Context, path string, err error, what string) error {
	if cerr := ctx.Err(); cerr != nil {
		return unsure{fmt.Errorf("the check of %s did not finish: %w", path, cerr)}
	}
	var not negative
	if !errors.As(err, &not) {
		return unsure{fmt.Errorf("could not check %s: %w", path, err)}
	}
	if not.cause == nil {
		return errors.New(what)
	}
	return fmt.Errorf("%s: %w", what, not.cause)
}

// lockOf is the lock of the worktree at path, "" when it has none: git
// worktree add locks a worktree until it has checked out every file, so a
// stopped try leaves it locked, and so does an owner who locked a finished
// one on purpose. pocketd uses neither, and never removes either: the
// owner looks. git names the worktree's own git folder, which holds its
// lock file "locked", so no list of every worktree is read, and a command
// in the sandbox says whether that name exists, a dangling link included,
// without reading it. An error says they could not tell.
func lockOf(ctx context.Context, t Tools, path string) (string, error) {
	// The worktree's git folder, and its lock's name joined to it: git's
	// own --git-path resolves a link at that name, which would look at the
	// link's target, not at the lock.
	dir, err := t.Git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("git cannot name its lock: %w", err)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("git named its git folder %s, not an absolute path", dir)
	}
	lock := filepath.Join(dir, "locked")
	kind, err := t.Kind(ctx, lock)
	if err != nil {
		return "", fmt.Errorf("look at %s: %w", lock, err)
	}
	if kind == "absent" {
		return "", nil
	}
	return lock, nil
}

// isWorktree checks that path is the top of a worktree of the repository
// whose common git folder is common, on branch: nil when it is, a
// negative answer when it is not, and another error when it cannot tell.
func isWorktree(ctx context.Context, t Tools, path, common, branch string) error {
	git := t.Git
	// path itself is a folder, not a link to one: a link left where a
	// worktree was moved from would pass every check below through it, and
	// the specialist would work in an alias another could point elsewhere.
	kind, err := t.Kind(ctx, path)
	if err != nil {
		return fmt.Errorf("look at %s: %w", path, err)
	}
	if kind != "dir" {
		return no(nil)
	}
	top, err := git(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return no(err)
	}
	if err := isSame(ctx, t, top, path); err != nil {
		return err
	}
	other, err := commonDir(ctx, git, path)
	if err != nil {
		return no(err)
	}
	if err := isSame(ctx, t, other, common); err != nil {
		return err
	}
	// The full ref: an abbreviation may carry heads/ where a tag has the
	// branch's name, and a detached HEAD has none.
	head, err := git(ctx, path, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return no(err)
	}
	if head != "refs/heads/"+branch {
		return no(nil)
	}
	// A branch with no commit (its ref gone) would start a history of its
	// own: the worktree's HEAD must name a commit.
	if _, err = git(ctx, path, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return no(err)
	}
	return nil
}

// isSame checks that a and b name one entry: a negative answer when they do
// not, the probe's error when it could not tell.
func isSame(ctx context.Context, t Tools, a, b string) error {
	ok, err := t.Same(ctx, a, b)
	if err != nil {
		return fmt.Errorf("compare %s with %s: %w", a, b, err)
	}
	if !ok {
		return no(nil)
	}
	return nil
}

// registered checks that git registered path as its worktree: the
// worktree's git folder, as git names it from path, is in the registry
// of the repository whose common folder is common, and holds a gitdir file
// that names path's .git back, read in the sandbox. Its answers are
// isWorktree's.
func registered(ctx context.Context, t Tools, path, common string) error {
	dir, err := t.Git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return no(err)
	}
	if !filepath.IsAbs(dir) {
		return no(nil)
	}
	// The git folder is in the repository's registry, where git lists its
	// worktrees from: common's worktrees folder, not one moved out of it.
	if err := isSame(ctx, t, filepath.Dir(dir), filepath.Join(common, "worktrees")); err != nil {
		return err
	}
	gitdir := filepath.Join(dir, "gitdir")
	text, ok, err := t.Read(ctx, gitdir)
	if err != nil {
		return fmt.Errorf("read %s: %w", gitdir, err)
	}
	if !ok {
		return no(nil)
	}
	// git writes the backlink absolute, or relative to the git folder
	// (worktree.useRelativePaths): both name path's .git. It is anchored
	// as written, never cleaned: a ".." after a missing folder or a link
	// resolves in the sandbox as git would resolve it, not as a string.
	back := strings.TrimRight(text, "\n")
	if !filepath.IsAbs(back) {
		back = dir + string(filepath.Separator) + back
	}
	// The backlink names .git in path itself: its folder, as written, is
	// path in the sandbox (a copy whose .git is a hard link to this one's
	// is another folder). And path's .git is the file git wrote there, not
	// a link to another worktree's.
	suffix := string(filepath.Separator) + ".git"
	if !strings.HasSuffix(back, suffix) {
		return no(nil)
	}
	if err := isSame(ctx, t, strings.TrimSuffix(back, suffix), path); err != nil {
		return err
	}
	dotGit := filepath.Join(path, ".git")
	kind, err := t.Kind(ctx, dotGit)
	if err != nil {
		return fmt.Errorf("look at %s: %w", dotGit, err)
	}
	if kind != "file" {
		return no(nil)
	}
	return nil
}

// commonDir is the git folder the checkout's worktrees share, absolute.
func commonDir(ctx context.Context, git Git, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return out, nil
}
