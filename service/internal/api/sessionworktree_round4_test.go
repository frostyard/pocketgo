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

// git makes the worktree in the checkout the request claimed, never in the
// one a link above it names once it moved, and the session starts only
// while the link still names that checkout (Codex review of #46, round 4).
func TestAWorktreeIsMadeInTheCheckoutItClaimed(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(h.root, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	otherCheckout := checkoutAt(t, other, "chairlift")
	h.writeProfile(t, "duncan.md", h.duncan(filepath.Join(link, "chairlift"), false))
	_, branch := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	moved := false
	h.engine.onGit = func(args []string) {
		// Once claimed, right before git looks for the branch: the link
		// moves to another checkout.
		if len(args) > 0 && args[0] == "for-each-ref" && !moved {
			moved = true
			if err := os.Remove(link); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(other, link); err != nil {
				t.Error(err)
			}
		}
	}
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.engine.created != 0 {
		t.Fatalf("%d sessions", h.engine.created)
	}
	if refs := hostGit(t, otherCheckout, "for-each-ref", "refs/heads/"+branch); refs != "" {
		t.Fatalf("git made %s in the checkout the link moved to", refs)
	}
	if refs := hostGit(t, h.chairlift, "for-each-ref", "refs/heads/"+branch); refs == "" {
		t.Fatal("git did not make the branch in the checkout the request claimed")
	}
}

// The same request id without the worktree, once the worktree was made, is
// another request: conflict, and the worktree's retry still finishes it
// (Codex review of #46, round 4).
func TestAPlainCreateCannotTakeAWorktreesRequestID(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	h.engine.createErr = errors.New("the durable store is locked")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("the first try did not fail")
	}
	for name, plain := range map[string]proto.CreateSession{
		"a folder":                  {RequestID: "r1", Cwd: h.work, Model: model},
		"the repository's checkout": {RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift"},
		"a standing profile":        {RequestID: "r1", Profile: "odrade"},
	} {
		_, err := h.client.CreateSession(ctx, plain)
		var e *proto.Error
		if !errors.As(err, &e) || e.Status != http.StatusConflict {
			t.Fatalf("%s with the worktree's id: %v", name, err)
		}
	}
	if h.engine.created != 0 {
		t.Fatalf("%d sessions", h.engine.created)
	}
	s, err := h.client.CreateSession(ctx, request)
	path, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	if err != nil || s.Cwd != path {
		t.Fatalf("the worktree's retry: %+v, %v", s, err)
	}
}

// The worktree's last check, which may take a while, comes before the
// profile's last read: a profile made private while the worktree is
// checked starts nothing (Codex review of #46, round 4).
func TestTheProfileIsReadAfterTheWorktreesLastCheck(t *testing.T) {
	h := newRepoHarness(t)
	checking := false
	h.server.afterSessionWorktree = func() { checking = true }
	h.engine.onGit = func([]string) {
		if checking {
			checking = false
			h.writeProfile(t, "duncan.md", h.duncan(h.chairlift, true))
		}
	}
	_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if h.engine.created != 0 {
		t.Fatalf("%d sessions", h.engine.created)
	}
}

// The worktree's folder is the request's own before git runs: another
// that would make it then finds it made, and an empty folder another made
// before is never filled (Codex review of #46, round 4).
func TestAnEmptyFolderTheRequestDidNotMakeIsNeverTaken(t *testing.T) {
	h := newRepoHarness(t)
	path, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	var raced error
	h.engine.onGit = func(args []string) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			raced = os.Mkdir(path, 0o700)
		}
	}
	s, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	if !errors.Is(raced, os.ErrExist) {
		t.Fatalf("another made the folder right before git: %v", raced)
	}
	if err != nil || s.Cwd != path {
		t.Fatalf("the request in its own folder: %+v, %v", s, err)
	}
	h.engine.onGit = nil
	path2, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r2")
	if err := os.Mkdir(path2, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r2", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if entries, err := os.ReadDir(path2); err != nil || len(entries) != 0 || h.engine.created != 1 {
		t.Fatalf("the folder holds %d entries (%v); %d sessions", len(entries), err, h.engine.created)
	}
}
