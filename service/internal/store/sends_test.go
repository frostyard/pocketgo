package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sendThread is an open thread whose specialist works in session.
func sendThread(t *testing.T, s *Store, key string, session int64) ThreadRow {
	t.Helper()
	row, _, err := s.CreateThread(ctx, ThreadRow{Key: key, Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "Fix it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachThread(ctx, row.ID, session, "/w", "", "the brief"); err != nil {
		t.Fatal(err)
	}
	opened, err := s.OpenThread(ctx, row.ID)
	if err != nil || opened.State != ThreadOpen {
		t.Fatalf("open %+v %v", opened, err)
	}
	return opened
}

// A supervisor's message is kept once by its key, only for a thread that
// is open, done or blocked, to its specialist's session (ADR-0032).
func TestASupervisorsMessageIsKeptOnce(t *testing.T) {
	s, _ := open(t)
	thread := sendThread(t, s, "delegate:1:c1", 9)
	row := ReportRow{Key: "team-send:1:team:1:4:s1", ThreadID: thread.ID, SessionID: 9, Text: "Renumber to 0089."}
	kept, err := s.FileSend(ctx, row)
	if err != nil || kept.Key != row.Key || kept.SessionID != 9 || kept.Text != row.Text || kept.SentAt != nil {
		t.Fatalf("kept %+v %v", kept, err)
	}
	// The same call again finds its row, whatever the thread is now.
	if _, err := s.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	again, err := s.FileSend(ctx, ReportRow{Key: row.Key, ThreadID: thread.ID, SessionID: 9, Text: "other"})
	if err != nil || again.Text != row.Text {
		t.Fatalf("again %+v %v", again, err)
	}
	// A new one for a closed thread is refused, and nothing is kept.
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:new", ThreadID: thread.ID, SessionID: 9, Text: "late"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("to a closed thread: %v", err)
	}
	if filed, _ := s.ReportFiled(ctx, "team-send:1:new"); filed {
		t.Fatal("a refused message was kept")
	}
	// So is one for a thread still starting, and one to another session.
	starting, _, err := s.CreateThread(ctx, ThreadRow{Key: "delegate:1:c2", Title: "x", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:starting", ThreadID: starting.ID, SessionID: 9, Text: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("to a starting thread: %v", err)
	}
	other := sendThread(t, s, "delegate:1:c3", 11)
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:wrong", ThreadID: other.ID, SessionID: 10, Text: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("to another session: %v", err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:none", ThreadID: 99, SessionID: 9, Text: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("to no thread: %v", err)
	}
	// Done and blocked threads take one.
	for i, state := range []string{ThreadDone, ThreadBlocked} {
		th := sendThread(t, s, "delegate:1:d"+state, int64(20+i))
		if _, _, _, err := s.FileReport(ctx, "r:"+state, th.ID, state, "x", nil, reportText); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:" + state, ThreadID: th.ID, SessionID: int64(20 + i), Text: "x"}); err != nil {
			t.Fatalf("%d: to a %s thread: %v", i, state, err)
		}
	}
	// It is owed to the specialist's session until it is noted delivered.
	if reports, _, err := s.Owed(ctx, 9); err != nil || len(reports) != 1 || reports[0].Key != row.Key {
		t.Fatalf("owed %d, %v", len(reports), err)
	}
}

