package api

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// homeThread delegates a thread to Duncan in his home, from Odrade's
// session, and reads it back once its brief went.
func homeThread(t *testing.T, h *teamHarness, callID string) store.ThreadRow {
	t.Helper()
	call := TeamCall{Session: h.super.ID, CallID: callID}
	if _, err := h.server.TeamDelegate(context.Background(), call, team.Delegation{Agent: "duncan", Task: "Renumber the ADR."}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.ThreadByKey(context.Background(), call.key())
	if err != nil || thread.Session == 0 || thread.State != store.ThreadOpen {
		t.Fatalf("the thread %+v, %v", thread, err)
	}
	return thread
}

// sendKey is the request key of the first attempt of Odrade's message of
// call.
func (h *teamHarness) sendKey(call TeamCall) string {
	return team.SendKey(call.Session, call.key()) + ":report"
}

// queueSends has every supervisor's message wait in its session's queue,
// as a busy specialist's does: the specialist is at work until placed.
func (h *teamHarness) queueSends(session int64) {
	h.engine.mu.Lock()
	if h.engine.queued == nil {
		h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	}
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[session] = true
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if team.IsSendKey(key) {
			h.engine.mu.Lock()
			if _, sent := h.engine.sent[key]; !sent {
				h.engine.queued[int64(len(h.engine.sent)+100)] = true
			}
			h.engine.mu.Unlock()
		}
	}
}

// placeQueued plays PiG: the queued message of key reaches its transcript,
// and its session's run goes on.
func (h *teamHarness) placeQueued(key string, session int64) {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	delete(h.engine.queued, h.engine.sent[key])
	delete(h.engine.busy, session)
}

// team_send delivers a supervisor's message to the specialist of a thread
// it delegated, marked as the supervisor's, once: the same call run again
// sends nothing twice, and answers as it is (ADR-0032).
func TestTeamSendDeliversToItsThreadsSpecialist(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	call := TeamCall{Session: h.super.ID, Task: 4, CallID: "s1"}
	text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: " core#281 took ADR-0088: renumber yours. "})
	if err != nil || !strings.HasPrefix(text, "Delivered to thread T1: the message is in Duncan's session.") {
		t.Fatalf("send %q %v", text, err)
	}
	key := h.sendKey(call)
	if h.engine.to[key] != thread.Session || h.engine.texts[key] != "[T1 · Odrade, the supervisor] core#281 took ADR-0088: renumber yours.\n\nWhen you have acted on it, report with team_message." {
		t.Fatalf("the message to %d: %q", h.engine.to[key], h.engine.texts[key])
	}
	if !h.engine.watching[thread.Session] {
		t.Fatal("the specialist was not watched before its message went")
	}
	if row, err := h.store.Report(ctx, team.SendKey(call.Session, call.key())); err != nil || row.SentAt == nil {
		t.Fatalf("the message is not noted delivered: %+v %v", row, err)
	}
	sent := len(h.engine.order)
	if again, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "another text"}); err != nil || again != text || len(h.engine.order) != sent {
		t.Fatalf("a run again %q %v, %d sends after %d", again, err, len(h.engine.order), sent)
	}
	// An open thread stays open, and its supervisor's message is no report.
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen || got.Report != "" {
		t.Fatalf("the thread %+v", got)
	}
	// Another call is a message of its own.
	if _, err := h.server.TeamSend(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "s1"}, team.Send{Thread: thread.ID, Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if got := h.sentTo(thread.Session); len(got) != 3 || !strings.Contains(got[2], "One more thing.") {
		t.Fatalf("the specialist got %q", got)
	}
}

