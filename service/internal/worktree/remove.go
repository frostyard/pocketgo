package worktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Kept is a worktree pocketd leaves in place, and why: trying again does
// not change it until someone looks (ADR-0014's no-force rule).
type Kept struct{ Reason string }

func (k Kept) Error() string { return k.Reason }

// Removal names a closed thread's worktree.
type Removal struct {
	// Path is the worktree, absolute.
	Path string
	// Branch is a task thread's branch: HEAD must be on it, and git branch
	// -d deletes it once the worktree is gone. "" for a cross-review's
	// worktree, detached at Head.
	Branch string
	// Head is the commit a cross-review's worktree is detached at (Branch "").
	Head string
	// Heads are the recorded head commits of a task thread's linked pull
	// requests. With none, HEAD must have no commit beyond its branch's start.
	Heads []string
}

// Remove removes r's worktree with git worktree remove, never --force,
// and then a task thread's branch with git branch -d, never -D. It requires
// an absolute path, a registered non-main worktree, no lock, the expected
// branch or detached review commit, and no changes or untracked files.
// Reviews use isolated git options and require a full commit id. A task's
// HEAD must be an ancestor of a recorded linked pull request's head, or,
// with none linked, of the branch's oldest recorded reflog commit. An absent
// path is done. A refusal is Kept; a check that could not tell is Unsure.
// Once the worktree is gone, a branch deletion failure is returned only as
// branchKept, a bounded one-line reason, not an error.
func Remove(ctx context.Context, t Tools, r Removal) (branchKept string, err error) {
	defer func() {
		var kept Kept
		if err != nil && !errors.As(err, &kept) && !Unsure(err) {
			err = Kept{Reason: err.Error()}
		}
	}()
	if !filepath.IsAbs(r.Path) {
		return "", Kept{Reason: "the worktree must be an absolute path"}
	}
	if r.Branch == "" {
		t = reviewTools(t)
		if !isCommitID(r.Head) {
			return "", notCommitID(r.Head)
		}
	}
	kind, err := t.Kind(ctx, r.Path)
	if err != nil {
		return "", unsure{fmt.Errorf("look at %s: %w", r.Path, err)}
	}
	if kind == "absent" {
		return "", nil
	}
	if kind != "dir" {
		return "", Kept{Reason: r.Path + " is not a folder"}
	}
	common, err := commonDir(ctx, t.Git, r.Path)
	if err != nil {
		return "", explain(ctx, r.Path, no(err), r.Path+" is not a git worktree")
	}
	if err := isTopOf(ctx, t, r.Path, common); err != nil {
		return "", explain(ctx, r.Path, err, r.Path+" is not a worktree git registered")
	}
	if err := registered(ctx, t, r.Path, common); err != nil {
		return "", explain(ctx, r.Path, err, r.Path+" is not a worktree git registered")
	}
	lock, err := lockOf(ctx, t, r.Path)
	if err != nil {
		return "", explain(ctx, r.Path, err, "")
	}
	if lock != "" {
		return "", Kept{Reason: fmt.Sprintf("%s is locked (see %s)", r.Path, lock)}
	}
	if r.Branch == "" {
		if err := isDetachedAt(ctx, t, r.Path, common, r.Head); err != nil {
			return "", explain(ctx, r.Path, err, fmt.Sprintf("%s is not detached at %s", r.Path, r.Head))
		}
	} else if err := isWorktree(ctx, t, r.Path, common, r.Branch); err != nil {
		return "", explain(ctx, r.Path, err, fmt.Sprintf("HEAD of %s is not on the branch %s", r.Path, r.Branch))
	}
	// Check changes before deciding whether the commits may be removed.
	status, err := t.Git(ctx, r.Path, "status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil {
		return "", explain(ctx, r.Path, no(err), "git could not check the worktree's status")
	}
	if strings.TrimSpace(status) != "" {
		return "", Kept{Reason: r.Path + " has uncommitted changes or untracked files"}
	}
	// An index entry marked assume-unchanged or skip-worktree hides a
	// file's change from status, and from git worktree remove's own check:
	// none may be, or the removal could delete that change (cross-review
	// of #106, round 3).
	if err := noHiddenEntries(ctx, t, r.Path); err != nil {
		return "", err
	}
	// Check the task's commits before removing either its folder or branch.
	if r.Branch != "" {
		if err := removableCommits(ctx, t, r); err != nil {
			return "", err
		}
	}
	if _, err := t.Git(ctx, common, "worktree", "remove", r.Path); err != nil {
		return "", explain(ctx, r.Path, no(err), "git would not remove "+r.Path)
	}
	if r.Branch != "" {
		if _, err := t.Git(ctx, common, "branch", "-d", r.Branch); err != nil {
			return staleReason("git branch -d kept " + r.Branch + ": " + err.Error()), nil
		}
	}
	return "", nil
}

