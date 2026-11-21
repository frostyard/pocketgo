package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The agent works at its worktree's real place, never through the link
// the profile names its checkout by, which may move (Codex review of #46,
// round 6).
func TestTheSessionWorksAtItsWorktreesRealPlace(t *testing.T) {
	h := newRepoHarness(t)
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(h.root, link); err != nil {
		t.Fatal(err)
	}
	h.writeProfile(t, "duncan.md", h.duncan(filepath.Join(link, "chairlift"), false))
	if _, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(h.root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "chairlift--duncan-session-"+proto.WorktreeSuffix("r1"))
	if spec := h.engine.specs[len(h.engine.specs)-1]; spec.Cwd != want {
		t.Fatalf("the agent works in %s, want %s", spec.Cwd, want)
	}
}

// A worktree made again at the request's place, on its branch, after its
// first try made one and failed, is not the request's: its retry is
// conflict, by the mark the request's worktree holds (Codex review of #46,
// round 6).
func TestAWorktreeMadeAgainAtItsPlaceIsNotTheRequests(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	h.engine.createErr = errors.New("the durable store is locked")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("the first try did not fail")
	}
	path, branch := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	hostGit(t, h.chairlift, "worktree", "remove", "--force", path)
	hostGit(t, h.chairlift, "worktree", "add", "-q", path, branch)
	_, err := h.client.CreateSession(ctx, request)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.engine.created != 0 {
		t.Fatalf("%d sessions", h.engine.created)
	}
}

// A request id that started a session without a worktree answers no
// CreateSession with one: the client asked for a worktree, and would get
// the checkout (Codex review of #46, round 6).
func TestAWorktreeCreateNeverReplaysAPlainSession(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	if _, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift"}); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.adds() != 0 {
		t.Fatalf("%d worktrees", h.adds())
	}
}
