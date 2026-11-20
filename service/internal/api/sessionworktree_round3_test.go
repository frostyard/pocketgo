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

// Another that makes the worktree's branch while this one's git runs wins
// it: this request's git fails, and no try of the request ever adopts the
// other's branch (Codex review of #46, round 3). Another pocketd cannot
// fill this request's folder, which it made alone (round 4), so the race
// is for the branch.
func TestAWorktreeAnotherPocketdMadeInARaceIsNeverAdopted(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	_, branch := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	raced := false
	h.engine.onGit = func(args []string) {
		if len(args) > 2 && args[0] == "worktree" && args[1] == "add" && !raced {
			raced = true
			hostGit(t, h.chairlift, "branch", branch)
		}
	}
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	// Conflict at once, as on every try after it (round 4).
	for try := range 3 {
		_, err := h.client.CreateSession(ctx, request)
		asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
		if h.engine.created != 0 {
			t.Fatalf("retry %d adopted the other pocketd's worktree", try)
		}
	}
}

// A retry after a link above the checkout moved to another checkout makes
// no second worktree there: the request's worktree is of the checkout git
// named when it was made (Codex review of #46, round 3).
func TestARetryAfterALinkMovedMakesNoSecondWorktree(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(h.root, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	checkoutAt(t, other, "chairlift")
	h.writeProfile(t, "duncan.md", h.duncan(filepath.Join(link, "chairlift"), false))
	h.engine.createErr = errors.New("the durable store is locked")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("the first try did not fail")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.CreateSession(ctx, request)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.adds() != 1 || h.engine.created != 0 {
		t.Fatalf("the retry made %d worktrees in all and %d sessions", h.adds(), h.engine.created)
	}
	second, _ := proto.SessionWorktree(filepath.Join(other, "chairlift"), "duncan", "", "r1")
	if _, err := os.Lstat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a second worktree at %s: %v", second, err)
	}
}

// A request whose worktree was made, and whose session did not start, keeps
// its id: another endpoint cannot take it, and the create's retry finishes
// its worktree (Codex review of #46, round 3).
func TestAWorktreesRequestIDStaysTheCreates(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "p", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.createErr = errors.New("the durable store is locked")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("the first try did not fail")
	}
	_, err = h.client.Send(ctx, plain.ID, proto.SendMessage{RequestID: "r1", Text: "hello"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	s, err := h.client.CreateSession(ctx, request)
	path, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	if err != nil || s.Cwd != path || h.adds() != 1 {
		t.Fatalf("the create's retry: %+v, %v, %d adds", s, err, h.adds())
	}
}