// team_send is refused in code for a caller that is no supervisor, for a
// thread another supervisor delegated, a closed thread, one still
// starting, no thread, and a text that is empty or over 6,000 characters;
// a refusal keeps and sends nothing (ADR-0032).
func TestTeamSendRefuses(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	leto, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	letos := TeamCall{Session: leto.ID, CallID: "d2"}
	if _, err := h.server.TeamDelegate(ctx, letos, team.Delegation{Agent: "duncan", Task: "Leto's work"}); err != nil {
		t.Fatal(err)
	}
	other, _ := h.store.ThreadByKey(ctx, letos.key())
	closed := homeThread(t, h, "d3")
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c3"}, closed.ID); err != nil {
		t.Fatal(err)
	}
	starting, _, err := h.store.CreateThread(ctx, store.ThreadRow{Key: "starting", Title: "x", Agent: "duncan", SupervisorSession: h.super.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	sent := len(h.engine.order)
	for name, tc := range map[string]struct {
		session int64
		send    team.Send
		want    string
	}{
		"an agent sends":     {agent.ID, team.Send{Thread: thread.ID, Text: "x"}, "only a supervisor"},
		"a plain session":    {plain.ID, team.Send{Thread: thread.ID, Text: "x"}, "only a supervisor"},
		"the specialist":     {thread.Session, team.Send{Thread: thread.ID, Text: "x"}, "only a supervisor"},
		"another's thread":   {h.super.ID, team.Send{Thread: other.ID, Text: "x"}, "delegated by another supervisor"},
		"to Odrade's thread": {leto.ID, team.Send{Thread: thread.ID, Text: "x"}, "delegated by another supervisor"},
		"a closed thread":    {h.super.ID, team.Send{Thread: closed.ID, Text: "x"}, "is closed"},
		"a starting thread":  {h.super.ID, team.Send{Thread: starting.ID, Text: "x"}, "still starting"},
		"no thread":          {h.super.ID, team.Send{Thread: 99, Text: "x"}, "no thread T99"},
		"an empty text":      {h.super.ID, team.Send{Thread: thread.ID, Text: "  "}, "empty"},
		"a long text":        {h.super.ID, team.Send{Thread: thread.ID, Text: strings.Repeat("x", team.MaxSend+1)}, "more than 6000"},
	} {
		if _, err := h.server.TeamSend(ctx, TeamCall{Session: tc.session, CallID: name}, tc.send); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
	if len(h.engine.order) != sent {
		t.Fatalf("a refusal sent %q", h.engine.order[sent:])
	}
	if unsent, _ := h.store.UnsentReports(ctx); len(unsent) != 0 {
		t.Fatalf("a refusal kept %+v", unsent)
	}
	// 6,000 characters go.
	if _, err := h.server.TeamSend(ctx, TeamCall{Session: h.super.ID, CallID: "full"}, team.Send{Thread: thread.ID, Text: strings.Repeat("é", team.MaxSend)}); err != nil {
		t.Fatalf("a message of %d characters: %v", team.MaxSend, err)
	}
}

// A supervisor's message in a done thread's specialist transcript takes the
// thread back to open, with its event, so its next report is expected; a
// blocked thread stays blocked (ADR-0032).
func TestASupervisorsMessageReopensADoneThread(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Done."}); err != nil {
		t.Fatal(err)
	}
	text, err := h.server.TeamSend(ctx, TeamCall{Session: h.super.ID, CallID: "s1"}, team.Send{Thread: thread.ID, Text: "T80 found a blocking point: fix it."})
	if err != nil || !strings.Contains(text, "T1 was done, and is open again") {
		t.Fatalf("send %q %v", text, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the thread %+v", got)
	}
	states := h.threadStates(thread.ID)
	if len(states) < 2 || states[len(states)-2] != proto.ThreadDone || states[len(states)-1] != proto.ThreadOpen {
		t.Fatalf("thread events %v", states)
	}
	// Its next report sets its state as any.
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusBlocked, Message: "Stuck."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamSend(ctx, TeamCall{Session: h.super.ID, CallID: "s2"}, team.Send{Thread: thread.ID, Text: "Try the other fixture."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadBlocked {
		t.Fatalf("a blocked thread %+v", got)
	}
}

// A busy specialist's queue holds the message: team_send says it is
// queued, and the thread stays done until the message is in the
// transcript; then a delivery pass notes it once, and the thread opens
// (ADR-0032).
func TestASupervisorsMessageToABusySpecialistIsQueued(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Done."}); err != nil {
		t.Fatal(err)
	}
	h.queueSends(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Renumber."})
	if err != nil || !strings.HasPrefix(text, "Queued for thread T1:") {
		t.Fatalf("send %q %v", text, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadDone {
		t.Fatalf("a queued message moved the thread: %+v", got)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadDone {
		t.Fatalf("a message still queued moved the thread: %+v", got)
	}
	h.placeQueued(h.sendKey(call), thread.Session)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("a delivered message left the thread %+v", got)
	}
	if n := countWith(h.sentTo(thread.Session), "Renumber."); n != 1 {
		t.Fatalf("the message went %d times", n)
	}
	if again, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Renumber."}); err != nil || !strings.HasPrefix(again, "Delivered to thread T1") {
		t.Fatalf("a run again %q %v", again, err)
	}
}

// restarted is a new server on the harness's work database and engine, as
// pocketd after a restart: what memory held is gone.
func (h *teamHarness) restarted(t *testing.T) *Server {
	t.Helper()
	s := New(h.store, h.hub, "test", h.clock.Now, nil)
	s.SetApprovals(approvals.New(h.store, s.PublishApproval))
	s.SetEngine(h.engine)
	s.SetProfiles(profiles.Folder{Dir: h.profileDir, Home: h.home})
	s.retry = func(func()) func() { return func() {} }
	s.goShift = func(func()) {}
	s.goSchedule = func(func()) {}
	t.Cleanup(s.Stop)
	return s
}

// A message that cannot go now is kept, and placed once by a later pass,
// a restarted pocketd's included: a submission whose noting a stop cut off
// is not sent again (ADR-0032).
func TestASupervisorsMessageGoesOnceAcrossARestart(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	h.failTo(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if text, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Renumber."}); err != nil || !strings.HasPrefix(text, "Queued") {
		t.Fatalf("send %q %v", text, err)
	}
	h.failTo(0)
	// pocketd stops: its message was submitted once, and its noting cut off.
	key := h.sendKey(call)
	if _, err := h.engine.Send(ctx, thread.Session, key, "[T1 · Odrade, the supervisor] Renumber."); err != nil {
		t.Fatal(err)
	}
	again := h.restarted(t)
	if err := again.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countWith(h.sentTo(thread.Session), "Renumber."); n != 1 {
		t.Fatalf("the message went %d times", n)
	}
	if row, _ := h.store.Report(ctx, team.SendKey(call.Session, call.key())); row.SentAt == nil {
		t.Fatal("not noted once placed")
	}
	// The call run again after the restart answers as it is.
	if text, err := again.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Renumber."}); err != nil || !strings.HasPrefix(text, "Delivered") {
		t.Fatalf("a run again %q %v", text, err)
	}
	if n := countWith(h.sentTo(thread.Session), "Renumber."); n != 1 {
		t.Fatalf("a run again sent it: %d times", n)
	}
}

// A report filed after a supervisor's message reached the transcript,
// before pocketd noted it, keeps its state: the message is noted first, so
// it never reopens the thread over the report it came before. One still
// queued when the report is filed reopens the thread once placed
// (ADR-0032).
func TestAReportAfterThePlacedMessageKeepsItsState(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Done."}); err != nil {
		t.Fatal(err)
	}
	h.queueSends(thread.Session)
	first := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamSend(ctx, first, team.Send{Thread: thread.ID, Text: "Fix the finding."}); err != nil {
		t.Fatal(err)
	}
	// PiG places it, and the specialist acts on it and reports done before
	// a pass notes it.
	h.placeQueued(h.sendKey(first), thread.Session)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusDone, Message: "Fixed."}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadDone || got.Report != "Fixed." {
		t.Fatalf("the report's state was overturned: %+v", got)
	}
	// Queued when the next report is filed, it comes after it.
	h.queueSends(thread.Session)
	second := TeamCall{Session: h.super.ID, CallID: "s2"}
	if _, err := h.server.TeamSend(ctx, second, team.Send{Thread: thread.ID, Text: "One more."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r3"}, team.Report{Status: team.StatusDone, Message: "Still done."}); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(h.sendKey(second), thread.Session)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("a message placed after the report left the thread %+v", got)
	}
}

// A close withdraws the thread's supervisor's message still queued, and
// drops it: it goes to no one, and team_send run again says so (ADR-0032).
func TestAClosedThreadDropsItsQueuedSupervisorsMessage(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	h.queueSends(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Renumber."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.engine.withdrawals, h.sendKey(call)) {
		t.Fatalf("not withdrawn: %q", h.engine.withdrawals)
	}
	if unsent, _ := h.store.UnsentReports(ctx); len(unsent) != 0 {
		t.Fatalf("still owed %+v", unsent)
	}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Renumber."}); err == nil || !strings.Contains(err.Error(), "went to no one") {
		t.Fatalf("a run again: %v", err)
	}
}

// A supervisor's message waits for its thread's brief to be in the
// specialist's transcript, as the owner's does (ADR-0032).
func TestASupervisorsMessageWaitsForTheBrief(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	queueBriefs(h)
	call := TeamCall{Session: h.super.ID, CallID: "d1"}
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.ThreadByKey(ctx, call.key())
	if thread.State != store.ThreadOpen || thread.Briefed {
		t.Fatalf("the thread %+v", thread)
	}
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{thread.Session: true}
	h.engine.mu.Unlock()
	send := TeamCall{Session: h.super.ID, CallID: "s1"}
	if text, err := h.server.TeamSend(ctx, send, team.Send{Thread: thread.ID, Text: "Renumber."}); err != nil || !strings.HasPrefix(text, "Queued") {
		t.Fatalf("send %q %v", text, err)
	}
	if _, sent := h.engine.sent[h.sendKey(send)]; sent {
		t.Fatal("the message went before the brief")
	}
	// The brief reaches the transcript: the message follows.
	h.engine.mu.Lock()
	h.engine.queued = map[int64]bool{}
	delete(h.engine.busy, thread.Session)
	h.engine.mu.Unlock()
	h.engine.onSend = nil
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.sentTo(thread.Session)
	if len(got) != 2 || !strings.Contains(got[0], "delegated this to you") || !strings.Contains(got[1], "Renumber.") {
		t.Fatalf("the specialist got %q", got)
	}
}

// A shift takes a supervisor's message as its round's input: the shift
// does not pause, its round waits for the message and the run it starts,
// and that run's STATUS line is the round's verdict (ADR-0032).
func TestAShiftTakesASupervisorsMessageAsRoundInput(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	h.answerRound(thread, thread.BriefSubmission, "Fixed one.\nSTATUS: CONTINUE")
	// The message cannot go yet: the round waits for it.
	h.failTo(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "t1"}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "The flaky one is TestB."}); err != nil {
		t.Fatal(err)
	}
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 {
		t.Fatalf("a round with a message owed moved: %+v", sh)
	}
	h.failTo(0)
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{thread.Session: true}
	h.engine.mu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	// It is in the transcript, and the specialist works on it: the shift
	// runs on, unpaused.
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 || sh.Reason != "" {
		t.Fatalf("the message paused or moved the shift: %+v", sh)
	}
	h.answerRound(thread, h.engine.sent[h.sendKey(call)], "Fixed TestB too.\nSTATUS: DONE")
	h.engine.mu.Lock()
	delete(h.engine.busy, thread.Session)
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	sh := h.shift(thread)
	if sh.State != team.ShiftSummary || sh.Reason != team.ShiftDone || len(sh.Rounds) != 1 || sh.Rounds[0].Verdict != team.VerdictDone {
		t.Fatalf("the round's verdict is not its last run's: %+v", sh)
	}
}

// A supervisor's session started before team_send gets it at start; a
// session of any other profile does not, and a standing one takes it at
// its own refresh (ADR-0032).
func TestOlderSupervisorSessionsGetTheSendTool(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	if err := os.WriteFile(filepath.Join(h.dir, "leto.md"), []byte(leto), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "vera", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	older, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.sendTool[older.ID] || h.engine.sendTool[plain.ID] || h.engine.sendTool[h.odrade] {
		t.Fatalf("team_send went to %v", h.engine.sendTool)
	}
}
