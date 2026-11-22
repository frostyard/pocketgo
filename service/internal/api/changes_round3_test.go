package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The guards of the Codex review of #109, round 3 (ADR-0053).

// A rename across a folder's boundary shows as the rename, both ways, and
// a sibling the folder holds stays out of it (P2).
func TestARenameAcrossAFolderShowsAsTheRename(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	body := strings.Repeat("a line that stays the same\n", 10)
	writeFile(t, filepath.Join(dir, "config"), body)
	gitIn(t, dir, "add", "config")
	gitIn(t, dir, "commit", "-q", "-m", "config")
	gitIn(t, dir, "update-ref", "refs/heads/main", "HEAD")
	// The file moves into a folder of its name, beside a new sibling.
	gitIn(t, dir, "rm", "-q", "--cached", "config")
	if err := os.Rename(filepath.Join(dir, "config"), filepath.Join(h.work, "moving")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(h.work, "moving"), filepath.Join(dir, "config", "settings")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "config", "other"), "a sibling\n")
	gitIn(t, dir, "add", "config")
	diff, err := h.client.ChangesFile(ctx, id, "config/settings")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Diff, "rename from config\nrename to config/settings\n") || strings.Contains(diff.Diff, "sibling") ||
		strings.Contains(diff.Diff, "deleted file mode") {
		t.Fatalf("into a folder: %q", diff.Diff)
	}

	// And back: a file in a folder moves to the folder's name.
	gitIn(t, dir, "commit", "-q", "-m", "moved")
	gitIn(t, dir, "update-ref", "refs/heads/main", "HEAD")
	gitIn(t, dir, "rm", "-q", "-r", "--cached", "config")
	if err := os.Rename(filepath.Join(dir, "config", "settings"), filepath.Join(h.work, "back")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(h.work, "back"), filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "config")
	diff, err = h.client.ChangesFile(ctx, id, "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Diff, "rename from config/settings\nrename to config\n") || strings.Contains(diff.Diff, "sibling") ||
		strings.Contains(diff.Diff, "new file mode") {
		t.Fatalf("out of a folder: %q", diff.Diff)
	}
}

// A HEAD that names a commit git cannot read is a checkout git cannot read,
// not a repository with no commit yet whose files all count as new (P2).
func TestAnUnreadableHeadIsNoNewRepository(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "a")
	// git refuses to write a ref to a missing object: the file is written
	// as a damaged checkout leaves it.
	if err := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(strings.Repeat("1", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.Changes(ctx, id)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// The base's refs are kept by their exact names, line by line: refs below a
// candidate's name, past one answer's bound, take no read down (P3).
func TestRefsBelowACandidateTakeNoReadDown(t *testing.T) {
	needsChangesGit(t)
	h := newHarness(t)
	ctx := context.Background()
	dir, id := repoSession(t, h, "repo")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	var packed strings.Builder
	packed.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for i := range 1200 {
		fmt.Fprintf(&packed, "%s refs/remotes/origin/master/%s-%04d\n", head, strings.Repeat("x", 40), i)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "packed-refs"), []byte(packed.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	h.server.SetEngine(boundedGit(h))
	got, err := h.client.Changes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != "main" || len(got.Files) != 1 {
		t.Fatalf("changes %+v", got)
	}
}
