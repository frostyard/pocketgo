package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// The guards of the Codex review of #109, round 2 (ADR-0053).

// A checkout's top swapped for another folder between git's listing and
// pocketd's read of a new file refuses the read: the file is read below the
// folder the read began in alone (P1).
func TestATopSwappedDuringAReadRefusesIt(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "new.txt"), "in\n")
	other := filepath.Join(h.work, "other")
	writeFile(t, filepath.Join(other, "new.txt"), "outside\nthe\ncheckout\n")
	swapped := false
	h.server.SetEngine(engineWith{fakeEngine: h.engine, readIn: func(ctx context.Context, root, folder, rel string, max int) (string, []byte, error) {
		if !swapped {
			swapped = true
			if err := os.Rename(dir, dir+".real"); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(other, dir); err != nil {
				t.Error(err)
			}
		}
		return h.engine.ReadIn(ctx, root, folder, rel, max)
	}})
	got, err := h.client.Changes(ctx, id)
	if err == nil {
		t.Fatalf("a read across a swapped top answered %+v", got)
	}
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".real", dir); err != nil {
		t.Fatal(err)
	}
	swapped = false
	diff, err := h.client.ChangesFile(ctx, id, "new.txt")
	if err == nil && strings.Contains(diff.Diff, "outside") {
		t.Fatalf("the diff read another folder: %q", diff.Diff)
	}
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// A new file whose name is no UTF-8 is listed, uncounted and with no diff,
// and the others are read: the list never fails for it (P2).
func TestANameThatIsNoUTF8KeepsTheList(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "caf\xe9.txt"), "latin\n")
	writeFile(t, filepath.Join(dir, "ok.txt"), "a\nb\n")
	got, err := h.client.Changes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 2 || !got.Files[0].NoDiff || !got.Files[0].Uncounted || got.Files[1].Path != "ok.txt" || got.Files[1].Added != 2 || got.Added != 2 {
		t.Fatalf("files %+v, +%d", got.Files, got.Added)
	}
}

// A file's patch is its own change alone, also when a folder takes its name
// as git runs: the folder's contents never join it (P2).
func TestAFilesPatchStaysItsOwnWhenTheFolderChanges(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "config"), "old\n")
	gitIn(t, dir, "add", "config")
	gitIn(t, dir, "commit", "-q", "-m", "config")
	gitIn(t, dir, "update-ref", "refs/heads/main", "HEAD")
	writeFile(t, filepath.Join(dir, "config"), "edited\n")
	h.engine.onGit = func(args []string) {
		if slices.Contains(args, "--src-prefix=a/") {
			// Right before the patch, the file becomes a folder.
			if err := os.Remove(filepath.Join(dir, "config")); err != nil {
				t.Error(err)
			}
			writeFile(t, filepath.Join(dir, "config", "settings"), "new\n")
			gitIn(t, dir, "add", "-A", "config")
		}
	}
	diff, err := h.client.ChangesFile(ctx, id, "config")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff.Diff, "settings") {
		t.Fatalf("config's patch holds the folder's files: %q", diff.Diff)
	}
}

// git that ran and refused the checkout at a later step, as a partial
// clone that lacks what a diff needs, is conflict with git's words, not an
// internal failure (P2).
func TestALaterGitRefusalSaysWhy(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	_, id := repoSession(t, h, "repo")
	h.server.SetEngine(engineEach{engineWith{fakeEngine: h.engine}, func(args []string) bool { return slices.Contains(args, "--numstat") }})
	_, err := h.client.Changes(ctx, id)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if !strings.Contains(err.Error(), "git cannot read this checkout: fatal: missing blob") {
		t.Fatalf("the refusal says %v", err)
	}
}

// engineEach is an engine whose GitEach fails as git refusing the
// checkout for the commands refuse names.
type engineEach struct {
	engineWith
	refuse func(args []string) bool
}

func (e engineEach) GitEach(ctx context.Context, dir string, sep byte, each func(string) error, args ...string) error {
	if e.refuse(args) {
		return fmt.Errorf("git %s: %w: fatal: missing blob", strings.Join(args, " "), &worktree.GitExit{Status: 128})
	}
	return e.engineWith.GitEach(ctx, dir, sep, each, args...)
}

// A new file its owner may run says so, as git would record it (P2).
func TestANewExecutableSaysSo(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	diff, err := h.client.ChangesFile(ctx, id, "run.sh")
	if err != nil || !strings.Contains(diff.Diff, "new file mode 100755\n") {
		t.Fatalf("diff %+v %v", diff, err)
	}
}

// boundedGit is the harness's engine with Git's output bound, as the
// engine's: past it is an error.
func boundedGit(h *harness) engineWith {
	return engineWith{fakeEngine: h.engine, git: func(ctx context.Context, d string, args ...string) (string, error) {
		out, err := h.engine.Git(ctx, d, args...)
		if len(out) > 64<<10 {
			return "", fmt.Errorf("git %s printed more than %d bytes", strings.Join(args, " "), 64<<10)
		}
		return out, err
	}}
}

// The listing that checks a new file's path is read record by record: a
// folder of the path's name holding more new files than one answer holds is
// not_found, not an internal failure (P3).
func TestAFolderOfManyNewFilesIsNoFile(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	long := strings.Repeat("n", 120)
	for i := range 700 {
		writeFile(t, filepath.Join(dir, "d", fmt.Sprintf("%s-%03d.txt", long, i)), "x\n")
	}
	h.server.SetEngine(boundedGit(h))
	_, err := h.client.ChangesFile(ctx, id, "d")
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
}

// A checkout whose top holds a newline in its name is read (P3).
func TestATopWithANewlineIsRead(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "re\npo")
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	got, err := h.client.Changes(ctx, id)
	if err != nil || !got.Repository || len(got.Files) != 1 {
		t.Fatalf("changes %+v %v", got, err)
	}
}
