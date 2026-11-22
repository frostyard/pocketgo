package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/changes"
)

// The guards of the Codex review of #109, round 1 (ADR-0053).

// repoSession is a session in a new repository dir of the harness's work
// folder, with a first commit on main.
func repoSession(t *testing.T, h *harness, name string, init ...string) (string, int64) {
	t.Helper()
	dir := filepath.Join(h.work, name)
	gitIn(t, h.work, append([]string{"init", "-q", "-b", "main"}, append(init, dir)...)...)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	session, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: name, Cwd: dir, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	return dir, session.ID
}

// An attributes file that comes while git reads refuses the answer: the
// read checks again once it read, as a filter it names may have made what
// git showed (P1).
func TestAnAttributesFileThatComesDuringAReadRefusesIt(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	gitIn(t, dir, "add", "a.txt")
	var once sync.Once
	h.engine.onGit = func(args []string) {
		if slices.Contains(args, "--numstat") {
			once.Do(func() { writeFile(t, filepath.Join(dir, ".git", "info", "attributes"), "* filter=made\n") })
		}
	}
	_, err := h.client.Changes(ctx, id)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if err := os.Remove(filepath.Join(dir, ".git", "info", "attributes")); err != nil {
		t.Fatal(err)
	}
	once = sync.Once{}
	_, err = h.client.ChangesFile(ctx, id, "a.txt")
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// A new file is read below the checkout through no link: a folder swapped
// for a link to elsewhere since git listed the file leads the read nowhere,
// for its count and for its diff (P1).
func TestANewFileIsReadThroughNoLink(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	outside := filepath.Join(h.work, "outside")
	writeFile(t, filepath.Join(outside, "secret.txt"), "outside\nthe\ncheckout\n")
	writeFile(t, filepath.Join(dir, "sub", "secret.txt"), "in\n")
	swapped := false
	swap := func() {
		if swapped {
			return
		}
		swapped = true
		if err := os.Rename(filepath.Join(dir, "sub"), filepath.Join(h.work, "sub.real")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
			t.Error(err)
		}
	}
	// The swap comes right after git listed the new files, before what
	// reads them next: git's own read of a new file or pocketd's.
	listed := false
	h.engine.onGit = func(args []string) {
		if listed {
			swap()
		}
		listed = listed || slices.Contains(args, "ls-files")
	}
	h.server.SetEngine(engineWith{fakeEngine: h.engine, readIn: func(ctx context.Context, root, rel string, max int) (string, []byte, error) {
		swap()
		return h.engine.ReadIn(ctx, root, rel, max)
	}})
	got, err := h.client.Changes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "sub/secret.txt" || !got.Files[0].Uncounted || got.Added != 0 {
		t.Fatalf("the swapped file was counted: %+v, +%d", got.Files, got.Added)
	}
	// Put it back, list it again, and swap it before its diff.
	if err := os.Remove(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(h.work, "sub.real"), filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	swapped, listed = false, false
	diff, err := h.client.ChangesFile(ctx, id, "sub/secret.txt")
	if err == nil && strings.Contains(diff.Diff, "outside") {
		t.Fatalf("the diff read outside the checkout: %q", diff.Diff)
	}
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
}

// A submodule's change shows by its commits, whatever diff.submodule the
// checkout sets (P2).
func TestASubmoduleShowsByItsCommits(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	// A submodule checked out at the commit its gitlink names.
	sub := filepath.Join(dir, "sub")
	gitIn(t, dir, "init", "-q", "-b", "main", sub)
	gitIn(t, sub, "commit", "-q", "--allow-empty", "-m", "sub")
	sha := gitIn(t, sub, "rev-parse", "HEAD")
	gitIn(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+sha+",sub")
	gitIn(t, dir, "config", "diff.submodule", "log")
	diff, err := h.client.ChangesFile(ctx, id, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Diff, "+Subproject commit "+sha+"\n") || strings.Contains(diff.Diff, "Submodule sub") {
		t.Fatalf("the submodule's diff %q", diff.Diff)
	}
}

// A git older than changes.MinGit, which takes no --no-lazy-fetch, reads
// nothing: a partial clone's read could fetch what it lacks into the
// checkout (P2).
func TestAnOlderGitReadsNoChanges(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	var calls gitCalls
	h.server.SetEngine(engineWith{fakeEngine: h.engine, git: func(ctx context.Context, d string, args ...string) (string, error) {
		calls.see(args)
		if args[len(args)-1] == "version" {
			return "git version 2.44.1", nil
		}
		return h.engine.Git(ctx, d, args...)
	}})
	_, err := h.client.Changes(ctx, id)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if err == nil || !strings.Contains(err.Error(), "git 2.45 or later") {
		t.Fatalf("the refusal says %v", err)
	}
	for _, args := range calls.all() {
		if slices.Contains(args, "diff") || slices.Contains(args, "ls-files") {
			t.Fatalf("an older git read the checkout: %q", args)
		}
	}
}

// A file removed from the index but kept in the folder is one change, the
// one a commit would make: its removal, and its diff says so (P2).
func TestAFileRemovedFromTheIndexIsOneChange(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "a.txt"), "a\nb\n")
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "a")
	gitIn(t, dir, "checkout", "-q", "-b", "feature", "HEAD")
	gitIn(t, dir, "rm", "-q", "--cached", "a.txt")
	got, err := h.client.Changes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.More != 0 || got.Files[0].Path != "a.txt" || got.Files[0].New || got.Files[0].Removed != 2 {
		t.Fatalf("files %+v, %d more", got.Files, got.More)
	}
	diff, err := h.client.ChangesFile(ctx, id, "a.txt")
	if err != nil || !strings.Contains(diff.Diff, "deleted file mode") {
		t.Fatalf("diff %+v %v", diff, err)
	}
}

