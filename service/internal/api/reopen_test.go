package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// closedHomeThread delegates a home thread to Duncan, has him report it
// done, and closes it from Odrade's session.
func closedHomeThread(t *testing.T, h *teamHarness, callID string) store.ThreadRow {
	t.Helper()
	ctx := context.Background()
	thread := homeThread(t, h, callID)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: callID + "-done"}, team.Report{Status: team.StatusDone, Message: "Done at e0bfba6."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: callID + "-close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := h.store.Thread(ctx, thread.ID)
	if err != nil || closed.State != store.ThreadClosed {
		t.Fatalf("the thread %+v, %v", closed, err)
	}
	return closed
}

// queueOwner has the owner's messages to session wait in its queue, as a
// busy specialist's do, until placeQueued places one.
func (h *teamHarness) queueOwner(session int64) {
	h.engine.mu.Lock()
	if h.engine.queued == nil {
		h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	}
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[session] = true
	h.engine.mu.Unlock()
	h.engine.onSend = func(to int64, key string) {
		if to == session && strings.HasPrefix(key, "u:") {
			h.engine.mu.Lock()
			if _, sent := h.engine.sent[key]; !sent {
				h.engine.queued[int64(len(h.engine.sent)+100)] = true
			}
			h.engine.mu.Unlock()
		}
	}
}

// The owner's message that reaches a closed thread's specialist takes the
// thread back to open, with its event, so the report of the work it asked
// for is filed (frostyard/pocketgo#55, ADR-0061).
func TestTheOwnersMessageReopensAClosedThread(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := closedHomeThread(t, h, "d1")
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Take Murbella's blocking point, push, and report."}); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.Thread(ctx, thread.ID)
	if err != nil || got.State != store.ThreadOpen || got.Reopened != 1 || got.ReopenedAt == nil {
		t.Fatalf("the thread %+v, %v", got, err)
	}
	states := h.threadStates(thread.ID)
	if len(states) < 2 || states[len(states)-2] != proto.ThreadClosed || states[len(states)-1] != proto.ThreadOpen {
		t.Fatalf("thread events %v", states)
	}
	if text, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusWorking, Message: "Pushed 28c0f56."}); err != nil ||
		!strings.HasPrefix(text, "Reported on thread T1") {
		t.Fatalf("the report %q, %v", text, err)
	}
	if left, _ := h.store.OwnerMessagesOf(ctx, thread.ID); len(left) != 0 {
		t.Fatalf("still followed: %+v", left)
	}
	// The same request again reopens nothing twice.
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Take Murbella's blocking point, push, and report."}); err != nil {
		t.Fatal(err)
	}
	if again, _ := h.store.Thread(ctx, thread.ID); again.State != store.ThreadClosed || again.Reopened != 1 {
		t.Fatalf("a retry reopened it: %+v", again)
	}
}

// Delivered is when PiG's record finds the message in the transcript, not
// when Send returns: a queued message leaves the thread closed, and its
// specialist's report is refused with what to do instead; once it is
// placed, the report notes it first and is filed. A withdrawn one reopens
// nothing (ADR-0061).
func TestTheOwnersMessageReopensOnlyOnceInTheTranscript(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := closedHomeThread(t, h, "d1")
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Report what you pushed."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("a queued message reopened it: %+v", got)
	}
	_, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "early"}, team.Report{Status: team.StatusWorking, Message: "x"})
	if err == nil || !strings.Contains(err.Error(), "thread T1 is closed and takes no more reports") || !strings.Contains(err.Error(), "ask the owner") {
		t.Fatalf("the early report: %v", err)
	}
	// Placed, and no delivery pass has noted it: the report notes it first.
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusDone, Message: "Pushed 28c0f56."}); err != nil {
		t.Fatalf("the report after the message: %v", err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadDone || got.Report != "Pushed 28c0f56." {
		t.Fatalf("the thread %+v", got)
	}

	// A withdrawn message is dropped, and the thread stays closed.
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m2", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if withdrew, err := h.engine.Withdraw(ctx, thread.Session, requestKeyOf(h.harness, "m2")); err != nil || !withdrew {
		t.Fatalf("withdraw %v %v", withdrew, err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed || got.Reopened != 1 {
		t.Fatalf("a withdrawn message reopened it: %+v", got)
	}
	if left, _ := h.store.OwnerMessagesOf(ctx, thread.ID); len(left) != 0 {
		t.Fatalf("a withdrawn message is still followed: %+v", left)
	}
}

// A message the owner sent while the thread was open, placed after a
// close, reopens it too; one placed before the close is noted by the close
// first, and reopens nothing after it (ADR-0061).
func TestACloseNotesTheOwnersMessageFirst(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Before the close."}); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed || got.Reopened != 0 {
		t.Fatalf("a message placed before the close reopened it: %+v", got)
	}

	// One queued over a close reopens the thread once placed.
	other := homeThread(t, h, "d2")
	h.queueOwner(other.Session)
	if _, err := h.client.Send(ctx, other.Session, proto.SendMessage{RequestID: "m2", Text: "Queued over the close."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}, other.ID); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(requestKeyOf(h.harness, "m2"), other.Session)
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, other.ID); got.State != store.ThreadOpen || got.Reopened != 1 {
		t.Fatalf("the message queued over the close: %+v", got)
	}
}