// Its delivery takes a done thread back to open in the same transaction,
// once; an open or a closed thread keeps its state (ADR-0032), and a
// blocked one opens too (ADR-0037).
func TestADeliveredMessageReopensADoneThread(t *testing.T) {
	s, _ := open(t)
	thread := sendThread(t, s, "delegate:1:c1", 9)
	if _, _, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadDone, "done", nil, reportText); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"team-send:1:a", "team-send:1:b"} {
		if _, err := s.FileSend(ctx, ReportRow{Key: key, ThreadID: thread.ID, SessionID: 9, Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	got, change, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID)
	if err != nil || !change.Reopened || change.Unblocked || got.State != ThreadOpen {
		t.Fatalf("delivered %+v %+v %v", got, change, err)
	}
	if row, _ := s.Report(ctx, "team-send:1:a"); row.SentAt == nil || row.UnblockedAt != nil {
		t.Fatalf("the message is not noted delivered, or noted as unblocking: %+v", row)
	}
	// Noted again, it reopens nothing: a report filed since stands.
	if _, _, _, err := s.FileReport(ctx, "r2", thread.ID, ThreadDone, "done again", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if got, change, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID); err != nil || change.Reopened || got.State != ThreadDone {
		t.Fatalf("noted again %+v %+v %v", got, change, err)
	}
	// An open thread stays open.
	if _, _, _, err := s.FileReport(ctx, "r3", thread.ID, ThreadOpen, "working", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if got, change, err := s.SendDelivered(ctx, "team-send:1:b", thread.ID); err != nil || change.Reopened || got.State != ThreadOpen {
		t.Fatalf("an open thread %+v %+v %v", got, change, err)
	}
	// A closed thread stays closed.
	closed := sendThread(t, s, "delegate:1:c2", 10)
	if _, _, _, err := s.FileReport(ctx, "r4", closed.ID, ThreadBlocked, "stuck", []AskRow{{Key: "r4:ask:0", SessionID: 10, Question: "q"}}, reportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:c", ThreadID: closed.ID, SessionID: 10, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, closed.ID); err != nil {
		t.Fatal(err)
	}
	if got, change, err := s.SendDelivered(ctx, "team-send:1:c", closed.ID); err != nil || change.Reopened || len(change.Withdrawn) != 0 || got.State != ThreadClosed {
		t.Fatalf("a closed thread %+v %+v %v", got, change, err)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 1 {
		t.Fatalf("a closed thread's ask was withdrawn: %+v", open)
	}
}

// blockedThread is a thread of session's specialist blocked by a report
// with an ask per question, and those asks.
func blockedThread(t *testing.T, s *Store, key string, session int64, questions ...string) (ThreadRow, []AskRow) {
	t.Helper()
	thread := sendThread(t, s, key, session)
	asks := make([]AskRow, len(questions))
	for i, q := range questions {
		asks[i] = AskRow{Key: key + ":r:ask:" + strconv.Itoa(i), SessionID: session, Question: q}
	}
	got, filed, _, err := s.FileReport(ctx, key+":r", thread.ID, ThreadBlocked, "stuck", asks, reportText)
	if err != nil || got.State != ThreadBlocked || len(filed) != len(questions) {
		t.Fatalf("blocked %+v %+v %v", got, filed, err)
	}
	return got, filed
}

// The delivery of a supervisor's message takes a blocked thread back to
// open in the transaction that notes it, withdraws its open asks, and
// records which message did: the asks leave the open list and owe the
// specialist nothing (ADR-0037).
func TestADeliveredMessageUnblocksABlockedThread(t *testing.T) {
	s, _ := open(t)
	thread, asks := blockedThread(t, s, "delegate:1:c1", 9, "Which branch?", "Squash?")
	other, otherAsks := blockedThread(t, s, "delegate:1:c2", 10, "Other?")
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:a", ThreadID: thread.ID, SessionID: 9, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	got, change, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID)
	if err != nil || !change.Reopened || !change.Unblocked || got.State != ThreadOpen || got.Report != "stuck" {
		t.Fatalf("delivered %+v %+v %v", got, change, err)
	}
	if len(change.Withdrawn) != 2 || change.Withdrawn[0].ID != asks[0].ID || change.Withdrawn[1].ID != asks[1].ID {
		t.Fatalf("withdrew %+v, want %d and %d", change.Withdrawn, asks[0].ID, asks[1].ID)
	}
	for _, a := range change.Withdrawn {
		if a.State != AskWithdrawn || a.WithdrawnBy != "team-send:1:a" || a.WithdrawnAt == nil || a.Answer != "" {
			t.Fatalf("a withdrawn ask %+v", a)
		}
	}
	if row, _ := s.Report(ctx, "team-send:1:a"); row.SentAt == nil || row.UnblockedAt == nil {
		t.Fatalf("the message %+v", row)
	}
	if by, total, err := s.AsksWithdrawnBy(ctx, "team-send:1:a", 10, 200); err != nil || len(by) != 2 || total != 2 || by[0].Question != "Which branch?" {
		t.Fatalf("withdrawn by the message %+v %d %v", by, total, err)
	}
	// Another thread's asks stay open.
	open, err := s.OpenAsks(ctx)
	if err != nil || len(open) != 1 || open[0].ID != otherAsks[0].ID {
		t.Fatalf("open asks %+v %v", open, err)
	}
	if got, _ := s.Thread(ctx, other.ID); got.State != ThreadBlocked {
		t.Fatalf("another thread %+v", got)
	}
	// A withdrawn ask owes its session nothing: a shift's round waits for
	// no answer of it.
	if reports, answers, err := s.Owed(ctx, 9); err != nil || len(reports) != 0 || len(answers) != 0 {
		t.Fatalf("owed %+v %+v %v", reports, answers, err)
	}
	if undelivered, err := s.UndeliveredAnswerIDs(ctx); err != nil || len(undelivered) != 0 {
		t.Fatalf("undelivered %v %v", undelivered, err)
	}
	// Noted again, it withdraws nothing more.
	if _, change, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID); err != nil || change.Reopened || len(change.Withdrawn) != 0 {
		t.Fatalf("noted again %+v %v", change, err)
	}
}

// The owner's answer and a supervisor's message race: whichever commits
// first stands. An answer to a withdrawn ask is ErrWithdrawn, which is a
// conflict, and keeps nothing of the answer; a withdrawal after the answer
// withdraws nothing, and the thread opens all the same (ADR-0037).
func TestAnAnswerAndAWithdrawalRace(t *testing.T) {
	s, _ := open(t)
	// The withdrawal first.
	thread, asks := blockedThread(t, s, "delegate:1:c1", 9, "Which branch?")
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:a", ThreadID: thread.ID, SessionID: 9, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID); err != nil {
		t.Fatal(err)
	}
	row, replayed, err := s.AnswerAsk(ctx, "laptop:1", asks[0].ID, "main", "laptop")
	if !errors.Is(err, ErrWithdrawn) || !errors.Is(err, ErrConflict) || replayed || row.ID != asks[0].ID || row.ThreadID != thread.ID {
		t.Fatalf("an answer to a withdrawn ask %+v %v %v", row, replayed, err)
	}
	if got, _ := s.Ask(ctx, asks[0].ID); got.State != AskWithdrawn || got.Answer != "" || got.AnsweredAt != nil {
		t.Fatalf("the ask %+v", got)
	}
	// Its request id is not spent: tried again, it is refused the same.
	if _, _, err := s.AnswerAsk(ctx, "laptop:1", asks[0].ID, "main", "laptop"); !errors.Is(err, ErrWithdrawn) {
		t.Fatalf("tried again: %v", err)
	}
	// The answer first.
	thread2, asks2 := blockedThread(t, s, "delegate:1:c2", 10, "Squash?", "Rebase?")
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:b", ThreadID: thread2.ID, SessionID: 10, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerAsk(ctx, "laptop:2", asks2[0].ID, "yes", "laptop"); err != nil {
		t.Fatal(err)
	}
	got, change, err := s.SendDelivered(ctx, "team-send:1:b", thread2.ID)
	if err != nil || !change.Unblocked || got.State != ThreadOpen || len(change.Withdrawn) != 1 || change.Withdrawn[0].ID != asks2[1].ID {
		t.Fatalf("after an answer %+v %+v %v", got, change, err)
	}
	if answered, _ := s.Ask(ctx, asks2[0].ID); answered.State != AskAnswered || answered.Answer != "yes" || answered.WithdrawnBy != "" {
		t.Fatalf("the answered ask %+v", answered)
	}
	// The answer is still owed to the specialist, and goes.
	if _, answers, err := s.Owed(ctx, 10); err != nil || len(answers) != 1 || answers[0].ID != asks2[0].ID {
		t.Fatalf("owed %+v %v", answers, err)
	}
	// A blocked thread with no open ask left opens, and withdraws nothing.
	thread3, asks3 := blockedThread(t, s, "delegate:1:c3", 11, "Tag?")
	if _, _, err := s.AnswerAsk(ctx, "laptop:3", asks3[0].ID, "v1", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:c", ThreadID: thread3.ID, SessionID: 11, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, change, err := s.SendDelivered(ctx, "team-send:1:c", thread3.ID); err != nil || !change.Unblocked || len(change.Withdrawn) != 0 || got.State != ThreadOpen {
		t.Fatalf("no open ask %+v %+v %v", got, change, err)
	}
}

