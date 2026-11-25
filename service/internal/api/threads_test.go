package api

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// threadsCall runs team_threads as session.
func (h *teamHarness) threadsCall(session int64, c team.ThreadsCall) (string, error) {
	return h.server.TeamThreads(context.Background(), TeamCall{Session: session, Task: 1, CallID: "th1"}, c)
}

// team_threads is the supervisor's alone: roleTools offers it to no other
// session, and the call is refused here too from an agent's session, a
// thread's specialist, a plain session and a session pocketd does not
// know, before anything is read. A call that breaks a rule is refused
// (ADR-0071).
func TestTeamThreadsIsTheSupervisorsAlone(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if text, err := h.threadsCall(h.super.ID, team.ThreadsCall{Action: team.ThreadsList}); err != nil || !strings.Contains(text, "With their specialists: none.") {
		t.Fatalf("the supervisor: %q, %v", text, err)
	}
	leto, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.threadsCall(leto.ID, team.ThreadsCall{Action: team.ThreadsList}); err != nil {
		t.Fatalf("another supervisor: %v", err)
	}
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for name, session := range map[string]int64{"an agent": agent.ID, "a plain session": plain.ID, "a thread's specialist": thread.Session} {
		if text, err := h.threadsCall(session, team.ThreadsCall{Action: team.ThreadsRead, Thread: 1}); err == nil || !strings.Contains(err.Error(), "only a supervisor") {
			t.Errorf("%s: %q, %v", name, text, err)
		}
	}
	if text, err := h.threadsCall(999, team.ThreadsCall{Action: team.ThreadsList}); err == nil {
		t.Errorf("no session: %q", text)
	}
	for c, want := range map[team.ThreadsCall]string{
		{Action: team.ThreadsList, Thread: 1}: "list takes no thread",
		{Action: team.ThreadsRead}:            "read needs thread",
		{Action: team.ThreadsRead, Thread: 9}: "there is no thread T9",
	} {
		if text, err := h.threadsCall(h.super.ID, c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %q, %v, want %q", c, text, err, want)
		}
	}
}