// removableCommits checks the task's HEAD against its linked heads or its
// recorded start. Missing linked commits cannot establish ancestry.
func removableCommits(ctx context.Context, t Tools, r Removal) error {
	head, err := t.Git(ctx, r.Path, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return explain(ctx, r.Path, no(err), "git could not read HEAD's commit")
	}
	if !isCommitID(head) {
		return notCommitID(head)
	}
	if len(r.Heads) > 0 {
		for _, h := range r.Heads {
			if !isCommitID(h) {
				continue
			}
			exists, err := hasCommit(ctx, t.Git, r.Path, h)
			if err != nil {
				return unsure{err}
			}
			if !exists {
				continue
			}
			ancestor, err := removalAncestor(ctx, t.Git, r.Path, head, h)
			if err != nil {
				return err
			}
			if ancestor {
				return nil
			}
		}
		return Kept{Reason: fmt.Sprintf("HEAD %s of %s has commits in no linked pull request", head[:12], r.Path)}
	}
	reason := "the start of the branch " + r.Branch + " is not recorded"
	log, err := t.Git(ctx, r.Path, "reflog", "show", "--format=%H", "refs/heads/"+r.Branch)
	if err != nil {
		return explain(ctx, r.Path, no(err), reason)
	}
	var start string
	for _, line := range strings.Split(log, "\n") {
		if line != "" {
			start = line
		}
	}
	if !isCommitID(start) {
		return Kept{Reason: reason}
	}
	ancestor, err := removalAncestor(ctx, t.Git, r.Path, head, start)
	if err != nil {
		return err
	}
	if !ancestor {
		return Kept{Reason: "the branch " + r.Branch + " has commits beyond its start, and no pull request is linked"}
	}
	return nil
}

// removalAncestor distinguishes a definite no (exit 1) from a check that
// could not tell, including other exits of merge-base itself.
func removalAncestor(ctx context.Context, git Git, path, head, target string) (bool, error) {
	_, err := git(ctx, path, "merge-base", "--is-ancestor", head, target)
	if err == nil {
		return true, nil
	}
	var exit *GitExit
	if errors.As(err, &exit) && exit.Status == 1 {
		return false, nil
	}
	return false, unsure{fmt.Errorf("git could not compare %s with %s: %w", head, target, err)}
}

// noHiddenEntries keeps a worktree whose index marks an entry
// assume-unchanged (a lowercase tag in git ls-files -v) or skip-worktree
// ("S"): status cannot see a change to such a file, so its absence from
// status says nothing. Every entry must be tagged "H ".
func noHiddenEntries(ctx context.Context, t Tools, path string) error {
	if t.Each == nil {
		return unsure{errors.New("no streaming git: the worktree's index cannot be checked")}
	}
	var hidden error
	err := t.Each(ctx, path, 0, func(entry string) error {
		if entry != "" && !strings.HasPrefix(entry, "H ") {
			hidden = Kept{Reason: fmt.Sprintf("%s marks %q so status cannot see a change to it", path, entry)}
			return hidden
		}
		return nil
	}, "ls-files", "-v", "-z")
	if hidden != nil {
		return hidden
	}
	if err != nil {
		return explain(ctx, path, no(err), "git could not list the worktree's index")
	}
	return nil
}
