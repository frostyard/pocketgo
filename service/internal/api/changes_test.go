package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/changes"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// gitIn runs git in dir as a test's fixture does, outside any sandbox,
// and returns its output, trimmed.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitCalls records every git call the engine runs.
type gitCalls struct {
	mu    sync.Mutex
	calls [][]string
}

func (g *gitCalls) see(args []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, slices.Clone(args))
}

func (g *gitCalls) all() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// A thread's session lists what it changed in its worktree since it left
// its base branch, committed and not yet, new files included, and the
// owner reads each file's diff (ADR-0053, rules 63 and 64): the issue's
// Done when, through the protocol.
func TestAThreadsChangesListWhatItChangedAndEachFilesDiff(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	base := gitIn(t, h.checkout, "rev-parse", "main")
	wt := thread.Cwd
	writeFile(t, filepath.Join(wt, "a.txt"), "one\ntwo\nthree\n")
	writeFile(t, filepath.Join(wt, "old.txt"), "keep\nthis\ntext\nhere\n")
	gitIn(t, wt, "add", "a.txt", "old.txt")
	gitIn(t, wt, "commit", "-q", "-m", "add a")
	gitIn(t, wt, "mv", "old.txt", "renamed.txt")
	gitIn(t, wt, "commit", "-q", "-m", "rename")
	writeFile(t, filepath.Join(wt, "a.txt"), "one\ntwo\nthree\nfour\n")
	writeFile(t, filepath.Join(wt, "b.txt"), "staged\nfile\n")
	gitIn(t, wt, "add", "b.txt")
	writeFile(t, filepath.Join(wt, "new file.md"), "# New\n\nnot\ntracked\n")
	writeFile(t, filepath.Join(wt, "logo.png"), "\x89PNG\x00\x00binary")

	got, err := h.client.Changes(ctx, thread.Session)
	if err != nil {
		t.Fatal(err)
	}
	top, _ := filepath.EvalSymlinks(wt)
	if !got.Repository || got.Top != top || got.Branch != thread.Branch || got.Base != "main" || got.BaseRule != proto.ChangesBaseDefault ||
		got.MergeBase != base || got.Head != gitIn(t, wt, "rev-parse", "HEAD") {
		t.Fatalf("changes %+v", got)
	}
	want := []proto.ChangedFile{
		{Path: "a.txt", Added: 4, Committed: true, Uncommitted: true},
		{Path: "b.txt", Added: 2, Uncommitted: true},
		{Path: "renamed.txt", Added: 4, Committed: true},
		{Path: "logo.png", Binary: true, New: true, Uncommitted: true},
		{Path: "new file.md", Added: 4, New: true, Uncommitted: true},
	}
	if !slices.Equal(got.Files, want) || got.Added != 14 || got.Removed != 0 || got.More != 0 {
		t.Fatalf("files %+v, +%d -%d, %d more", got.Files, got.Added, got.Removed, got.More)
	}

	diff, err := h.client.ChangesFile(ctx, thread.Session, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if diff.Cut || diff.MergeBase != base || !strings.Contains(diff.Diff, "--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1,4 @@\n+one\n+two\n+three\n+four\n") {
		t.Fatalf("a.txt's diff %+v", diff)
	}
	fresh, err := h.client.ChangesFile(ctx, thread.Session, "new file.md")
	if err != nil {
		t.Fatal(err)
	}
	// git ends a path with a space by a tab, as GNU diff does.
	if !strings.Contains(fresh.Diff, "new file mode") || !strings.Contains(fresh.Diff, "+++ b/new file.md\t\n@@ -0,0 +1,4 @@\n+# New\n+\n+not\n+tracked\n") {
		t.Fatalf("the new file's diff %+v", fresh)
	}
	if bin, err := h.client.ChangesFile(ctx, thread.Session, "logo.png"); err != nil || !strings.Contains(bin.Diff, "Binary files") {
		t.Fatalf("the binary file's diff %+v %v", bin, err)
	}
	// Committed changes of a file the folder no longer changes are a
	// tracked file's diff from the merge base too.
	writeFile(t, filepath.Join(h.checkout, "outside.txt"), "not the session's\n")
	for path, status := range map[string]int{"unchanged.txt": 404, "outside.txt": 404, "../example/outside.txt": 400, "/etc/passwd": 400, "": 400} {
		_, err := h.client.ChangesFile(ctx, thread.Session, path)
		asError(t, err, status, map[int]proto.ErrorCode{404: proto.ErrorCodeNotFound, 400: proto.ErrorCodeBadRequest}[status])
	}
	if _, err := h.client.Changes(ctx, 999); err == nil {
		t.Fatal("no session's changes were read")
	} else {
		asError(t, err, 404, proto.ErrorCodeNotFound)
	}
}

// The base of a thread with a pull request is the pull request's base
// branch, at its merge base with the session's HEAD (ADR-0053).
func TestAThreadsPullRequestGivesItsBase(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	wt := thread.Cwd
	writeFile(t, filepath.Join(wt, "x.txt"), "x\n")
	gitIn(t, wt, "add", "x.txt")
	gitIn(t, wt, "commit", "-q", "-m", "x")
	// develop holds x.txt already: the pull request's change is what came
	// after it.
	gitIn(t, h.checkout, "branch", "develop", thread.Branch)
	writeFile(t, filepath.Join(wt, "y.txt"), "y\ny\n")
	gitIn(t, wt, "add", "y.txt")
	gitIn(t, wt, "commit", "-q", "-m", "y")
	row := store.PullRow{Repository: "frostyard/example", Number: 3, Title: "Y", URL: "u", State: store.PullOpen,
		Head: thread.Branch, HeadSHA: strings.Repeat("a", 40), Base: "develop", CheckedAt: h.clock.Now()}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "k", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{row},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	got, err := h.client.Changes(ctx, thread.Session)
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != "develop" || got.BaseRule != proto.ChangesBasePull || got.MergeBase != gitIn(t, h.checkout, "rev-parse", "develop") ||
		len(got.Files) != 1 || got.Files[0].Path != "y.txt" || !got.Files[0].Committed {
		t.Fatalf("changes %+v", got)
	}
}

// A session whose folder is in no git checkout answers no repository, and
// no file of it is read (rule 63).
func TestASessionOutsideACheckoutHasNoRepository(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.client.Changes(ctx, session.ID)
	if err != nil || got.Repository || got.SessionID != session.ID || got.Files == nil || len(got.Files) != 0 {
		t.Fatalf("changes %+v %v", got, err)
	}
	_, err = h.client.ChangesFile(ctx, session.ID, "a.txt")
	asError(t, err, 404, proto.ErrorCodeNotFound)
}

// A repository with no commit yet counts what the folder holds from
// nothing: no base, staged and new files uncommitted.
func TestARepositoryWithNoCommitCountsFromNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := filepath.Join(h.work, "fresh")
	gitIn(t, h.work, "init", "-q", "-b", "main", dir)
	writeFile(t, filepath.Join(dir, "staged.go"), "package x\n")
	gitIn(t, dir, "add", "staged.go")
	writeFile(t, filepath.Join(dir, "loose.go"), "package y\n\n")
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: dir, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.client.Changes(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []proto.ChangedFile{{Path: "staged.go", Added: 1, Uncommitted: true}, {Path: "loose.go", Added: 2, New: true, Uncommitted: true}}
	if got.Head != "" || got.BaseRule != proto.ChangesBaseHead || got.MergeBase != changes.EmptyTree || !slices.Equal(got.Files, want) {
		t.Fatalf("changes %+v", got)
	}
	if diff, err := h.client.ChangesFile(ctx, session.ID, "staged.go"); err != nil || !strings.Contains(diff.Diff, "+package x\n") {
		t.Fatalf("diff %+v %v", diff, err)
	}
}

// A repository inside the checkout, which git lists as its folder, is a
// new entry pocketd does not count, and the read goes on.
func TestARepositoryInsideTheCheckoutIsListedUncounted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := filepath.Join(h.work, "repo")
	gitIn(t, h.work, "init", "-q", "-b", "main", dir)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	gitIn(t, dir, "init", "-q", "-b", "main", filepath.Join(dir, "vendor", "inner"))
	writeFile(t, filepath.Join(dir, "vendor", "inner", "x.go"), "package x\n")
	writeFile(t, filepath.Join(dir, "z.txt"), "z\n")
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: dir, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.client.Changes(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []proto.ChangedFile{{Path: "vendor/inner/", New: true, Uncommitted: true, Uncounted: true}, {Path: "z.txt", Added: 1, New: true, Uncommitted: true}}
	if !slices.Equal(got.Files, want) || got.Added != 1 {
		t.Fatalf("files %+v, +%d", got.Files, got.Added)
	}
}

// A diff longer than proto.MaxFileDiff comes cut after its last whole
// line, and says so (ADR-0053).
func TestALongDiffIsCutAtAWholeLineAndSaysSo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := filepath.Join(h.work, "repo")
	gitIn(t, h.work, "init", "-q", "-b", "main", dir)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	var long strings.Builder
	for i := 0; long.Len() < 2*proto.MaxFileDiff; i++ {
		fmt.Fprintf(&long, "line %d of a file longer than one answer\n", i)
	}
	writeFile(t, filepath.Join(dir, "long.txt"), long.String())
	gitIn(t, dir, "add", "long.txt")
	writeFile(t, filepath.Join(dir, "new-long.txt"), long.String())
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: dir, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"long.txt", "new-long.txt"} {
		diff, err := h.client.ChangesFile(ctx, session.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		if !diff.Cut || len(diff.Diff) > proto.MaxFileDiff || len(diff.Diff) < proto.MaxFileDiff/2 || !strings.HasSuffix(diff.Diff, " answer\n") {
			t.Fatalf("%s: cut %v, %d bytes, ends %q", path, diff.Cut, len(diff.Diff), diff.Diff[max(0, len(diff.Diff)-40):])
		}
	}
}

