package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func removalTask(t *testing.T) (checkout, path, branch string) {
	t.Helper()
	checkout = repo(t)
	if err := os.WriteFile(filepath.Join(checkout, "tracked"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "tracked")
	removalCommit(t, checkout)
	path, branch = checkout+"--task", "agent/task"
	mustGit(t, checkout, "worktree", "add", "-b", branch, path, "HEAD")
	return
}

func removalCommit(t *testing.T, path string) string {
	t.Helper()
	mustGit(t, path, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "task")
	return mustGit(t, path, "rev-parse", "HEAD")
}

func removalMerge(t *testing.T, checkout, branch string) {
	t.Helper()
	mustGit(t, checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "merge", "-q", "--no-ff", branch)
}

func assertRemovalKept(t *testing.T, r Removal, contains string) {
	t.Helper()
	branchKept, err := Remove(context.Background(), tools, r)
	var kept Kept
	if !errors.As(err, &kept) || Unsure(err) || !strings.Contains(kept.Reason, contains) || branchKept != "" {
		t.Fatalf("Remove = %q, %v; want Kept containing %q", branchKept, err, contains)
	}
	if _, err := os.Stat(r.Path); err != nil {
		t.Fatalf("worktree did not stay: %v", err)
	}
	if r.Branch != "" {
		if exists, err := hasBranch(context.Background(), git, r.Path, r.Branch); !exists || err != nil {
			t.Fatalf("branch did not stay: %v, %v", exists, err)
		}
	}
}

func assertRemovalGone(t *testing.T, checkout, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree not gone: %v", err)
	}
	if list := mustGit(t, checkout, "worktree", "list", "--porcelain"); strings.Contains(list, "worktree "+path+"\n") {
		t.Fatalf("worktree is still registered: %s", list)
	}
}

func TestRemoveTakesAReviewsCleanWorktree(t *testing.T) {
	checkout := repo(t)
	path := checkout + "--review"
	sha := mustGit(t, checkout, "rev-parse", "HEAD")
	mustGit(t, checkout, "worktree", "add", "--detach", path, sha)
	kept, err := Remove(context.Background(), tools, Removal{Path: path, Head: sha})
	if err != nil || kept != "" {
		t.Fatalf("Remove = %q, %v", kept, err)
	}
	assertRemovalGone(t, checkout, path)
}

func TestRemoveTakesAMergedThreadsWorktreeAndBranch(t *testing.T) {
	checkout, path, branch := removalTask(t)
	c := removalCommit(t, path)
	removalMerge(t, checkout, branch)
	kept, err := Remove(context.Background(), tools, Removal{Path: path, Branch: branch, Heads: []string{c}})
	if err != nil || kept != "" {
		t.Fatalf("Remove = %q, %v", kept, err)
	}
	assertRemovalGone(t, checkout, path)
	if exists, err := hasBranch(context.Background(), git, checkout, branch); exists || err != nil {
		t.Fatalf("branch remains: %v, %v", exists, err)
	}
}

func TestRemoveKeepsADirtyWorktree(t *testing.T) {
	for _, name := range []string{"untracked", "tracked"} {
		t.Run(name, func(t *testing.T) {
			checkout, path, branch := removalTask(t)
			c := removalCommit(t, path)
			removalMerge(t, checkout, branch)
			if err := os.WriteFile(filepath.Join(path, name), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r := Removal{Path: path, Branch: branch, Heads: []string{c}}
			assertRemovalKept(t, r, path+" has uncommitted changes or untracked files")
		})
	}
}

func TestRemoveKeepsCommitsInNoLinkedPullRequest(t *testing.T) {
	checkout, path, branch := removalTask(t)
	c1 := removalCommit(t, path)
	c2 := removalCommit(t, path)
	r := Removal{Path: path, Branch: branch, Heads: []string{c1}}
	assertRemovalKept(t, r, "commits in no linked pull request")
	r.Heads = []string{strings.Repeat("f", 40)}
	assertRemovalKept(t, r, "commits in no linked pull request")
	r.Heads = []string{c2}
	if _, err := Remove(context.Background(), tools, r); err != nil {
		t.Fatal(err)
	}
	assertRemovalGone(t, checkout, path)
}

func TestRemoveTakesANoPullThreadWithNoCommits(t *testing.T) {
	checkout, path, branch := removalTask(t)
	kept, err := Remove(context.Background(), tools, Removal{Path: path, Branch: branch})
	if err != nil || kept != "" {
		t.Fatalf("Remove = %q, %v", kept, err)
	}
	assertRemovalGone(t, checkout, path)
	if exists, err := hasBranch(context.Background(), git, checkout, branch); exists || err != nil {
		t.Fatalf("branch remains: %v, %v", exists, err)
	}
}

func TestRemoveKeepsANoPullThreadWithCommits(t *testing.T) {
	_, path, branch := removalTask(t)
	removalCommit(t, path)
	assertRemovalKept(t, Removal{Path: path, Branch: branch}, "beyond its start")
}

func TestRemoveKeepsALockedWorktree(t *testing.T) {
	checkout, path, branch := removalTask(t)
	mustGit(t, checkout, "worktree", "lock", path)
	assertRemovalKept(t, Removal{Path: path, Branch: branch}, path+" is locked (see ")
}

func TestRemoveNeverTakesTheCheckoutItself(t *testing.T) {
	checkout := repo(t)
	assertRemovalKept(t, Removal{Path: checkout, Branch: "main"}, "is not a worktree git registered")
}

func TestRemoveKeepsABranchGitWillNotDelete(t *testing.T) {
	checkout, path, branch := removalTask(t)
	c := removalCommit(t, path)
	kept, err := Remove(context.Background(), tools, Removal{Path: path, Branch: branch, Heads: []string{c}})
	if err != nil || !strings.Contains(kept, "git branch -d kept "+branch) || strings.ContainsAny(kept, "\r\n") || len(kept) > MaxStale {
		t.Fatalf("Remove = %q, %v", kept, err)
	}
	assertRemovalGone(t, checkout, path)
	if exists, err := hasBranch(context.Background(), git, checkout, branch); !exists || err != nil {
		t.Fatalf("branch not kept: %v, %v", exists, err)
	}
}

func TestRemoveOfAnAbsentWorktreeIsDone(t *testing.T) {
	kept, err := Remove(context.Background(), tools, Removal{Path: filepath.Join(t.TempDir(), "absent"), Branch: "agent/task"})
	if err != nil || kept != "" {
		t.Fatalf("Remove = %q, %v", kept, err)
	}
}

func TestAWorktreeRemoveThatCouldNotRunIsUnsure(t *testing.T) {
	_, path, branch := removalTask(t)
	tl := tools
	failed := errors.New("sandbox failed")
	tl.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if slices.Contains(args, "remove") {
			return "", failed
		}
		return git(ctx, dir, args...)
	}
	kept, err := Remove(context.Background(), tl, Removal{Path: path, Branch: branch})
	if !Unsure(err) || !errors.Is(err, failed) || kept != "" {
		t.Fatalf("Remove = %q, %v", kept, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree did not stay: %v", err)
	}
}

func TestRemoveKeepsAWorktreeOnAnotherBranch(t *testing.T) {
	_, path, branch := removalTask(t)
	mustGit(t, path, "checkout", "-b", "agent/other")
	assertRemovalKept(t, Removal{Path: path, Branch: branch}, "HEAD of "+path+" is not on the branch "+branch)
}
