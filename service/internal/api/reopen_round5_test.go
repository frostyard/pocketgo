package api

import (
	"context"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The owner's close run again notes the thread's placed messages before it
// answers: one that reached the specialist after the close reopened it,
// and the answer says so (Codex review of #121, round 5).
func TestTheOwnersCloseRunAgainNotesPlacedMessagesFirst(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	if _, err := h.client.CloseThread(ctx, thread.ID, proto.CloseThread{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	h.engine.onSend = nil
	again, err := h.client.CloseThread(ctx, thread.ID, proto.CloseThread{RequestID: "c1"})
	if err != nil || again.State != proto.ThreadOpen || again.Reopens != 1 {
		t.Fatalf("the close run again answered %+v, %v", again, err)
	}
}