// team_threads lists, from pocketd's records, every thread not closed by
// who moves it next as the work board does, marking the ones the calling
// session delegated, and the threads closed in the window; and reads one
// thread whole with its sessions, folder, asks, pull requests and verdict
// (ADR-0071).
func TestTeamThreadsListsAndReadsTheThreads(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	leto, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		session int64
		call    string
		del     team.Delegation
	}{
		{h.super.ID, "c1", team.Delegation{Agent: "vera", Task: "Fix the flaky test.", Title: "Fix CI", Repository: "frostyard/example"}},
		{h.super.ID, "c2", team.Delegation{Agent: "duncan", Task: "Tidy the docs.", Title: "Tidy"}},
		{leto.ID, "c3", team.Delegation{Agent: "duncan", Task: "Write the notes.", Title: "Notes"}},
		{h.super.ID, "c4", team.Delegation{Agent: "duncan", Task: "An old job.", Title: "Old"}},
	} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: d.session, CallID: d.call}, d.del); err != nil {
			t.Fatal(err)
		}
	}
	vera, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	tidy, err := h.store.Thread(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: vera.Session, Task: 5, CallID: "m1"}, team.Report{Message: "A question.", Status: team.StatusBlocked,
		Asks: []team.Ask{{Question: "Squash?"}, {Question: "Rebase?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pulls", vera.ID, store.ThreadBlocked, "A pull request.", nil,
		[]store.PullRow{inboxPull(h, vera, 3, "Fix the flaky test", false)}, func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "Yes, squash."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: tidy.Session, Task: 6, CallID: "m2"}, team.Report{Message: "Tidied.", Status: team.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "x4"}, 4); err != nil {
		t.Fatal(err)
	}
	// Two cross-reviews of #3 pocketd started: one closed at its verdict,
	// one that runs.
	pull, _, err := h.store.PullByRef(ctx, "frostyard/example", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"review:done", "review:runs"} {
		if _, _, err := h.store.CreateThread(ctx, store.ThreadRow{Key: key, Title: "Review #3", Agent: "duncan", SupervisorSession: h.super.ID, Task: "x",
			ReviewPull: pull.ID, ReviewHead: pull.HeadSHA}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.CloseThread(ctx, 5); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Hour)

	text, err := h.threadsCall(h.super.ID, team.ThreadsCall{Action: team.ThreadsList})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"team_threads at Wed 18 Nov 23:00 UTC, from pocketd's records alone",
		"On the desk: 4 threads not closed, of its cap of 12;",
		"Waiting for the owner (1):\n" + `- T1 "Fix CI" · vera · blocked · 1 open ask · pull requests: frostyard/example#3 (open, CI not read yet, no verdict, read 1h ago) · changed 1h ago · delegated by you` + "\n",
		"With their specialists (1):\n" + fmt.Sprintf(`- T3 "Notes" · duncan · open · changed 1h ago · delegated by session %d`, leto.ID) + "\n",
		"Waiting for their supervisor (1):\n" + `- T2 "Tidy" · duncan · done · changed 1h ago · delegated by you` + "\n",
		"Cross-reviews pocketd runs and closes at their verdicts (1):\n" + `- T6 "Review #3" · duncan · starting · reviews frostyard/example#3 at 333333333333 · changed 1h ago` + "\n",
		"Closed threads whose records changed in the last 7 days (1):\n" + `- T4 "Old" · duncan · closed · record last changed 1h ago · delegated by you` + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the list lacks %q:\n%s", want, text)
		}
	}
	// The closed review is left out: its verdict shows on its pull request.
	if strings.Contains(text, "T5 ") {
		t.Errorf("the list holds the closed review:\n%s", text)
	}
	// Read whole, it names pocketd as its starter, never the author's
	// supervisor session it copies (Codex review of #175, round 1), and a
	// review closed with no session started none.
	if text, err := h.threadsCall(h.super.ID, team.ThreadsCall{Action: team.ThreadsRead, Thread: 5}); err != nil ||
		!strings.Contains(text, "- Started by: pocketd, as a cross-review of frostyard/example#3 at 333333333333; it closes at its verdict") ||
		strings.Contains(text, "Delegated by") || !strings.Contains(text, "- Specialist's session: none; pocketd started no session for it") {
		t.Errorf("the closed review read whole: %v\n%s", err, text)
	}
	// Leto, another supervisor, sees the same threads, its own marked.
	if text, err := h.threadsCall(leto.ID, team.ThreadsCall{Action: team.ThreadsList}); err != nil ||
		!strings.Contains(text, `- T3 "Notes" · duncan · open · changed 1h ago · delegated by you`) ||
		!strings.Contains(text, fmt.Sprintf(`- T2 "Tidy" · duncan · done · changed 1h ago · delegated by session %d`, h.super.ID)) {
		t.Errorf("leto's list: %v\n%s", err, text)
	}

	text, err = h.threadsCall(h.super.ID, team.ThreadsCall{Action: team.ThreadsRead, Thread: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"team_threads read T1 at Wed 18 Nov 23:00 UTC",
		`T1 "Fix CI" · vera · blocked · next: the owner`,
		fmt.Sprintf("- Delegated by: session %d, this session\n", h.super.ID),
		fmt.Sprintf("- Specialist's session: %d, faux/faux-1\n", vera.Session),
		"- Repository: frostyard/example · branch " + vera.Branch + " · worktree " + vera.Cwd + "\n",
		"- Worker: none yet\n",
		"Task:\n  Fix the flaky test.\n",
		"Latest report:\n  A pull request.\n",
		"Asks (2):\n" + `- A1 · answered 1h ago (Wed 18 Nov 22:00 UTC) on laptop: "Squash?" · the answer: "Yes, squash."` + "\n" +
			`- A2 · open, asked 1h ago (Wed 18 Nov 22:00 UTC): "Rebase?"` + "\n",
		"Pull requests (1):\n" + `- frostyard/example#3 · "Fix the flaky test" · open · ready-for-human: no · verdict at its head: none · CI: not read yet · read 1h ago`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the read lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Shift") {
		t.Errorf("a thread with no shift shows one:\n%s", text)
	}
	if text, err := h.threadsCall(leto.ID, team.ThreadsCall{Action: team.ThreadsRead, Thread: 4}); err != nil ||
		!strings.Contains(text, `T4 "Old" · duncan · closed · next: nobody: it is closed`) ||
		!strings.Contains(text, fmt.Sprintf("- Delegated by: session %d, of profile odrade", h.super.ID)) {
		t.Errorf("leto reads T4: %v\n%s", err, text)
	}
}