// A supervisor's team_send to a closed thread it delegated reopens it once
// the message is in the specialist's transcript, with a text that says so
// (ADR-0061).
func TestASupervisorsMessageReopensAClosedThread(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := closedHomeThread(t, h, "d1")
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Murbella's point blocks core#283: fix it."})
	if err != nil || !strings.Contains(text, "T1 is open: its next report is expected") {
		t.Fatalf("send %q %v", text, err)
	}
	if h.engine.texts[h.sendKey(call)] != team.SendTextToClosed(thread.ID, "Odrade", "Murbella's point blocks core#283: fix it.") {
		t.Fatalf("the message %q", h.engine.texts[h.sendKey(call)])
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen || got.Reopened != 1 {
		t.Fatalf("the thread %+v", got)
	}
	if states := h.threadStates(thread.ID); states[len(states)-1] != proto.ThreadOpen {
		t.Fatalf("thread events %v", states)
	}
}

// A supervisor's message kept for a closed thread and still queued when
// another message reopened the thread and a close followed goes to no one:
// that close dropped it, and it reopens nothing (ADR-0061).
func TestAReopeningMessageAfterALaterCloseIsDropped(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := closedHomeThread(t, h, "d1")
	h.queueSends(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Fix it."}); err != nil || !strings.Contains(text, "the closed thread is open again then") {
		t.Fatalf("send %q %v", text, err)
	}
	// The owner's message reopens the thread first.
	h.engine.mu.Lock()
	delete(h.engine.busy, thread.Session)
	h.engine.mu.Unlock()
	onSend := h.engine.onSend
	h.engine.onSend = nil
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Do it now."}); err != nil {
		t.Fatal(err)
	}
	h.engine.onSend = onSend
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the owner's message did not reopen it: %+v", got)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed || got.Reopened != 1 {
		t.Fatalf("the dropped message reopened it: %+v", got)
	}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Fix it."}); err == nil || !strings.Contains(err.Error(), "went to no one") {
		t.Fatalf("its answer: %v", err)
	}
}

// A closed thread whose worktree pocketd removed takes no message from the
// owner or its supervisor: the refusal says why and names the thread that
// carries its work on (ADR-0061).
func TestAClosedThreadWhoseWorktreeIsGoneTakesNoMessage(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	row := cleanupClose(t, h, thread)
	cleanupGone(t, h, row)
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "next"},
		team.Delegation{Agent: "duncan", Task: "Take Murbella's re-check point on core#283.", Follows: thread.ID}); err != nil {
		t.Fatal(err)
	}
	sent := len(h.engine.order)
	_, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Take Murbella's point."})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	for _, want := range []string{"thread T1 is closed and its folder is no longer its own", "Send it to T2 (Duncan's thread), which carries its work on."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the owner's refusal %q lacks %q", err, want)
		}
	}
	_, err = h.server.TeamSend(ctx, TeamCall{Session: h.super.ID, CallID: "s1"}, team.Send{Thread: thread.ID, Text: "Take Murbella's point."})
	if err == nil || !strings.Contains(err.Error(), "Send it to T2 (Duncan's thread) with team_send") {
		t.Fatalf("the supervisor's refusal: %v", err)
	}
	if len(h.engine.order) != sent {
		t.Fatalf("a refusal sent %q", h.engine.order[sent:])
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("the thread %+v", got)
	}
}

// A closed thread's worktree stays while a message that may reopen it is
// on its way, though the specialist is idle: once the message is noted the
// thread is open, and its worktree is not removed (ADR-0061).
func TestTheCleanupWaitsForAMessageThatMayReopen(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Queued over the close."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	// Placed, the run over, and not noted yet.
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	h.server.reviewPass(ctx)
	if _, err := os.Stat(thread.Cwd); err != nil {
		t.Fatalf("the worktree went from under the message: %v", err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the thread %+v", got)
	}
	h.server.reviewPass(ctx)
	if _, err := os.Stat(thread.Cwd); err != nil {
		t.Fatalf("a reopened thread's worktree was removed: %v", err)
	}
	// Closed again, it is cleaned again.
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close-again"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	cleanupGone(t, h, thread)
}

// A delegation's follows names the new thread as the older one's
// successor, with the older thread's event; team_message on the closed
// older thread names it (ADR-0061).
func TestFollowsNamesTheSuccessor(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	old := closedHomeThread(t, h, "d1")
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "d2"},
		team.Delegation{Agent: "duncan", Task: "Carry T1 on.", Follows: old.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.Thread(ctx, old.ID)
	if err != nil || got.Successor != 2 {
		t.Fatalf("the old thread %+v, %v", got, err)
	}
	if next, _ := h.store.Thread(ctx, 2); next.Follows != old.ID {
		t.Fatalf("the new thread %+v", next)
	}
	h.mu.Lock()
	var named bool
	for _, e := range h.events {
		named = named || (e.Type == proto.EventThread && e.Thread.ID == old.ID && e.Thread.Successor == 2)
	}
	h.mu.Unlock()
	if !named {
		t.Fatal("no event of the old thread named its successor")
	}
	_, err = h.server.TeamMessage(ctx, TeamCall{Session: old.Session, CallID: "late"}, team.Report{Status: team.StatusWorking, Message: "Late."})
	if err == nil || !strings.Contains(err.Error(), "Its work goes on in thread T2 (Duncan's thread)") {
		t.Fatalf("the late report: %v", err)
	}
	// Refused: another supervisor's thread, no thread, and plan work.
	leto, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		session int64
		d       team.Delegation
		want    string
	}{
		"another's thread": {leto.ID, team.Delegation{Agent: "duncan", Task: "x", Follows: old.ID}, "another supervisor"},
		"no thread":        {h.super.ID, team.Delegation{Agent: "duncan", Task: "x", Follows: 99}, "no thread T99"},
		"plan work":        {h.super.ID, team.Delegation{Agent: "duncan", Task: "x", Follows: old.ID, Plan: &team.PlanRef{Start: true}}, "carries on no thread"},
	} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: tc.session, CallID: name}, tc.d); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
