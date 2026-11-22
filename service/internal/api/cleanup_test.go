package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

func cleanupCommit(t *testing.T, path string) string {
	t.Helper()
	branchBaseGit(t, path, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "task")
	return branchBaseGit(t, path, "rev-parse", "HEAD")
}

// cleanupMerged records the real head as merged, then drives the same
// refresh and review passes that close a merged thread in production.
func cleanupMerged(t *testing.T, h *teamHarness, thread store.ThreadRow, head string) store.ThreadRow {
	t.Helper()
	ctx := context.Background()
	answer := strings.ReplaceAll(githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false), strings.Repeat("a", 40), head)
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": answer})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "report"}, team.Report{Message: "PR #3", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	pulls, err := h.store.PullsOfThread(ctx, thread.ID)
	if err != nil || len(pulls) != 1 {
		t.Fatalf("pulls = %+v, %v", pulls, err)
	}
	merged := pulls[0]
	merged.State, merged.CheckedAt = store.PullMerged, h.clock.Now()
	if _, _, err := h.store.UpdatePull(ctx, merged.ID, merged); err != nil {
		t.Fatal(err)
	}
	if done, err := h.server.RefreshPulls(ctx); !done || err != nil {
		t.Fatalf("refresh = %v, %v", done, err)
	}
	h.server.reviewPass(ctx)
	row, err := h.store.Thread(ctx, thread.ID)
	if err != nil || row.State != store.ThreadClosed {
		t.Fatalf("closed thread = %+v, %v", row, err)
	}
	return row
}

func cleanupClose(t *testing.T, h *teamHarness, thread store.ThreadRow) store.ThreadRow {
	t.Helper()
	ctx := context.Background()
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	row, err := h.store.Thread(ctx, thread.ID)
	if err != nil || row.State != store.ThreadClosed {
		t.Fatalf("closed thread = %+v, %v", row, err)
	}
	return row
}

func cleanupGone(t *testing.T, h *teamHarness, thread store.ThreadRow) {
	t.Helper()
	if _, err := os.Lstat(thread.Cwd); !os.IsNotExist(err) {
		t.Fatalf("worktree not gone: %v", err)
	}
	if refs := branchBaseGit(t, h.checkout, "for-each-ref", "--format=%(refname)", "refs/heads/"+thread.Branch); refs != "" {
		t.Fatalf("branch remains: %s", refs)
	}
}

func cleanupKept(t *testing.T, h *teamHarness, row store.ThreadRow, why string) {
	t.Helper()
	if _, err := os.Stat(row.Cwd); err != nil {
		t.Fatalf("worktree not kept: %v", err)
	}
	if refs := branchBaseGit(t, h.checkout, "for-each-ref", "--format=%(refname)", "refs/heads/"+row.Branch); refs == "" {
		t.Fatal("branch not kept")
	}
	if !strings.Contains(row.Report, row.Cwd) || !strings.Contains(row.Report, why) {
		t.Fatalf("report lacks kept reason: %q", row.Report)
	}
}

func TestAMergedThreadsCleanWorktreeAndBranchGo(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	head := cleanupCommit(t, thread.Cwd)
	branchBaseGit(t, h.checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "merge", "-q", "--no-ff", thread.Branch)
	row := cleanupMerged(t, h, thread, head)
	cleanupGone(t, h, row)
	if row.Report != team.MergedText([]string{"frostyard/example#3"}) {
		t.Fatalf("report = %q", row.Report)
	}
}