// The unblocking is migration 20, after the launches' 19 (ADR-0037): a
// work database at 19 keeps its asks and its outbox, which name no
// withdrawal. A message kept before the upgrade, whose text says nothing
// of unblocking, leaves a blocked thread blocked as it did then; one kept
// after unblocks it, at the store's clock (Codex review of #62, round 1).
func TestTheUnblockMigrationUpgradesADatabaseAt19(t *testing.T) {
	if SchemaVersion < 20 || !strings.Contains(migrations[18], "CREATE TABLE launches") || !strings.Contains(migrations[19], "withdrawn_by") {
		t.Fatalf("schema version %d: the unblocking is not migration 20, after the launches' 19", SchemaVersion)
	}
	path := filepath.Join(t.TempDir(), "work.sqlite")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{`CREATE TABLE schema_version (version INTEGER NOT NULL)`}
	steps = append(steps, migrations[:19]...)
	steps = append(steps, `INSERT INTO schema_version (version) VALUES (19)`,
		`INSERT INTO threads (id, key, title, agent, supervisor_session, session, task, state, report, owner, brief, briefed_at, created_at, updated_at)
			VALUES (1, 'delegate:1:c1', 'Fix CI', 'vera', 1, 9, 'Fix it', 'blocked', 'stuck', 'o1', 'the brief', 1, 1, 1)`,
		`INSERT INTO asks (id, key, thread_id, session_id, question, state, created_at) VALUES (1, 'r:ask:0', 1, 9, 'Which branch?', 'open', 1)`,
		`INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES ('team-send:1:a', 1, 9, 'x', 1)`)
	for i, step := range steps {
		if _, err := db.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	_ = db.Close()
	at := time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)
	s, err := Open(ctx, path, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version %d, %v", version, err)
	}
	if a, err := s.Ask(ctx, 1); err != nil || a.State != AskOpen || a.WithdrawnBy != "" || a.WithdrawnAt != nil {
		t.Fatalf("the ask after the upgrade: %+v, %v", a, err)
	}
	if r, err := s.Report(ctx, "team-send:1:a"); err != nil || r.UnblockedAt != nil || r.SentAt != nil {
		t.Fatalf("the message after the upgrade: %+v, %v", r, err)
	}
	// The message kept before the upgrade: the thread stays blocked, and
	// its ask waits.
	got, change, err := s.SendDelivered(ctx, "team-send:1:a", 1)
	if err != nil || change.Reopened || change.Unblocked || len(change.Withdrawn) != 0 || got.State != ThreadBlocked {
		t.Fatalf("a message kept before the upgrade %+v %+v %v", got, change, err)
	}
	if a, _ := s.Ask(ctx, 1); a.State != AskOpen {
		t.Fatalf("a message kept before the upgrade withdrew %+v", a)
	}
	// One kept after it unblocks the thread, at the store's clock.
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:b", ThreadID: 1, SessionID: 9, Text: "y"}); err != nil {
		t.Fatal(err)
	}
	got, change, err = s.SendDelivered(ctx, "team-send:1:b", 1)
	if err != nil || !change.Unblocked || got.State != ThreadOpen || len(change.Withdrawn) != 1 || change.Withdrawn[0].ID != 1 {
		t.Fatalf("delivered after the upgrade %+v %+v %v", got, change, err)
	}
	if w := change.Withdrawn[0].WithdrawnAt; w == nil || !w.Equal(at) {
		t.Fatalf("withdrawn at %v, want %v", w, at)
	}
	if r, _ := s.Report(ctx, "team-send:1:b"); r.UnblockedAt == nil || !r.UnblockedAt.Equal(at) {
		t.Fatalf("unblocked at %v, want %v", r.UnblockedAt, at)
	}
}

