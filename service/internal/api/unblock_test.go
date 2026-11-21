package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// blockThread files a blocked report on thread with an ask per question,
// from its specialist, and returns the asks.
func blockThread(t *testing.T, h *teamHarness, thread store.ThreadRow, callID string, questions ...string) []proto.Ask {
	t.Helper()
	asks := make([]team.Ask, len(questions))
	for i, q := range questions {
		asks[i] = team.Ask{Question: q}
	}
	if _, err := h.server.TeamMessage(context.Background(), TeamCall{Session: thread.Session, CallID: callID},
		team.Report{Status: team.StatusBlocked, Message: "Stuck.", Asks: asks}); err != nil {
		t.Fatal(err)
	}
	open, err := h.client.Asks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []proto.Ask
	for _, a := range open {
		if a.Thread == thread.ID {
			out = append(out, a)
		}
	}
	if len(out) != len(questions) {
		t.Fatalf("asks %+v, want %d", out, len(questions))
	}
	return out
}

// askEvents are the states the ask events of ask id carried, and the
// index of each in the harness's events.
func (h *teamHarness) askEvents(id int64) (states []proto.AskState, at []int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, e := range h.events {
		if e.Type == proto.EventAsk && e.Ask.ID == id {
			states, at = append(states, e.Ask.State), append(at, i)
		}
	}
	return states, at
}

// lastThreadEvent is the index of thread id's last event in the harness's
// events, -1 for none.
func (h *teamHarness) lastThreadEvent(id int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	last := -1
	for i, e := range h.events {
		if e.Type == proto.EventThread && e.Thread.ID == id {
			last = i
		}
	}
	return last
}

// A supervisor's message that reaches a blocked thread's specialist
// unblocks the thread: in the step that notes it delivered, its open asks
// are withdrawn and leave the owner's Inbox, each with its ask event before
// the thread's, the thread is open again, and team_send's answer says so
// and names the asks. The owner's answer to a withdrawn ask is refused with
// the reason, and nothing reaches the specialist for it (ADR-0037).
func TestASupervisorsMessageUnblocksABlockedThread(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	asks := blockThread(t, h, thread, "r1", "Which branch?", "Squash?")
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Use main, and squash."})
	want := "Delivered to thread T1: the message is in Duncan's session. It unblocked T1, which was blocked: pocketd withdrew A1 (\"Which branch?\") " +
		"and A2 (\"Squash?\") from the owner's Inbox, and the owner will not answer them. T1 is open: its next report is expected."
	if err != nil || !strings.HasPrefix(text, want) {
		t.Fatalf("send %q %v, want it to begin %q", text, err, want)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen || got.Report != "Stuck." {
		t.Fatalf("the thread %+v", got)
	}
	if open, err := h.client.Asks(ctx); err != nil || len(open) != 0 {
		t.Fatalf("the Inbox's asks %+v %v", open, err)
	}
	last := h.lastThreadEvent(thread.ID)
	for _, a := range asks {
		states, at := h.askEvents(a.ID)
		if !slices.Equal(states, []proto.AskState{proto.AskOpen, proto.AskWithdrawn}) || at[1] > last {
			t.Fatalf("A%d's events %v at %v, the thread's last at %d", a.ID, states, at, last)
		}
	}
	if states := h.threadStates(thread.ID); states[len(states)-1] != proto.ThreadOpen {
		t.Fatalf("thread events %v", states)
	}
	// The owner's answer, too late, is refused with the reason, and goes
	// nowhere.
	_, err = h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a1", Answer: "main"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Message != team.AnswerWithdrawn(asks[0].ID, thread.ID) {
		t.Fatalf("the refusal says %v", err)
	}
	if got := countWith(h.sentTo(thread.Session), "The owner answered"); got != 0 {
		t.Fatalf("an answer reached the specialist %d times", got)
	}
	if a, _ := h.store.Ask(ctx, asks[0].ID); a.State != store.AskWithdrawn || a.Answer != "" {
		t.Fatalf("the ask %+v", a)
	}
	// Run again, team_send says the same, from the message's own record.
	if again, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Use main, and squash."}); err != nil || again != text {
		t.Fatalf("a run again %q %v", again, err)
	}
	// Another message to the open thread unblocks nothing, and says so.
	if other, err := h.server.TeamSend(ctx, TeamCall{Session: h.super.ID, CallID: "s2"}, team.Send{Thread: thread.ID, Text: "One more thing."}); err != nil ||
		strings.Contains(other, "unblocked") || !strings.Contains(other, "T1 is open") {
		t.Fatalf("another message %q %v", other, err)
	}
}

// A message queued for a busy specialist unblocks its thread only once it
// is in the transcript. An answer the owner gives before then stands, and
// reaches the specialist; the rest of the thread's asks are withdrawn, and
// team_send's answer names only those (ADR-0037).
func TestAnAnswerBeforeTheQueuedMessageStands(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	asks := blockThread(t, h, thread, "r1", "Which branch?", "Squash?")
	h.queueSends(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Use main."}); err != nil || !strings.HasPrefix(text, "Queued") ||
		!strings.Contains(text, "a blocked one's asks that wait for the owner are withdrawn") {
		t.Fatalf("send %q %v", text, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadBlocked {
		t.Fatalf("a queued message unblocked the thread: %+v", got)
	}
	if open, _ := h.client.Asks(ctx); len(open) != 2 {
		t.Fatalf("a queued message withdrew asks: %+v", open)
	}
	if _, err := h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a1", Answer: "main"}); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(h.sendKey(call), thread.Session)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the thread %+v", got)
	}
	if a, _ := h.store.Ask(ctx, asks[0].ID); a.State != store.AskAnswered || !a.Delivered {
		t.Fatalf("the answered ask %+v", a)
	}
	if a, _ := h.store.Ask(ctx, asks[1].ID); a.State != store.AskWithdrawn {
		t.Fatalf("the open ask %+v", a)
	}
	if n := countWith(h.sentTo(thread.Session), `The owner answered A1 ("Which branch?"): main`); n != 1 {
		t.Fatalf("the answer went %d times", n)
	}
	text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Use main."})
	if err != nil || !strings.Contains(text, "pocketd withdrew A2 (\"Squash?\") from the owner's Inbox, and the owner will not answer it.") || strings.Contains(text, "A1") {
		t.Fatalf("a run again %q %v", text, err)
	}
}

// A report filed after the message reached the transcript, before pocketd
// noted it, notes it first: the asks are withdrawn, with their events, and
// the report's own state stands (ADR-0032, ADR-0037).
func TestAReportAfterTheMessageWithdrawsTheAsksFirst(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	asks := blockThread(t, h, thread, "r1", "Which branch?")
	h.queueSends(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Use main."}); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(h.sendKey(call), thread.Session)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusDone, Message: "Merged into main."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadDone || got.Report != "Merged into main." {
		t.Fatalf("the report's state was overturned: %+v", got)
	}
	if states, _ := h.askEvents(asks[0].ID); !slices.Equal(states, []proto.AskState{proto.AskOpen, proto.AskWithdrawn}) {
		t.Fatalf("the ask's events %v", states)
	}
	if open, _ := h.client.Asks(ctx); len(open) != 0 {
		t.Fatalf("the Inbox's asks %+v", open)
	}
	if row, _ := h.store.Report(ctx, team.SendKey(call.Session, call.key())); row.UnblockedAt == nil {
		t.Fatalf("the message %+v", row)
	}
}
