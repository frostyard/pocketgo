package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// A body with no title, or a null one, renames nothing and keeps its
// requestId free: only a string, "" for the default, renames (Codex
// review of #118, round 1).
func TestARenameNeedsItsTitle(t *testing.T) {
	h := newHarness(t)
	h.writeProfiles(map[string]string{"chani.md": chani})
	ctx := context.Background()
	mine, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Profile: "chani", Title: "Notes"})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + h.token, proto.HeaderProtocol: strconv.Itoa(proto.Version), "Content-Type": "application/json"}
	for _, body := range []string{`{"requestId":"r1"}`, `{"requestId":"r1","title":null}`} {
		response, e := h.raw("POST", proto.Path(proto.PathTitle, mine.ID), body, headers)
		if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest || !strings.HasPrefix(e.Message, "title is required") {
			t.Fatalf("%s: %d %+v", body, response.StatusCode, e)
		}
	}
	if row, _ := h.store.Session(ctx, mine.ID); row.Title != "Notes" {
		t.Fatalf("a body with no title renamed the session %q", row.Title)
	}
	if got, err := h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r1", Title: "Garden plan"}); err != nil || got.Title != "Garden plan" {
		t.Fatalf("the requestId with its title: %+v, %v", got, err)
	}
}

// A private profile's standing session is renamed whoever opened it: one a
// schedule opened (created_by schedule) is no session pocketd started for
// the team's work, and New Session on the profile opens it with its new
// title (ADR-0058; Codex review of #118, round 1).
func TestAPrivateStandingSessionAScheduleOpenedIsRenamed(t *testing.T) {
	h := newHarness(t)
	h.writeProfiles(map[string]string{"chani.md": "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\nsession: standing\n---\n# Chani\n"})
	ctx := context.Background()
	id, why, err := h.server.openStandingFor(ctx, "chani", false)
	if err != nil || why != "" {
		t.Fatalf("open: %d %q %v", id, why, err)
	}
	if row, err := h.store.Session(ctx, id); err != nil || row.CreatedBy != standingCreator || !row.Standing {
		t.Fatalf("the standing session %+v, %v", row, err)
	}
	renamed, err := h.client.RenameSession(ctx, id, proto.RenameSession{RequestID: "r", Title: "Chani: mornings"})
	if err != nil || renamed.Title != "Chani: mornings" || !renamed.Renamable {
		t.Fatalf("the rename %+v, %v", renamed, err)
	}
	opened, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "n", Profile: "chani"})
	if err != nil || opened.ID != id || opened.Title != "Chani: mornings" {
		t.Fatalf("New Session opens %+v, %v", opened, err)
	}
}