func TestADirtyWorktreeIsKeptAndItsReportSaysWhy(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	head := cleanupCommit(t, thread.Cwd)
	branchBaseGit(t, h.checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "merge", "-q", "--no-ff", thread.Branch)
	if err := os.WriteFile(filepath.Join(thread.Cwd, "untracked"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	row := cleanupMerged(t, h, thread, head)
	cleanupKept(t, h, row, "uncommitted changes or untracked files")
	// Republished closes and later passes do not append the note twice.
	h.server.reviewPass(context.Background())
	again, err := h.store.Thread(context.Background(), thread.ID)
	if err != nil || again.Report != row.Report {
		t.Fatalf("second pass = %+v, %v", again, err)
	}
}

func TestCommitsInNoLinkedPullRequestKeepTheWorktree(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	head := cleanupCommit(t, thread.Cwd)
	cleanupCommit(t, thread.Cwd)
	cleanupKept(t, h, cleanupMerged(t, h, thread, head), "commits in no linked pull request")
}

func TestAClosedThreadWithNoPullAndNoCommitsLosesItsWorktree(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	cleanupGone(t, h, cleanupClose(t, h, thread))
}

func TestAClosedThreadWithNoPullButCommitsKeepsIt(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	cleanupCommit(t, thread.Cwd)
	cleanupKept(t, h, cleanupClose(t, h, thread), "beyond its start")
}

func TestAnOwnersSessionWorktreeIsNeverRemoved(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	if err := h.store.ClaimWorktree(context.Background(), "owner-worktree", store.SessionWorktree{Path: thread.Cwd, Dest: thread.Cwd, Branch: thread.Branch, Top: h.checkout}); err != nil {
		t.Fatal(err)
	}
	row := cleanupClose(t, h, thread)
	h.server.cleanMu.Lock()
	decided := h.server.cleanDone[thread.ID]
	h.server.cleanMu.Unlock()
	if !decided {
		t.Fatal("cleanup never decided to leave the owner's worktree alone")
	}
	if _, err := os.Stat(row.Cwd); err != nil {
		t.Fatalf("owner's worktree removed: %v", err)
	}
	if strings.Contains(row.Report, row.Cwd) {
		t.Fatalf("owner's worktree was noted: %q", row.Report)
	}
}

func TestALockedWorktreeIsKept(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	branchBaseGit(t, h.checkout, "worktree", "lock", thread.Cwd)
	cleanupKept(t, h, cleanupClose(t, h, thread), "locked")
}

// A kept worktree's note whose report write fails is written at the next
// pass, exactly once, and only then is the thread decided.
func TestAKeptWorktreesNoteThatFailedToWriteIsWrittenAtTheNextPass(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	branchBaseGit(t, h.checkout, "worktree", "lock", thread.Cwd)
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TRIGGER cleanup_note_fails BEFORE UPDATE OF report ON threads
		WHEN instr(NEW.report, 'pocketd kept the worktree') > 0 BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	row := cleanupClose(t, h, thread)
	if strings.Contains(row.Report, row.Cwd) {
		t.Fatalf("failed write was noted: %q", row.Report)
	}
	h.server.cleanMu.Lock()
	decided := h.server.cleanDone[thread.ID]
	h.server.cleanMu.Unlock()
	if decided {
		t.Fatal("thread decided before its note was written")
	}
	if _, err := db.Exec(`DROP TRIGGER cleanup_note_fails`); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(context.Background())
	after, err := h.store.Thread(context.Background(), thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanupKept(t, h, after, "locked")
	if n := strings.Count(after.Report, "pocketd kept the worktree"); n != 1 {
		t.Fatalf("report has %d notes, want 1: %q", n, after.Report)
	}
	h.server.reviewPass(context.Background())
	again, err := h.store.Thread(context.Background(), thread.ID)
	if err != nil || again.Report != after.Report {
		t.Fatalf("third pass = %+v, %v", again, err)
	}
}

// cleanupReviewHarness starts a real detached review worktree at a real
// linked head, using the same profiles and GitHub fake as the review tests.
func cleanupReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	needsReviewGit(t)
	h := &reviewHarness{teamHarness: newTeamHarness(t)}
	h.engine.more = []proto.Model{
		{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}},
		{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}},
	}
	h.writeProfiles(map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":   "---\nmodel: anthropic/claude-opus-5-5\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n",
	})
	h.server.reviewAsync, h.server.reviewsOn = false, true
	h.server.repoURL = func(string) string { return h.checkout }
	h.vera = veraThread(t, h.teamHarness)
	head := cleanupCommit(t, h.vera.Cwd)
	base := branchBaseGit(t, h.checkout, "rev-parse", "main")
	h.gh = &fakeGitHub{repo: "frostyard/example", number: 3, branch: h.vera.Branch, head: head, state: "open", labels: map[string]bool{}, ci: map[string]string{head: greenRuns}, base: base}
	h.engine.mu.Lock()
	h.engine.onGH = h.gh.gh
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(context.Background(), TeamCall{Session: h.vera.Session, Task: 1, CallID: "review-pull"}, team.Report{Message: "PR #3", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	threads, err := h.store.ReviewThreadsOf(context.Background(), h.pull(t).ID)
	if err != nil || len(threads) != 1 || threads[0].Session == 0 {
		t.Fatalf("review threads = %+v, %v", threads, err)
	}
	h.teg = threads[0]
	if got := branchBaseGit(t, h.teg.Cwd, "rev-parse", "HEAD"); got != head {
		t.Fatalf("review HEAD = %s, want %s", got, head)
	}
	if got := branchBaseGit(t, h.teg.Cwd, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
		t.Fatalf("review is not detached: %s", got)
	}
	return h
}

func cleanupReviewGone(t *testing.T, h *reviewHarness) {
	t.Helper()
	if _, err := os.Lstat(h.teg.Cwd); !os.IsNotExist(err) {
		t.Fatalf("review worktree not gone: %v", err)
	}
	list := branchBaseGit(t, h.checkout, "worktree", "list", "--porcelain")
	if strings.Contains(list, "worktree "+h.teg.Cwd+"\n") {
		t.Fatalf("review worktree remains registered: %s", list)
	}
}

func TestAReviewsWorktreeGoesWithItsVerdict(t *testing.T) {
	h := cleanupReviewHarness(t)
	if _, err := h.verdict(h.teg.Session, "verdict", h.teg.ReviewHead, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	// The fake session is idle: its run has ended before the cleanup pass.
	h.server.reviewPass(context.Background())
	cleanupReviewGone(t, h)
	row, err := h.store.Thread(context.Background(), h.teg.ID)
	verdict, found, verr := h.store.ReviewOfThread(context.Background(), h.teg.ID)
	if err != nil || verr != nil || !found || row.State != store.ThreadClosed {
		t.Fatalf("review = %+v, %v; verdict = %+v, %v", row, err, verdict, verr)
	}
	want := fmt.Sprintf("Verdict R%d: frostyard/example#3 at %s is %s.", verdict.ID, shortSHA(h.teg.ReviewHead), team.VerdictReady)
	if row.Report != want {
		t.Fatalf("review report = %q, want %q", row.Report, want)
	}
}

func TestAReviewsWorktreeGoesWhenItEndsWithoutAVerdict(t *testing.T) {
	h := cleanupReviewHarness(t)
	// Its idle session has no verdict; the pass ends the review and cleans it.
	h.server.reviewPass(context.Background())
	row, err := h.store.Thread(context.Background(), h.teg.ID)
	if err != nil || row.State != store.ThreadClosed || !strings.Contains(row.Report, "no verdict") {
		t.Fatalf("review = %+v, %v", row, err)
	}
	cleanupReviewGone(t, h)
}

func TestAReviewsWorktreeWaitsForItsRun(t *testing.T) {
	h := cleanupReviewHarness(t)
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[h.teg.Session] = true
	h.engine.mu.Unlock()
	if _, err := h.verdict(h.teg.Session, "verdict", h.teg.ReviewHead, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(context.Background())
	row, err := h.store.Thread(context.Background(), h.teg.ID)
	if err != nil || row.State != store.ThreadClosed {
		t.Fatalf("review did not close: %+v, %v", row, err)
	}
	if _, err := os.Stat(h.teg.Cwd); err != nil {
		t.Fatalf("busy review's worktree removed: %v", err)
	}
	h.engine.mu.Lock()
	h.engine.busy[h.teg.Session] = false
	h.engine.mu.Unlock()
	h.server.reviewPass(context.Background())
	cleanupReviewGone(t, h)
}

// cleanupFailRemoveEngine fails the first worktree removal before git ran;
// all other engine behavior remains the harness's, without a production hook.
type cleanupFailRemoveEngine struct {
	Engine
	attempts int
}

func (e *cleanupFailRemoveEngine) Git(ctx context.Context, dir string, args ...string) (string, error) {
	if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
		e.attempts++
		if e.attempts == 1 {
			return "", errors.New("sandbox failed")
		}
	}
	return e.Engine.Git(ctx, dir, args...)
}

func TestAWorktreeRemovalThatCouldNotRunIsTriedAgain(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	engine := &cleanupFailRemoveEngine{Engine: h.server.engine}
	h.server.engine = engine
	row := cleanupClose(t, h, thread)
	if engine.attempts != 1 {
		t.Fatalf("first pass attempted removal %d times, want 1", engine.attempts)
	}
	if _, err := os.Stat(row.Cwd); err != nil {
		t.Fatalf("uncertain removal did not keep its worktree: %v", err)
	}
	if row.Report != "" {
		t.Fatalf("uncertain removal was noted: %q", row.Report)
	}
	h.server.reviewPass(context.Background())
	if engine.attempts != 2 {
		t.Fatalf("second pass attempted removal %d times, want 2", engine.attempts)
	}
	cleanupGone(t, h, row)
	after, err := h.store.Thread(context.Background(), thread.ID)
	if err != nil || after.Report != "" {
		t.Fatalf("successful retry was noted: %+v, %v", after, err)
	}
}
