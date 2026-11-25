package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func withOrigin(t *testing.T, checkout string) (bare string) {
	t.Helper()
	bare = filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, checkout, "clone", "-q", "--bare", checkout, bare)
	mustGit(t, checkout, "remote", "add", "origin", bare)
	return bare
}

func advance(t *testing.T, bare, branch string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "advance")
	mustGit(t, bare, "clone", "-q", bare, clone)
	mustGit(t, clone, "checkout", "-B", branch)
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "advance")
	mustGit(t, clone, "push", "-q", "origin", branch)
	return mustGit(t, clone, "rev-parse", "HEAD")
}

func TestAddStartsFromTheTipOfOriginsDefaultBranch(t *testing.T) {
	checkout := repo(t)
	head := mustGit(t, checkout, "rev-parse", "HEAD")
	tip := advance(t, withOrigin(t, checkout), "main")
	path, branch := checkout+"--fresh", "agent/fresh"
	start, err := Add(context.Background(), tools, checkout, path, branch)
	if err != nil {
		t.Fatal(err)
	}
	if start != (Start{Commit: tip, Ref: "origin/main"}) {
		t.Fatalf("start = %+v", start)
	}
	if got := mustGit(t, path, "rev-parse", "HEAD"); got != tip || got == head {
		t.Fatalf("HEAD = %s, tip = %s, checkout = %s", got, tip, head)
	}
	if _, err := git(context.Background(), checkout, "rev-parse", "--abbrev-ref", branch+"@{upstream}"); err == nil {
		t.Fatal("new branch tracks an upstream")
	}
	if got := mustGit(t, checkout, "rev-parse", "HEAD"); got != head {
		t.Fatal("checkout HEAD changed")
	}
}

func TestAddFollowsOriginsDefaultBranchByName(t *testing.T) {
	checkout := repo(t)
	bare := withOrigin(t, checkout)
	tip := advance(t, bare, "trunk")
	mustGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/trunk")
	path := checkout + "--trunk"
	start, err := Add(context.Background(), tools, checkout, path, "agent/trunk")
	if err != nil {
		t.Fatal(err)
	}
	if start != (Start{Commit: tip, Ref: "origin/trunk"}) || mustGit(t, path, "rev-parse", "HEAD") != tip {
		t.Fatalf("start = %+v", start)
	}
}

func TestAFetchThatFailsStartsFromHEAD(t *testing.T) {
	for _, remote := range []bool{true, false} {
		name := "no remote"
		if remote {
			name = "missing origin"
		}
		t.Run(name, func(t *testing.T) {
			checkout := repo(t)
			if remote {
				mustGit(t, checkout, "remote", "add", "origin", filepath.Join(t.TempDir(), "absent"))
			}
			path := checkout + "--fallback"
			start, err := Add(context.Background(), tools, checkout, path, "agent/fallback")
			checkFallback(t, checkout, path, start, err)
		})
	}
}

func checkFallback(t *testing.T, checkout, path string, start Start, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	head := mustGit(t, checkout, "rev-parse", "HEAD")
	if start.Commit != head || start.Ref != "HEAD" || start.Stale == "" || strings.ContainsAny(start.Stale, "\r\n") || len(start.Stale) > MaxStale {
		t.Fatalf("start = %+v", start)
	}
	if mustGit(t, path, "rev-parse", "HEAD") != head {
		t.Fatal("did not fall back to HEAD")
	}
}

func TestARetryKeepsItsBranchWhereItIs(t *testing.T) {
	checkout := repo(t)
	bare := withOrigin(t, checkout)
	tip1 := advance(t, bare, "main")
	path, branch := checkout+"--retry", "agent/retry"
	if _, err := Add(context.Background(), tools, checkout, path, branch); err != nil {
		t.Fatal(err)
	}
	tip2 := advance(t, bare, "main")
	tl := tools
	tl.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if slices.Contains(args, "ls-remote") || slices.Contains(args, "fetch") {
			t.Fatal("retry looked up or fetched origin")
		}
		return git(ctx, dir, args...)
	}
	for _, remove := range []bool{false, true} {
		if remove {
			mustGit(t, checkout, "worktree", "remove", "--force", path)
		}
		start, err := Add(context.Background(), tl, checkout, path, branch)
		if err != nil {
			t.Fatal(err)
		}
		if start != (Start{}) {
			t.Fatalf("retry start = %+v", start)
		}
		if got := mustGit(t, path, "rev-parse", "HEAD"); got != tip1 || got == tip2 {
			t.Fatalf("retry HEAD = %s", got)
		}
	}
}

