package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// The owner's answer reaches the asking session and, once, the session
// that delegated the thread: a note with the question, the answer and the
// thread's next step, placed by the outbox. The same request again tells
// nothing new, and the specialist's own report is the only other message
// (ADR-0071).
func TestTheSupervisorHearsTheOwnersAnswerOnce(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "vera", Task: "Fix CI", Title: "Fix CI", Repository: "frostyard/example"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}, team.Report{Message: "A question.", Status: team.StatusBlocked,
		Asks: []team.Ask{{Question: "Squash the commits?"}}}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	before := len(h.sentTo(h.super.ID))
	for range 2 {
		if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "Yes, squash."}); err != nil {
			t.Fatal(err)
		}
		h.runScheduled(20)
		if err := h.server.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := h.sentTo(h.super.ID)[before:]
	want := "[T1 · A1 answered] The owner answered vera's ask A1 in T1 \"Fix CI\".\nQuestion: \"Squash the commits?\"\nAnswer: \"Yes, squash.\"\n" +
		"pocketd brings the answer to vera's session. vera goes on with T1 once the answer is in its session, and its next report comes to you."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("the supervisor got %q, want one note %q", got, want)
	}
	if answers := countWith(h.sentTo(thread.Session), "The owner answered A1"); answers != 1 {
		t.Fatalf("the specialist got the answer %d times", answers)
	}
	note, err := h.store.Note(ctx, team.AnsweredKey(thread.Owner, "team:"+fmt.Sprint(thread.Session)+":5:m1:ask:0", "Yes, squash."))
	if err != nil || note.SentAt == nil || note.SessionID != h.super.ID {
		t.Fatalf("the note %+v %v", note, err)
	}
}

// An owner's message that reached a closed thread's specialist reopens the
// thread once noted. An answer that comes between its placement and its
// noting notes it first, under outboxMu, so the answer's note reads the
// reopened thread and never tells the supervisor to reopen an open one
// (ADR-0061, ADR-0071; Codex review of #175, round 1).
func TestTheAnswersNoteReadsTheThreadAfterItsPlacedMessages(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}, team.Report{Message: "A question.", Status: team.StatusBlocked,
		Asks: []team.Ask{{Question: "Keep the old numbers?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Carry on with the renumbering."}); err != nil {
		t.Fatal(err)
	}
	// Placed in the transcript, and no pass has noted it yet.
	h.placeQueued(requestKeyOf(h.harness, "o1"), thread.Session)
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("the thread before the answer %+v", got)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "Keep them."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen || got.Reopened != 1 {
		t.Fatalf("the thread after the answer %+v", got)
	}
	note, err := h.store.Note(ctx, team.AnsweredKey(thread.Owner, "team:"+fmt.Sprint(thread.Session)+":5:m1:ask:0", "Keep them."))
	if err != nil || strings.Contains(note.Text, "is closed") || !strings.Contains(note.Text, "duncan goes on with T1 once the answer is in its session") {
		t.Fatalf("the note %+v %v", note, err)
	}
}
