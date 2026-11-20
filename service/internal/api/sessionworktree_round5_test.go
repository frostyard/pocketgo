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

// A worktree the request made, removed since with its branch kept, is not
// made again by a retry: it is checked as it is (Codex review of #46,
// round 5).
func TestARetryNeverMakesItsWorktreeAgain(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	h.engine.createErr = errors.New("the durable store is locked")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("the first try did not fail")
	}
	path, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	hostGit(t, h.chairlift, "worktree", "remove", "--force", path)
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("a retry whose worktree went started a session")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) || h.engine.created != 0 || h.adds() != 1 {
		t.Fatalf("the retry: %v, %d sessions, %d adds", err, h.engine.created, h.adds())
	}
}

// A folder an earlier try made and left before its worktree is not filled
// by a retry: another may be in its place (Codex review of #46, round 5).
func TestAFolderAnEarlierTryLeftIsNeverFilled(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	path, branch := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	key := "u:" + h.device + ":r1"
	claim := store.SessionWorktree{Path: path, Branch: branch, Top: hostGit(t, h.chairlift, "rev-parse", "--show-toplevel"), Dest: filepath.Join(parent, filepath.Base(path))}
	if err := h.store.ClaimWorktree(ctx, key, claim); err != nil {
		t.Fatal(err)
	}
	if err := h.store.MarkWorktree(ctx, key, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 || h.engine.created != 0 {
		t.Fatalf("the folder holds %d entries (%v); %d sessions", len(entries), err, h.engine.created)
	}
}

// A request id that made a worktree's session answers no plain create,
// before or after that session is recorded (Codex review of #46, round 5).
func TestAPlainCreateNeverReplaysAWorktreesSession(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	if _, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Cwd: h.work, Model: model})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// A step of the worktree that could not be recorded is pocketd's failure,
// not the request's: internal (Codex review of #46, round 5).
func TestAStepThatCannotBeRecordedIsInternal(t *testing.T) {
	h := newRepoHarness(t)
	h.server.markWorktree = func(context.Context, string, int) error { return errors.New("the work database is locked") }
	_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
}