func TestAFetchCutByTheCallersDeadlineMakesNothing(t *testing.T) {
	checkout := repo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tl := tools
	tl.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if slices.Contains(args, "ls-remote") {
			cancel()
			return "", ctx.Err()
		}
		return git(ctx, dir, args...)
	}
	path, branch := checkout+"--cancelled", "agent/cancelled"
	_, err := Add(ctx, tl, checkout, path, branch)
	if err == nil || !Unsure(err) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("path exists: %v", err)
	}
	if exists, err := hasBranch(context.Background(), git, checkout, branch); exists || err != nil {
		t.Fatalf("branch exists = %v, error = %v", exists, err)
	}
}

func TestAddNewStartsFromTheTipOfOriginsDefaultBranch(t *testing.T) {
	checkout := repo(t)
	head := mustGit(t, checkout, "rev-parse", "HEAD")
	tip := advance(t, withOrigin(t, checkout), "main")
	path, branch := checkout+"--owner-fresh", "owner/fresh"
	var got steps
	start, err := AddNew(context.Background(), tools, checkout, path, branch, token, StepNone, got.record)
	if err != nil {
		t.Fatal(err)
	}
	if start != (Start{Commit: tip, Ref: "origin/main"}) {
		t.Fatalf("start = %+v", start)
	}
	if actual := mustGit(t, path, "rev-parse", "HEAD"); actual != tip || actual == head {
		t.Fatalf("HEAD = %s", actual)
	}
	if _, err := git(context.Background(), checkout, "rev-parse", "--abbrev-ref", branch+"@{upstream}"); err == nil {
		t.Fatal("new branch tracks an upstream")
	}
	if mustGit(t, checkout, "rev-parse", "HEAD") != head {
		t.Fatal("checkout HEAD changed")
	}
}

func TestAddNewStartsFromHEADWhenTheFetchFails(t *testing.T) {
	checkout := repo(t)
	mustGit(t, checkout, "remote", "add", "origin", filepath.Join(t.TempDir(), "absent"))
	path := checkout + "--owner-fallback"
	var got steps
	start, err := AddNew(context.Background(), tools, checkout, path, "owner/fallback", token, StepNone, got.record)
	checkFallback(t, checkout, path, start, err)
}

func TestStaleReasonIsOneBoundedUTF8Line(t *testing.T) {
	reason := staleReason("lookup\r\nfailed\n" + strings.Repeat("界", MaxStale))
	if strings.ContainsAny(reason, "\r\n") || !strings.HasPrefix(reason, "lookup; failed; ") || !utf8.ValidString(reason) || len(reason) > MaxStale || !strings.HasSuffix(reason, "…") {
		t.Fatalf("reason = %q (%d bytes)", reason, len(reason))
	}
}

// AddNewKeepingBase hands its keep the new branch's base, why it starts
// from HEAD included, before the folder is made or any step recorded; a
// keep that fails makes nothing, so a retry decides the base again, and a
// retry from a made worktree decides none (ADR-0074; Codex review of #174,
// round 2).
func TestAddNewKeepsTheBaseBeforeItMakesAnything(t *testing.T) {
	checkout := repo(t)
	mustGit(t, checkout, "remote", "add", "origin", filepath.Join(t.TempDir(), "absent"))
	path, branch := checkout+"--owner-kept", "owner/kept"
	var got steps
	var kept []Start
	failed := errors.New("the record failed")
	keep := func(start Start) error {
		kept = append(kept, start)
		if _, err := os.Lstat(path); !os.IsNotExist(err) || len(got) != 0 {
			t.Fatalf("the base was handed over after the folder (%v) or a step (%v)", err, got)
		}
		if len(kept) == 1 {
			return failed
		}
		return nil
	}
	if _, err := AddNewKeepingBase(context.Background(), tools, checkout, path, branch, token, StepNone, keep, got.record); !errors.Is(err, failed) {
		t.Fatalf("a keep that fails: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) || len(got) != 0 {
		t.Fatalf("a failed keep made the folder (%v) or recorded %v", err, got)
	}
	start, err := AddNewKeepingBase(context.Background(), tools, checkout, path, branch, token, StepNone, keep, got.record)
	checkFallback(t, checkout, path, start, err)
	if len(kept) != 2 || kept[1] != start || kept[1].Stale == "" {
		t.Fatalf("kept %+v, started %+v", kept, start)
	}
	if _, err := AddNewKeepingBase(context.Background(), tools, checkout, path, branch, token, got[len(got)-1], keep, got.record); err != nil || len(kept) != 2 {
		t.Fatalf("a retry from the made worktree: %v, kept %d bases", err, len(kept))
	}
}