// A file a commit edited and the folder then renamed is both committed and
// not: its change is marked by either end of each part (P2).
func TestAnEditCommittedThenRenamedIsBoth(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	body := strings.Repeat("a line of the file that stays\n", 20)
	writeFile(t, filepath.Join(dir, "old.go"), body)
	gitIn(t, dir, "add", "old.go")
	gitIn(t, dir, "commit", "-q", "-m", "old")
	gitIn(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(dir, "old.go"), body+"one more\n")
	gitIn(t, dir, "commit", "-q", "-am", "edit")
	gitIn(t, dir, "mv", "old.go", "new.go")
	got, err := h.client.Changes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := []proto.ChangedFile{{Path: "new.go", OldPath: "old.go", Added: 1, Committed: true, Uncommitted: true}}
	if !slices.Equal(got.Files, want) {
		t.Fatalf("files %+v", got.Files)
	}
}

// A file's patch is its own change alone: a folder that took its name, and
// what it holds, stays out (P2).
func TestAFilesPatchLeavesOutAFolderOfItsName(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "config"), "old\n")
	gitIn(t, dir, "add", "config")
	gitIn(t, dir, "commit", "-q", "-m", "config")
	gitIn(t, dir, "checkout", "-q", "-b", "feature", "HEAD")
	gitIn(t, dir, "update-ref", "refs/heads/main", "HEAD")
	gitIn(t, dir, "rm", "-q", "config")
	writeFile(t, filepath.Join(dir, "config", "settings"), "new\n")
	gitIn(t, dir, "add", "config/settings")
	diff, err := h.client.ChangesFile(ctx, id, "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Diff, "deleted file mode") || strings.Contains(diff.Diff, "settings") {
		t.Fatalf("config's patch %q", diff.Diff)
	}
}

// The first proto.MaxCountedNew new files are counted though the list is
// full: their lines are in the totals (P2).
func TestNewFilesAreCountedPastTheList(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	for i := range proto.MaxChangedFiles {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("t%03d.txt", i)), "x\n")
	}
	gitIn(t, dir, "add", ".")
	writeFile(t, filepath.Join(dir, "zz-new.txt"), "a\nb\n")
	got, err := h.client.Changes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != proto.MaxChangedFiles || got.More != 1 || got.Added != proto.MaxChangedFiles+2 {
		t.Fatalf("%d files, %d more, +%d", len(got.Files), got.More, got.Added)
	}
}

// A folder with a checkout git cannot read says why, not that it is in
// none (P2).
func TestACheckoutGitCannotReadSaysWhy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := filepath.Join(h.work, "broken")
	writeFile(t, filepath.Join(dir, ".git"), "gitdir: "+filepath.Join(h.work, "gone")+"\n")
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: dir, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.client.Changes(ctx, session.ID)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if !strings.Contains(err.Error(), "git cannot read the checkout of "+dir) {
		t.Fatalf("the refusal says %v", err)
	}
}

// A SHA-256 repository with no commit yet counts from its own empty tree
// (P2).
func TestAnUnbornSHA256RepositoryCountsFromItsEmptyTree(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir := filepath.Join(h.work, "sha256")
	gitIn(t, h.work, "init", "-q", "-b", "main", "--object-format=sha256", dir)
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n")
	gitIn(t, dir, "add", "a.go")
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: dir, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.client.Changes(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MergeBase != changes.EmptyTree256 || len(got.Files) != 1 || got.Files[0].Added != 1 {
		t.Fatalf("changes %+v", got)
	}
}

// A diff of bytes that are no UTF-8 fits its bound as the client decodes
// it, each such byte a U+FFFD of three, and says it is cut (P2).
func TestADiffOfBytesThatAreNoUTF8FitsItsBound(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "latin.txt"), "start\n")
	gitIn(t, dir, "add", "latin.txt")
	gitIn(t, dir, "commit", "-q", "-m", "latin")
	gitIn(t, dir, "update-ref", "refs/heads/main", "HEAD")
	writeFile(t, filepath.Join(dir, "latin.txt"), strings.Repeat("\xe9\xe8\xe0\xe7\xf9\xe2\xea\xee\xf4\xfb\n", proto.MaxFileDiff/8))
	diff, err := h.client.ChangesFile(ctx, id, "latin.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Diff) > proto.MaxFileDiff || !utf8.ValidString(diff.Diff) || !diff.Cut || !strings.HasSuffix(diff.Diff, "\n") {
		t.Fatalf("a diff of %d bytes, cut %v", len(diff.Diff), diff.Cut)
	}
}

// A new file named - is itself, not standard input (P3).
func TestANewFileNamedDashIsItself(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "-"), "dash\n")
	got, err := h.client.Changes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "-" || got.Files[0].Added != 1 {
		t.Fatalf("files %+v", got.Files)
	}
	diff, err := h.client.ChangesFile(ctx, id, "-")
	if err != nil || !strings.Contains(diff.Diff, "+dash\n") {
		t.Fatalf("diff %+v %v", diff, err)
	}
}
