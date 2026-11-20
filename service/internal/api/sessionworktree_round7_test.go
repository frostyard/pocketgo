package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// A try that stopped after git made its worktree and before it marked it
// leaves a worktree no retry takes: one made again there, unmarked, cannot
// be told from it (Codex review of #46, round 7).
func TestAnUnmarkedWorktreeIsNeverTakenOnARetry(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	path, branch := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	key := "u:" + h.device + ":r1"
	claim := store.SessionWorktree{Path: path, Branch: branch, Top: hostGit(t, h.chairlift, "rev-parse", "--show-toplevel"),
		Dest: filepath.Join(parent, filepath.Base(path)), Target: requestTarget(request)}
	if err := h.store.ClaimWorktree(ctx, key, claim); err != nil {
		t.Fatal(err)
	}
	if err := h.store.MarkWorktree(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	hostGit(t, h.chairlift, "worktree", "add", "-q", "-b", branch, path, "HEAD")
	_, err = h.client.CreateSession(ctx, request)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.engine.created != 0 {
		t.Fatalf("%d sessions", h.engine.created)
	}
}

// The checkout and the worktree's place come from one resolution of the
// folder they share: a link that moves between them makes nothing where it
// points now (Codex review of #46, round 7).
func TestALinkMovedAsThePlaceIsResolvedSplitsNothing(t *testing.T) {
	h := newRepoHarness(t)
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(h.root, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	checkoutAt(t, other, "chairlift")
	h.writeProfile(t, "duncan.md", h.duncan(filepath.Join(link, "chairlift"), false))
	calls := 0
	h.engine.onGit = func([]string) {
		// The second git of the first resolution: the checkout was named.
		if calls++; calls == 2 {
			if err := os.Remove(link); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(other, link); err != nil {
				t.Error(err)
			}
		}
	}
	_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	if err == nil {
		t.Fatal("a session started after its link moved")
	}
	if _, err := os.Lstat(filepath.Join(other, "chairlift--duncan-session-"+proto.WorktreeSuffix("r1"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("something was made where the link points now: %v", err)
	}
}

// A request id that made a worktree answers no CreateSession naming
// another target with it, not even with a worktree: a replay is of the
// request it made it for (Codex review of #46, round 7).
func TestAWorktreesRequestIDAnswersNoOtherTarget(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	first, err := h.client.CreateSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]proto.CreateSession{
		"another repository": {RequestID: "r1", Profile: "duncan", Repository: "frostyard/plain", Worktree: true},
		"another profile":    {RequestID: "r1", Repository: "frostyard/snow", Worktree: true, Model: model},
		"another title":      {RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true, Title: "Other"},
	} {
		_, err := h.client.CreateSession(ctx, other)
		var e *proto.Error
		if !errors.As(err, &e) || e.Status != http.StatusConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != first.ID {
		t.Fatalf("the request again: %+v, %v", again, err)
	}
}
