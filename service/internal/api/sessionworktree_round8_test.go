package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// A retry whose checkout moved to a folder that is no checkout is told it
// made its worktree already, conflict, before that folder is looked at
// (Codex review of #46, round 8).
func TestARetryToAFolderThatIsNoCheckoutIsTheMovedConflict(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	h.engine.createErr = errors.New("the durable store is locked")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("the first try did not fail")
	}
	h.writeProfile(t, "duncan.md", h.duncan(h.plain, false))
	_, err := h.client.CreateSession(ctx, request)
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// What a worktree's request named is told apart field by field, whatever a
// field holds, a NUL included (Codex review of #46, round 8).
func TestARequestsTargetKeepsItsFieldsApart(t *testing.T) {
	a := proto.CreateSession{Profile: "duncan", Repository: "frostyard/chairlift", Cwd: "/srv/a\x00b", Title: "c"}
	b := proto.CreateSession{Profile: "duncan", Repository: "frostyard/chairlift", Cwd: "/srv/a", Title: "b\x00c"}
	if requestTarget(a) == requestTarget(b) {
		t.Fatal("two targets that differ in their fields' bounds are one")
	}
	same := a
	if requestTarget(a) != requestTarget(same) {
		t.Fatal("a target is not its own")
	}
}
