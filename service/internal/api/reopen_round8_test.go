package api

import (
	"context"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// team_message tells the specialist to call again only for a message in
// its transcript that waits for the worker's stop: one still queued,
// which it has not seen, leaves the closed thread's refusal as it is
// (Codex review of #121, round 8).
func TestAQueuedMessageHoldsNoReport(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	startedWorker(t, h, thread)
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	_, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "late"}, team.Report{Status: team.StatusWorking, Message: "x"})
	if err == nil || err.Error() == team.ReportHeld(thread.ID) || !strings.Contains(err.Error(), "is closed and takes no more reports") {
		t.Fatalf("the report with the message queued: %v", err)
	}
	// Placed, it holds the report until the stop.
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "late2"}, team.Report{Status: team.StatusWorking, Message: "x"}); err == nil ||
		err.Error() != team.ReportHeld(thread.ID) {
		t.Fatalf("the report with the message placed: %v", err)
	}
}