// team_threads writes nothing: no row of the work database changes, no
// event goes out, no message is sent and gh never runs (ADR-0071).
func TestTeamThreadsWritesNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "vera", Task: "Fix CI", Repository: "frostyard/example"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}, team.Report{Message: "A question.", Status: team.StatusWorking,
		Asks: []team.Ask{{Question: "Squash?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pulls", thread.ID, store.ThreadOpen, "A pull request.", nil,
		[]store.PullRow{inboxPull(h, thread, 3, "Fix the flaky test", false)}, func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)

	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	before := tableDump(t, db)
	h.mu.Lock()
	events := len(h.events)
	h.mu.Unlock()
	h.engine.mu.Lock()
	sent, gh := len(h.engine.order), len(h.engine.ghCalls)
	h.engine.mu.Unlock()

	for _, c := range []team.ThreadsCall{{Action: team.ThreadsList}, {Action: team.ThreadsRead, Thread: 1}} {
		if text, err := h.threadsCall(h.super.ID, c); err != nil || !strings.Contains(text, "#3") || !strings.Contains(text, "T1") {
			t.Fatalf("%s: %v\n%s", c.Action, err, text)
		}
	}
	if after := tableDump(t, db); after != before {
		t.Fatalf("the reads changed the work database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.events) != events || len(h.engine.order) != sent || len(h.engine.ghCalls) != gh {
		t.Fatalf("the reads sent %d events, %d messages and ran gh %d times", len(h.events)-events, len(h.engine.order)-sent, len(h.engine.ghCalls)-gh)
	}
}

// A read of a plan's own thread reads its building plan's delivered tasks'
// threads to say who moves it next, as the work board does: the owner
// while a task's thread waits for an answer, its tasks' specialists
// otherwise (Codex review of #175, round 2).
func TestAPlanLeadsReadMovesWithItsTasks(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.settled()
	h.approve("a1")
	for _, d := range []struct{ call, task, agent, repository string }{{"d1", "t1", "vera", "frostyard/example"}, {"d3", "t3", "duncan", ""}} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: d.call},
			team.Delegation{Agent: d.agent, Task: "Build it.", Repository: d.repository, Plan: &team.PlanRef{ID: h.plan, Task: d.task}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		text, err := h.threadsCall(h.super.ID, team.ThreadsCall{Action: team.ThreadsRead, Thread: h.thread.ID})
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	if text := read(); !strings.Contains(text, `T1 "Signing" · duncan · open · next: its specialist`) {
		t.Fatalf("a lead whose tasks work:\n%s", text)
	}
	keys, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"}.key())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: keys.Session, Task: 6, CallID: "q1"}, team.Report{Message: "A question.", Status: team.StatusWorking,
		Asks: []team.Ask{{Question: "Which key size?"}}}); err != nil {
		t.Fatal(err)
	}
	if text := read(); !strings.Contains(text, `T1 "Signing" · duncan · open · next: the owner`) {
		t.Fatalf("a lead whose task waits for the owner:\n%s", text)
	}
}