// The asks a message withdrew are read by their index.
func TestWithdrawnAsksUseTheirIndex(t *testing.T) {
	s, _ := open(t)
	if plan := planOf(t, s, `SELECT id FROM asks WHERE withdrawn_by = 'k' AND withdrawn_by != '' ORDER BY id`); !strings.Contains(plan, "asks_withdrawn") {
		t.Errorf("plan %q does not use asks_withdrawn", plan)
	}
}

// The asks a message withdrew are read for team_send's answer bounded,
// however many the thread had: as many as it names, each question cut, and
// the count of all (Codex review of #62, round 2).
func TestTheWithdrawnAsksReadIsBounded(t *testing.T) {
	s, _ := open(t)
	thread := sendThread(t, s, "delegate:1:c1", 9)
	for r := range 75 {
		asks := make([]AskRow, 4)
		for i := range asks {
			asks[i] = AskRow{Key: "r" + strconv.Itoa(r) + ":ask:" + strconv.Itoa(i), SessionID: 9, Question: strings.Repeat("é", 500)}
		}
		if _, _, _, err := s.FileReport(ctx, "r"+strconv.Itoa(r), thread.ID, ThreadBlocked, "stuck", asks, reportText); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:a", ThreadID: thread.ID, SessionID: 9, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, change, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID); err != nil || len(change.Withdrawn) != 300 {
		t.Fatalf("delivered: %d withdrawn, %v", len(change.Withdrawn), err)
	}
	by, total, err := s.AsksWithdrawnBy(ctx, "team-send:1:a", 108, 161)
	if err != nil || len(by) != 108 || total != 300 {
		t.Fatalf("read %d of %d, %v", len(by), total, err)
	}
	for _, a := range by {
		if n := len([]rune(a.Question)); n != 161 {
			t.Fatalf("A%d's question read with %d characters", a.ID, n)
		}
	}
}

// A question that holds a NUL is read for team_send's answer whole, as the
// noting gave it, and a long one cut to whole characters (Codex review of
// #62, round 3).
func TestAWithdrawnQuestionIsReadAsItWasAsked(t *testing.T) {
	s, _ := open(t)
	nul := "Use NUL\x00 or newline?"
	long := strings.Repeat("é", 300)
	_, asks := blockedThread(t, s, "delegate:1:c1", 9, nul, long)
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:a", ThreadID: asks[0].ThreadID, SessionID: 9, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, change, err := s.SendDelivered(ctx, "team-send:1:a", asks[0].ThreadID); err != nil || len(change.Withdrawn) != 2 || change.Withdrawn[0].Question != nul {
		t.Fatalf("delivered %+v %v", change, err)
	}
	by, total, err := s.AsksWithdrawnBy(ctx, "team-send:1:a", 10, 161)
	if err != nil || total != 2 || len(by) != 2 || by[0].Question != nul || by[1].Question != strings.Repeat("é", 161) {
		t.Fatalf("read %+v of %d, %v", by, total, err)
	}
}
