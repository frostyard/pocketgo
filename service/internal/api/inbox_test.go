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

// team_inbox is the supervisor's alone: roleTools offers it to no other
// session, and the call is refused here too from an agent's session, a
// thread's specialist, a plain session and a session pocketd does not
// know, before anything is read (ADR-0046).
func TestTeamInboxIsTheSupervisorsAlone(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if text, err := h.server.TeamInbox(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "i1"}); err != nil || !strings.Contains(text, "Waiting for the owner now:") {
		t.Fatalf("the supervisor: %q, %v", text, err)
	}
	leto, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamInbox(ctx, TeamCall{Session: leto.ID, Task: 1, CallID: "i1"}); err != nil {
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
		if text, err := h.server.TeamInbox(ctx, TeamCall{Session: session, Task: 1, CallID: "i1"}); err == nil || !strings.Contains(err.Error(), "only a supervisor") {
			t.Errorf("%s: %q, %v", name, text, err)
		}
	}
	if text, err := h.server.TeamInbox(ctx, TeamCall{Session: 999, Task: 1, CallID: "i1"}); err == nil {
		t.Errorf("no session: %q", text)
	}
}

// inboxPull is a pull request of thread as a report links it, read at the
// clock's now.
func inboxPull(h *teamHarness, thread store.ThreadRow, number int64, title string, draft bool) store.PullRow {
	return store.PullRow{GitHubID: 1000 + number, Repository: "frostyard/example", Number: number, Title: title, URL: fmt.Sprintf("https://github.com/frostyard/example/pull/%d", number),
		State: store.PullOpen, Draft: draft, Head: thread.Branch, HeadSHA: strings.Repeat(fmt.Sprint(number%10), 40), Base: "main", CheckedAt: h.clock.Now()}
}

// settlePull records pull request number as merged or closed by a read at
// the clock's now.
func settlePull(t *testing.T, h *teamHarness, number int64, state string) {
	t.Helper()
	ctx := context.Background()
	p, found, err := h.store.PullByRef(ctx, "frostyard/example", number)
	if err != nil || !found {
		t.Fatalf("#%d: %v %v", number, found, err)
	}
	seq, err := h.store.NextPullRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.State, p.ReadSeq, p.CheckedAt = state, seq, h.clock.Now()
	if _, changed, err := h.store.UpdatePull(ctx, p.ID, p); err != nil || !changed {
		t.Fatalf("#%d %s: %v %v", number, state, changed, err)
	}
}