// The list holds at most proto.MaxChangedFiles files and counts the rest,
// and counts the lines of at most proto.MaxCountedNew new files.
func TestTheChangesListIsBounded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := filepath.Join(h.work, "repo")
	gitIn(t, h.work, "init", "-q", "-b", "main", dir)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	for i := range proto.MaxChangedFiles + 5 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), "a\nb\n")
	}
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: dir, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.client.Changes(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != proto.MaxChangedFiles || got.More != 5 || got.Added != 2*proto.MaxCountedNew {
		t.Fatalf("%d files, %d more, +%d", len(got.Files), got.More, got.Added)
	}
	for i, f := range got.Files {
		if counted := i < proto.MaxCountedNew; f.Uncounted == counted || (counted && f.Added != 2) {
			t.Fatalf("file %d: %+v", i, f)
		}
	}
}

// Every git a read runs carries the isolation options, a checkout whose
// attributes no option turns off is refused before any read, and no
// command of the read writes the checkout (ADR-0053).
func TestChangesRunGitIsolatedAndRefuseWhatItCannotIsolate(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	writeFile(t, filepath.Join(thread.Cwd, "a.txt"), "a\n")
	writeFile(t, filepath.Join(thread.Cwd, "b.txt"), "b\n")
	gitIn(t, thread.Cwd, "add", "b.txt")
	// A hook and an fsmonitor the checkout names never run.
	marker := filepath.Join(h.work, "ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	writeFile(t, filepath.Join(h.checkout, ".git", "hooks", "fsmonitor"), hook)
	if err := os.Chmod(filepath.Join(h.checkout, ".git", "hooks", "fsmonitor"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, h.checkout, "config", "core.fsmonitor", filepath.Join(h.checkout, ".git", "hooks", "fsmonitor"))
	gitIn(t, h.checkout, "config", "diff.external", filepath.Join(h.checkout, ".git", "hooks", "fsmonitor"))
	// The worktree's own git folder and index: a write of the index goes
	// through a lock renamed into that folder, which changes both.
	gitDir := gitIn(t, thread.Cwd, "rev-parse", "--absolute-git-dir")
	stamp := func() string {
		dir, err1 := os.Stat(gitDir)
		index, err2 := os.Stat(filepath.Join(gitDir, "index"))
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		return dir.ModTime().String() + " " + index.ModTime().String()
	}
	before := stamp()
	var calls gitCalls
	h.engine.onGit = calls.see
	if _, err := h.client.Changes(ctx, thread.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ChangesFile(ctx, thread.Session, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ChangesFile(ctx, thread.Session, "b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checkout's fsmonitor or external diff ran")
	}
	prefix := team.ReviewGitOptions
	seen := calls.all()
	if len(seen) == 0 {
		t.Fatal("no git ran")
	}
	for _, args := range seen {
		if !slices.Equal(args[:len(prefix)], prefix) {
			t.Errorf("git %q runs without the isolation options", args)
		}
	}
	if after := stamp(); after != before {
		t.Errorf("a read wrote the worktree's index: %s, then %s", before, after)
	}
	// Attributes git reads whatever a read sets refuse the read, before
	// any file of the checkout is listed.
	writeFile(t, filepath.Join(h.checkout, ".git", "info", "attributes"), "* filter=x\n")
	calls = gitCalls{}
	_, err := h.client.Changes(ctx, thread.Session)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.ChangesFile(ctx, thread.Session, "a.txt")
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	for _, args := range calls.all() {
		if slices.Contains(args, "diff") || slices.Contains(args, "ls-files") {
			t.Errorf("git %q read the checkout after the refusal", args)
		}
	}
}

// Reads run at most maxChangesReads at once: one that finds every place
// taken waits, and ends with its request, running no git.
func TestChangesReadsWaitForAPlace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if cap(h.server.changesSlots) != maxChangesReads {
		t.Fatalf("%d places", cap(h.server.changesSlots))
	}
	for range maxChangesReads {
		h.server.changesSlots <- struct{}{}
	}
	var calls gitCalls
	h.engine.onGit = calls.see
	waiting, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := h.client.Changes(waiting, session.ID)
		done <- err
	}()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a read ran with every place taken")
	}
	if len(calls.all()) != 0 {
		t.Fatalf("git ran: %q", calls.all())
	}
	for range maxChangesReads {
		<-h.server.changesSlots
	}
	if _, err := h.client.Changes(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
}