// team_inbox lists, from pocketd's records, what the desktop's Inbox shows
// as waiting (an open ask, an open pull request that is no draft with its
// label, verdict, CI and last read, a plan to approve, a pending lesson, a
// pending approval) and what was settled in the 7 days before the
// harness's clock: an ask answered, a pull request closed. What was
// settled 8 days before is left out (ADR-0046).
func TestTeamInboxListsWhatWaitsAndWhatWasSettled(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.settled() // P1, led by duncan in T1, waits for the owner's approval.
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "vera", Task: "Fix CI", Repository: "frostyard/example"}); err != nil {
		t.Fatal(err)
	}
	vera, err := h.store.Thread(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Day 0: vera asks three questions and links four pull requests; the
	// owner answers A1, and #5 merges.
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: vera.Session, Task: 5, CallID: "m1"}, team.Report{Message: "Three questions.", Status: team.StatusWorking,
		Asks: []team.Ask{{Question: "Squash?"}, {Question: "Which branch?"}, {Question: "Ship it\nnow?", Options: []string{"yes", "no"}}}}); err != nil {
		t.Fatal(err)
	}
	pulls := []store.PullRow{inboxPull(h.teamHarness, vera, 3, "Fix the flaky test", false), inboxPull(h.teamHarness, vera, 4, "A draft", true),
		inboxPull(h.teamHarness, vera, 5, "Old fix", false), inboxPull(h.teamHarness, vera, 6, "Abandoned", false)}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pulls", vera.ID, store.ThreadOpen, "Four pull requests.", nil, pulls,
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "Yes, squash."}); err != nil {
		t.Fatal(err)
	}
	settlePull(t, h.teamHarness, 5, store.PullMerged)

	// Day 8: the owner answers A2, #6 closes, CI at #3's head is green and
	// a ready verdict backs it, vera proposes a lesson and a tool call
	// waits for approval.
	h.clock.Advance(8 * 24 * time.Hour)
	if _, err := h.client.AnswerAsk(ctx, 2, proto.AnswerAsk{RequestID: "a2", Answer: "main"}); err != nil {
		t.Fatal(err)
	}
	settlePull(t, h.teamHarness, 6, store.PullClosed)
	three, _, err := h.store.PullByRef(ctx, "frostyard/example", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.SetPullChecks(ctx, three.ID, three.HeadSHA, string(proto.ChecksGreen)); err != nil {
		t.Fatal(err)
	}
	// GitHub named #3's merge base, and a ready verdict was filed on it; a
	// later not-ready verdict on another merge base backs nothing, so the
	// verdict at its head is the ready one (ADR-0019; Codex review of #91,
	// round 1).
	mergeBase := strings.Repeat("c", 40)
	if three, _, err = h.store.SetPullMergeBase(ctx, three.ID, three.HeadSHA, three.Base, mergeBase); err != nil || three.MergeBase != mergeBase {
		t.Fatalf("the merge base: %+v %v", three, err)
	}
	for _, r := range []store.ReviewRow{
		{Key: "review", Verdict: "ready", MergeBase: mergeBase},
		{Key: "stale", Verdict: "not-ready", MergeBase: strings.Repeat("d", 40)},
	} {
		r.PullID, r.ThreadID, r.SessionID, r.Agent, r.Provider, r.Model, r.Family = three.ID, vera.ID, vera.Session, "irulan", "openai", "gpt", "openai"
		r.HeadSHA, r.Summary, r.BaseRef = three.HeadSHA, "Read it.", "main"
		if _, _, _, _, err := h.store.AddReview(ctx, three, r, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: vera.Session, Task: 6, CallID: "l1"}, team.Memory{Action: team.MemoryAdd, Text: "Rebase before you push."}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.RequestApproval(ctx, store.ApprovalRow{SessionID: vera.Session, TaskID: 9, CallID: "w1", Tool: "write", Arguments: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Rounds every 5 minutes, as StartPulls records them, with no timer and
	// no review pass started on these records (Codex review of #91, round
	// 1; TestTheInboxSaysHowOftenPullRequestsAreRead covers StartPulls).
	h.server.sessionMu.Lock()
	h.server.pullsStarted, h.server.pullEvery = true, 5*time.Minute
	h.server.sessionMu.Unlock()
	h.clock.Advance(time.Hour)

	text, err := h.server.TeamInbox(ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"team_inbox at Thu 26 Nov 23:00 UTC, from pocketd's records alone",
		"again every 5m;",
		"Asks (1):\n" + `- A3 · T2 "Fix CI" · vera asked 8d ago (Wed 18 Nov 22:00 UTC): "Ship it now?" Options: yes | no.` + "\n",
		"Pull requests to review (1):\n" + `- frostyard/example#3 · T2 "Fix CI" · "Fix the flaky test" · open · ready-for-human: no · verdict at its head: ready, by irulan (openai) · CI: green · read 8d ago (Wed 18 Nov 22:00 UTC)` + "\n",
		"Plans to approve (1):\n" + `- P1 · T1 "Signing" · "Signing" · lead duncan · rev `,
		"Lessons to decide (1):\n" + `- L1 · vera · new, by vera: "Rebase before you push."` + "\n",
		"Tool calls to approve (1):\n- " + store.ApprovalID(9, "w1") + " · session " + fmt.Sprint(vera.Session) + " · write · asked 1h ago",
		"Settled in the last 7 days, since Thu 19 Nov 23:00 UTC:\nAsks answered or withdrawn (1):\n" +
			`- A2 · T2 "Fix CI" · vera: "Which branch?" · answered 1h ago (Thu 26 Nov 22:00 UTC) on laptop: "main"` + "\n",
		"Pull requests merged or closed whose record changed in the window (1):\n" + `- frostyard/example#6 · T2 "Fix CI" · "Abandoned" · closed · record last changed 1h ago (Thu 26 Nov 22:00 UTC)` + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"#4", "#5", "Old fix", "A1 ·", "Squash", "not-ready"} {
		if strings.Contains(text, not) {
			t.Errorf("the answer holds %q:\n%s", not, text)
		}
	}
}

// The answer's first line says how often pocketd reads pull requests, as
// StartPulls set it: not begun before it runs, every interval after, or
// in no rounds. The rounds' timer is the test's, never the wall clock's
// (Codex review of #91, round 1).
func TestTheInboxSaysHowOftenPullRequestsAreRead(t *testing.T) {
	for _, tc := range []struct {
		every time.Duration
		want  string
	}{{5 * time.Minute, "again every 5m;"}, {0, "in no rounds (-pr-every 0)"}} {
		h := newTeamHarness(t)
		ctx := context.Background()
		var rounds []time.Duration
		h.server.pullTimer = func(d time.Duration, _ func()) func() {
			rounds = append(rounds, d)
			return func() {}
		}
		inbox := func() string {
			t.Helper()
			text, err := h.server.TeamInbox(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "i1"})
			if err != nil {
				t.Fatal(err)
			}
			return text
		}
		if text := inbox(); !strings.Contains(text, "have not begun since it started") {
			t.Fatalf("before StartPulls:\n%s", text)
		}
		h.server.StartPulls(tc.every)
		if text := inbox(); !strings.Contains(text, tc.want) {
			t.Fatalf("after StartPulls(%v):\n%s", tc.every, text)
		}
		if tc.every > 0 && (len(rounds) != 1 || rounds[0] != tc.every) || tc.every == 0 && len(rounds) != 0 {
			t.Fatalf("StartPulls(%v) scheduled %v", tc.every, rounds)
		}
	}
}

// tableDump is every row of every table of the database at path, read
// apart from the server's store, with SQLite's data_version, which moves
// when another connection commits.
func tableDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	var version int64
	if err := db.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "data_version %d\n", version)
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+table+`"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %v\n", table, values)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// team_inbox writes nothing: no row of the work database changes, no
// event goes out, no message is sent and gh never runs (ADR-0046).
func TestTeamInboxWritesNothing(t *testing.T) {
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
		Asks: []team.Ask{{Question: "Squash?"}, {Question: "Rebase?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pulls", thread.ID, store.ThreadOpen, "A pull request.", nil,
		[]store.PullRow{inboxPull(h, thread, 3, "Fix the flaky test", false), inboxPull(h, thread, 4, "Done", false)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "yes"}); err != nil {
		t.Fatal(err)
	}
	settlePull(t, h, 4, store.PullMerged)

	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// One connection, so data_version compares commits by others.
	db.SetMaxOpenConns(1)
	before := tableDump(t, db)
	h.mu.Lock()
	events := len(h.events)
	h.mu.Unlock()
	h.engine.mu.Lock()
	sent, gh := len(h.engine.order), len(h.engine.ghCalls)
	h.engine.mu.Unlock()

	text, err := h.server.TeamInbox(ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "i1"})
	if err != nil || !strings.Contains(text, "#3") || !strings.Contains(text, "#4") || !strings.Contains(text, "A1 ·") || !strings.Contains(text, "A2 ·") {
		t.Fatalf("the read: %v\n%s", err, text)
	}
	if after := tableDump(t, db); after != before {
		t.Fatalf("the read changed the work database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.events) != events || len(h.engine.order) != sent || len(h.engine.ghCalls) != gh {
		t.Fatalf("the read sent %d events, %d messages and ran gh %d times", len(h.events)-events, len(h.engine.order)-sent, len(h.engine.ghCalls)-gh)
	}
}
