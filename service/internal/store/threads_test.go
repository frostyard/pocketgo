package store

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestThreadsLifecycle(t *testing.T) {
	s, _ := open(t)
	row, existed, err := s.CreateThread(ctx, ThreadRow{Key: "delegate:1:c1", Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "Fix it"})
	if err != nil || existed || row.State != ThreadStarting || row.ID == 0 || len(row.Owner) != 16 || strings.ToLower(row.Owner) != row.Owner {
		t.Fatalf("create %+v %v %v", row, existed, err)
	}
	// The same delegation finds the same thread.
	again, existed, err := s.CreateThread(ctx, ThreadRow{Key: "delegate:1:c1", Title: "other", Agent: "x", SupervisorSession: 1, Task: "x"})
	if err != nil || !existed || again.ID != row.ID || again.Title != "Fix CI" || again.Owner != row.Owner {
		t.Fatalf("retry %+v %v %v", again, existed, err)
	}
	if n, _ := s.OpenThreads(ctx); n != 1 {
		t.Fatalf("open threads %d", n)
	}
	attached, err := s.AttachThread(ctx, row.ID, 9, "/w--vera-fix-ci", "vera/fix-ci", "the brief")
	if err != nil || attached.State != ThreadStarting || attached.Session != 9 || attached.Branch != "vera/fix-ci" {
		t.Fatalf("attach %+v %v", attached, err)
	}
	// Another session never takes it over.
	if other, _ := s.AttachThread(ctx, row.ID, 10, "/x", "", ""); other.Session != 9 {
		t.Fatalf("a second attach moved it to %+v", other)
	}
	if got, err := s.ThreadOfSession(ctx, 9); err != nil || got.ID != row.ID {
		t.Fatalf("thread of session %+v %v", got, err)
	}
	if opened, err := s.OpenThread(ctx, row.ID); err != nil || opened.State != ThreadOpen {
		t.Fatalf("open %+v %v", opened, err)
	}
	got, asks, _, err := s.FileReport(ctx, "r1", row.ID, ThreadDone, "PR #3 is open", nil, reportText)
	if err != nil || got.State != ThreadDone || got.Report != "PR #3 is open" || len(asks) != 0 {
		t.Fatalf("report %+v %v %v", got, asks, err)
	}
	if got, err := s.CloseThread(ctx, row.ID); err != nil || got.State != ThreadClosed {
		t.Fatalf("close %+v %v", got, err)
	}
	if n, _ := s.OpenThreads(ctx); n != 0 {
		t.Fatalf("a closed thread is on the desk: %d", n)
	}
	// A closed thread takes no report, and keeps no ask of one.
	if _, _, _, err := s.FileReport(ctx, "r2", row.ID, ThreadDone, "late", []AskRow{{Key: "r2:ask:0", SessionID: 9, Question: "q"}}, reportText); !errors.Is(err, ErrConflict) {
		t.Fatalf("report after close: %v", err)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 0 {
		t.Fatalf("a refused report left asks %+v", open)
	}
	// Nor does it open again.
	if got, _ := s.OpenThread(ctx, row.ID); got.State != ThreadClosed {
		t.Fatalf("a closed thread opened: %+v", got)
	}
}

// A report filed once is never filed again: a report run again after a
// newer one leaves the newer one, and returns the asks it made.
func TestAReportIsFiledOnce(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.AttachThread(ctx, row.ID, 9, "/w", "", "b")
	// A report while the thread starts is taken: the brief went.
	first, asks, message, err := s.FileReport(ctx, "r1", row.ID, ThreadOpen, "halfway",
		[]AskRow{{Key: "r1:ask:0", SessionID: 9, Question: "Squash?", Options: []string{"yes", "no"}}}, reportText)
	if err != nil || first.State != ThreadOpen || len(asks) != 1 || asks[0].Question != "Squash?" || asks[0].ThreadID != row.ID {
		t.Fatalf("first %+v %+v %v", first, asks, err)
	}
	if message.SessionID != 1 || message.Text != fmt.Sprintf("asks %d", asks[0].ID) || message.SentAt != nil {
		t.Fatalf("message %+v", message)
	}
	if _, _, _, err := s.FileReport(ctx, "r2", row.ID, ThreadDone, "done", nil, reportText); err != nil {
		t.Fatal(err)
	}
	again, replayed, same, err := s.FileReport(ctx, "r1", row.ID, ThreadOpen, "halfway", []AskRow{{Key: "r1:ask:0", SessionID: 9, Question: "Squash?"}}, reportText)
	if err != nil || again.State != ThreadDone || again.Report != "done" || len(replayed) != 1 || replayed[0].ID != asks[0].ID || same.Text != message.Text {
		t.Fatalf("a replay %+v %+v %+v %v", again, replayed, same, err)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 1 {
		t.Fatalf("asks %+v", open)
	}
	if _, _, _, err := s.FileReport(ctx, "r3", 99, ThreadDone, "x", nil, reportText); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown thread: %v", err)
	}
	// Each message waits to be sent, the oldest first, until it went.
	unsent, err := s.UnsentReports(ctx)
	if err != nil || len(unsent) != 2 || unsent[0].Key != "r1" || unsent[1].Key != "r2" {
		t.Fatalf("unsent %+v %v", unsent, err)
	}
	if err := s.ReportSent(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if unsent, _ := s.UnsentReports(ctx); len(unsent) != 1 || unsent[0].Key != "r2" {
		t.Fatalf("unsent after one went %+v", unsent)
	}
	// A withdrawn submission is noted, and the next takes a new attempt.
	if err := s.ReportSubmitted(ctx, "r2", 41); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ReportWithdrawn(ctx, "r2", 3); err != nil || changed {
		t.Fatalf("a withdrawal of another attempt changed it: %v %v", changed, err)
	}
	if changed, err := s.ReportWithdrawn(ctx, "r2", 0); err != nil || !changed {
		t.Fatalf("withdrawn %v %v", changed, err)
	}
	if mine, err := s.UnsentReportsOf(ctx, row.ID); err != nil || len(mine) != 1 || mine[0].Submission != 0 || mine[0].Attempt != 1 {
		t.Fatalf("unsent of the thread %+v %v", mine, err)
	}
	if other, _ := s.UnsentReportsOf(ctx, 99); len(other) != 0 {
		t.Fatalf("another thread's %+v", other)
	}
	if oldest, ok, err := s.OldestUnsentReport(ctx, row.ID, 1); err != nil || !ok || oldest.Key != "r2" {
		t.Fatalf("oldest %+v %v %v", oldest, ok, err)
	}
	if threads, err := s.ThreadsWithUnsentReports(ctx); err != nil || !slices.Equal(threads, []int64{row.ID}) {
		t.Fatalf("threads %v %v", threads, err)
	}
	_ = s.ReportSent(ctx, "r2")
	if _, ok, err := s.OldestUnsentReport(ctx, row.ID, 1); err != nil || ok {
		t.Fatalf("a report waits after all went: %v %v", ok, err)
	}
}

// The owner's close claims its request id in the transaction that closes:
// an id used for another thread closes nothing.
func TestCloseThreadAsClaimsItsRequest(t *testing.T) {
	s, _ := open(t)
	one, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	two, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if got, err := s.CloseThreadAs(ctx, "u:1", one.ID); err != nil || got.State != ThreadClosed {
		t.Fatalf("close %+v %v", got, err)
	}
	if got, err := s.CloseThreadAs(ctx, "u:1", one.ID); err != nil || got.State != ThreadClosed {
		t.Fatalf("a retry %+v %v", got, err)
	}
	if _, err := s.CloseThreadAs(ctx, "u:1", two.ID); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("a reused id: %v", err)
	}
	if got, _ := s.Thread(ctx, two.ID); got.State != ThreadStarting {
		t.Fatalf("a reused id closed %+v", got)
	}
	if _, err := s.CloseThreadAs(ctx, "u:2", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown thread: %v", err)
	}
	if _, _, err := s.LookupRequest(ctx, "u:2", "closeThread:99"); err != nil {
		t.Fatal(err)
	}
}

// A thread that could not start leaves the desk with why; one that started
// is never failed.
func TestFailThread(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if got, err := s.ThreadByKey(ctx, "k1"); err != nil || got.ID != row.ID {
		t.Fatalf("by key %+v %v", got, err)
	}
	failed, err := s.FailThread(ctx, row.ID, "no checkout")
	if err != nil || failed.State != ThreadClosed || failed.Report != "no checkout" || failed.Session != 0 {
		t.Fatalf("fail %+v %v", failed, err)
	}
	other, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.AttachThread(ctx, other.ID, 5, "/w", "", "b")
	_, _ = s.OpenThread(ctx, other.ID)
	if got, _ := s.FailThread(ctx, other.ID, "late"); got.State != ThreadOpen || got.Report != "" {
		t.Fatalf("a started thread failed: %+v", got)
	}
	if _, err := s.ThreadByKey(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
}

// A thread whose brief never reached its specialist fails, open or
// starting; one whose brief was noted, or that was reported on, never does.
func TestFailUnbriefed(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.AttachThread(ctx, row.ID, 5, "/w", "", "b")
	_, _ = s.OpenThread(ctx, row.ID)
	if got, err := s.FailUnbriefed(ctx, row.ID, "moved"); err != nil || got.State != ThreadClosed || got.Report != "moved" {
		t.Fatalf("an open thread without its brief: %+v %v", got, err)
	}
	briefed, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.AttachThread(ctx, briefed.ID, 6, "/w", "", "b")
	_ = s.Briefed(ctx, briefed.ID)
	if got, _ := s.FailUnbriefed(ctx, briefed.ID, "moved"); got.State != ThreadStarting || got.Report != "" {
		t.Fatalf("a briefed thread failed: %+v", got)
	}
	reported, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k3", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.AttachThread(ctx, reported.ID, 7, "/w", "", "b")
	if _, _, _, err := s.FileReport(ctx, "r1", reported.ID, ThreadDone, "done", nil, func([]int64) string { return "done" }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.FailUnbriefed(ctx, reported.ID, "moved"); got.State != ThreadDone {
		t.Fatalf("a reported thread failed: %+v", got)
	}
}

func TestAsksAreAnsweredOnce(t *testing.T) {
	s, _ := open(t)
	thread, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	ask, err := s.CreateAsk(ctx, AskRow{Key: "message:9:c2:0", ThreadID: thread.ID, SessionID: 9, Question: "Squash?", Options: []string{"yes", "no"}})
	if err != nil || ask.State != AskOpen || !slices.Equal(ask.Options, []string{"yes", "no"}) {
		t.Fatalf("create %+v %v", ask, err)
	}
	if again, _ := s.CreateAsk(ctx, AskRow{Key: "message:9:c2:0", ThreadID: thread.ID, SessionID: 9, Question: "other"}); again.ID != ask.ID {
		t.Fatalf("a retried ask made %+v", again)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 1 {
		t.Fatalf("open asks %+v", open)
	}
	answered, replayed, err := s.AnswerAsk(ctx, "r1", ask.ID, "yes", "laptop")
	if err != nil || replayed || answered.State != AskAnswered || answered.Answer != "yes" || answered.AnsweredAt == nil {
		t.Fatalf("answer %+v %v %v", answered, replayed, err)
	}
	if again, replayed, err := s.AnswerAsk(ctx, "r1", ask.ID, "yes", "laptop"); err != nil || !replayed || again.Answer != "yes" {
		t.Fatalf("replay %+v %v %v", again, replayed, err)
	}
	if _, _, err := s.AnswerAsk(ctx, "r2", ask.ID, "no", "laptop"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second answer: %v", err)
	}
	if _, _, err := s.AnswerAsk(ctx, "r3", 999, "no", "laptop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown ask: %v", err)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 0 {
		t.Fatalf("an answered ask is open: %+v", open)
	}
	if err := s.AnswerSubmitted(ctx, ask.ID, 17); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.AnswerWithdrawn(ctx, ask.ID, 0); err != nil || !changed {
		t.Fatalf("withdrawn %v %v", changed, err)
	}
	if changed, _ := s.AnswerWithdrawn(ctx, ask.ID, 0); changed {
		t.Fatal("a second withdrawal of the same submission changed it again")
	}
	if got, _ := s.Ask(ctx, ask.ID); got.Submission != 0 || got.Attempt != 1 {
		t.Fatalf("a withdrawn answer %+v", got)
	}
	// The answer waits for delivery until the asking session has it.
	if waiting, err := s.UndeliveredAnswers(ctx); err != nil || len(waiting) != 1 || waiting[0].ID != ask.ID {
		t.Fatalf("undelivered %+v %v", waiting, err)
	}
	if err := s.AnswerDelivered(ctx, ask.ID); err != nil {
		t.Fatal(err)
	}
	if waiting, _ := s.UndeliveredAnswers(ctx); len(waiting) != 0 {
		t.Fatalf("a delivered answer waits: %+v", waiting)
	}
}

// reportText is a supervisor's message naming the asks' ids.
func reportText(asks []int64) string {
	return fmt.Sprintf("asks %v", strings.Trim(fmt.Sprint(asks), "[]"))
}

// A thread's brief is followed until it is in the transcript, and a
// starting thread until it starts.
func TestThreadsToFollow(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x", TaskID: 42})
	if row.TaskID != 42 {
		t.Fatalf("task id %+v", row)
	}
	follow := func() []int64 {
		t.Helper()
		rows, err := s.ThreadsToFollow(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if got := follow(); len(got) != 1 {
		t.Fatalf("a starting thread: %v", got)
	}
	_, _ = s.AttachThread(ctx, row.ID, 9, "/w", "", "Do it.")
	_, _ = s.OpenThread(ctx, row.ID)
	if got := follow(); len(got) != 1 {
		t.Fatalf("an open thread whose brief is not placed: %v", got)
	}
	if err := s.BriefSubmitted(ctx, row.ID, 5); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.BriefWithdrawn(ctx, row.ID, 0); err != nil || !changed {
		t.Fatalf("withdrawn %v %v", changed, err)
	}
	got, _ := s.Thread(ctx, row.ID)
	if got.Brief != "Do it." || got.BriefSubmission != 0 || got.BriefAttempt != 1 || got.Briefed {
		t.Fatalf("thread %+v", got)
	}
	if err := s.Briefed(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if got := follow(); len(got) != 0 {
		t.Fatalf("a briefed open thread is followed: %v", got)
	}
}

// planOf is SQLite's query plan of query, its details joined.
func planOf(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, "; ")
}

// A delivery pass, a report's thread lookup and the desk's count read
// their partial indexes, never the whole history of closed threads.
func TestThreadQueriesUseTheirIndexes(t *testing.T) {
	s, _ := open(t)
	for query, index := range map[string]string{
		`SELECT ` + threadColumns + ` FROM threads WHERE ` + followPredicate + ` ORDER BY id`: "threads_open",
		`SELECT ` + threadColumns + ` FROM threads WHERE session = 5 AND session != 0`:        "threads_session",
		`SELECT COUNT(*) FROM threads WHERE state != 'closed'`:                                "threads_open",
	} {
		if plan := planOf(t, s, query); !strings.Contains(plan, index) {
			t.Errorf("%s: plan %q does not use %s", query, plan, index)
		}
	}
}

// A close's request records that the close committed; a reservation alone
// has no result.
func TestACloseRecordsItsRequest(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if _, _, err := s.ClaimRequest(ctx, "u:1", "closeThread:1"); err != nil {
		t.Fatal(err)
	}
	if result, _, _ := s.LookupRequest(ctx, "u:1", "closeThread:1"); result != "" {
		t.Fatalf("a reservation's result %q", result)
	}
	if _, err := s.CloseThreadAs(ctx, "u:1", row.ID); err != nil {
		t.Fatal(err)
	}
	if result, _, _ := s.LookupRequest(ctx, "u:1", "closeThread:1"); result != RequestClosed {
		t.Fatalf("a close's result %q", result)
	}
}

// A release frees only a reservation that did not complete, of its own
// kind: one tried again late never frees a request that took effect.
func TestReleaseRequestFreesOnlyAnUnfinishedReservation(t *testing.T) {
	s, _ := open(t)
	if _, _, err := s.ClaimRequest(ctx, "k1", "closeThread:1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseRequest(ctx, "k1", "closeThread:2"); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := s.LookupRequest(ctx, "k1", "closeThread:1"); !held {
		t.Fatal("a release of another kind freed the key")
	}
	if err := s.ReleaseRequest(ctx, "k1", "closeThread:1"); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := s.LookupRequest(ctx, "k1", "closeThread:1"); held {
		t.Fatal("an unfinished reservation was kept")
	}
	thread, _, _ := s.CreateThread(ctx, ThreadRow{Key: "t", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if _, err := s.CloseThreadAs(ctx, "k2", thread.ID); err != nil {
		t.Fatal(err)
	}
	kind := "closeThread:" + strconv.FormatInt(thread.ID, 10)
	if err := s.ReleaseRequest(ctx, "k2", kind); err != nil {
		t.Fatal(err)
	}
	if result, held, _ := s.LookupRequest(ctx, "k2", kind); !held || result != RequestClosed {
		t.Fatalf("a completed close was freed: %q %v", result, held)
	}
}

// A review thread closes once, and its message to the author is kept
// exactly then, once; a thread that is no review thread does not close
// this way (ADR-0019).
func TestCloseReview(t *testing.T) {
	s, _ := open(t)
	author, _, err := s.CreateThread(ctx, ThreadRow{Key: "d1", Title: "Work", Agent: "duncan", SupervisorSession: 1, Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateThread(ctx, ThreadRow{Key: "review:a/b#1:" + strings.Repeat("a", 40), Title: "Cross-review", Agent: "duncan",
		SupervisorSession: 1, Task: "", ReviewPull: 7, ReviewHead: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReviewThreadsOf(ctx, 7); err != nil || len(got) != 1 || got[0].ID != review.ID || got[0].ReviewHead != strings.Repeat("a", 40) {
		t.Fatalf("review threads %+v, %v", got, err)
	}
	msg := &ReportRow{Key: "review-verdict:1", ThreadID: review.ID, SessionID: 42, Text: "ready"}
	row, changed, err := s.CloseReview(ctx, review.ID, "Verdict R1: ready.", msg)
	if err != nil || !changed || row.State != ThreadClosed || row.Report != "Verdict R1: ready." {
		t.Fatalf("close: %+v, %v, %v", row, changed, err)
	}
	if _, changed, err := s.CloseReview(ctx, review.ID, "again", &ReportRow{Key: "review-verdict:2", ThreadID: review.ID, SessionID: 42, Text: "x"}); err != nil || changed {
		t.Fatalf("a second close: %v, %v", changed, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE thread_id = ?`, review.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reports kept %d, %v", n, err)
	}
	if _, changed, err := s.CloseReview(ctx, author.ID, "no", nil); err != nil || changed {
		t.Fatalf("an author thread closed as a review: %v, %v", changed, err)
	}
}

// A review is recorded only while its pull request, as stored, is open, no
// draft and at the head it reviews (Codex review of #26, round 2).
func TestCreateReviewThreadChecksThePull(t *testing.T) {
	s, _ := open(t)
	p := linkedPull(t, s)
	row := ThreadRow{Key: "review:a", Title: "Cross-review", Agent: "vera", SupervisorSession: 1, Task: "t", ReviewPull: p.ID, ReviewHead: "other"}
	if _, _, err := s.CreateReviewThread(ctx, row, p.Base, p.ReadSeq); !errors.Is(err, ErrConflict) {
		t.Fatalf("a review of a head that is not the pull request's: %v", err)
	}
	row.ReviewHead = p.HeadSHA
	if _, err := s.db.ExecContext(ctx, `UPDATE pull_requests SET draft = 1 WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReviewThread(ctx, row, p.Base, p.ReadSeq); !errors.Is(err, ErrConflict) {
		t.Fatalf("a review of a draft: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE pull_requests SET draft = 0 WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReviewThread(ctx, row, "release", p.ReadSeq); !errors.Is(err, ErrConflict) {
		t.Fatalf("a review planned on another base branch: %v", err)
	}
	// A later read, which may have failed, was recorded since the one the
	// review was planned from (Codex review of #26, round 10).
	if _, _, err := s.CreateReviewThread(ctx, row, p.Base, p.ReadSeq-1); !errors.Is(err, ErrConflict) {
		t.Fatalf("a review planned from an older read: %v", err)
	}
	if got, _, err := s.CreateReviewThread(ctx, row, p.Base, p.ReadSeq); err != nil || got.ReviewPull != p.ID {
		t.Fatalf("a review of the pull request as it is: %+v, %v", got, err)
	}
}

// A review's base is no wire field: keeping it changes no thread's
// updated_at (Codex review of #26, round 3).
func TestSetReviewBaseKeepsUpdatedAt(t *testing.T) {
	s, _ := open(t)
	p := linkedPull(t, s)
	row, _, err := s.CreateReviewThread(ctx, ThreadRow{Key: "review:a", Title: "Cross-review", Agent: "vera", SupervisorSession: 1, Task: "t",
		ReviewPull: p.ID, ReviewHead: p.HeadSHA}, p.Base, p.ReadSeq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET updated_at = 5 WHERE id = ?`, row.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetReviewBase(ctx, row.ID, strings.Repeat("e", 40))
	if err != nil || got.ReviewBase != strings.Repeat("e", 40) || got.UpdatedAt.UnixMilli() != 5 {
		t.Fatalf("after SetReviewBase: %+v, %v", got, err)
	}
}

// A thread names one worker, only while it is open; its replies are kept
// once each, and none for a closed thread (ADR-0021).
func TestAThreadKeepsOneWorker(t *testing.T) {
	s, _ := open(t)
	thread, _, err := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ThreadOfWorker(ctx, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session 0: %v", err)
	}
	worker := func(id int64) SessionRow {
		return SessionRow{ID: id, Title: "T1 · worker", Route: "worker-default", CreatedBy: "team"}
	}
	row, err := s.RecordWorkerSession(ctx, "worker:x", worker(70), thread.ID)
	if err != nil || row.WorkerSession != 70 {
		t.Fatalf("first worker %+v, %v", row, err)
	}
	if got, err := s.Session(ctx, 70); err != nil || got.Route != "worker-default" {
		t.Fatalf("the worker's session row %+v, %v", got, err)
	}
	if row, err = s.RecordWorkerSession(ctx, "worker:x", worker(70), thread.ID); err != nil || row.WorkerSession != 70 {
		t.Fatalf("the same worker again %+v, %v", row, err)
	}
	if _, err := s.RecordWorkerSession(ctx, "worker:y", worker(71), thread.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second worker: %v", err)
	}
	if _, err := s.Session(ctx, 71); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused worker's session row was kept: %v", err)
	}
	if got, err := s.ThreadOfWorker(ctx, 70); err != nil || got.ID != thread.ID {
		t.Fatalf("ThreadOfWorker %+v, %v", got, err)
	}
	reply := ReportRow{Key: "worker-reply:x:1", ThreadID: thread.ID, SessionID: 5, Text: "[T1 · worker] done"}
	for range 2 {
		if queued, err := s.QueueWorkerReport(ctx, reply); err != nil || !queued {
			t.Fatalf("queue %v, %v", queued, err)
		}
	}
	if unsent, _ := s.UnsentReportsOf(ctx, thread.ID); len(unsent) != 1 || unsent[0].SessionID != 5 {
		t.Fatalf("unsent %+v", unsent)
	}
	if _, err := s.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	reply.Key = "worker-reply:x:2"
	if queued, err := s.QueueWorkerReport(ctx, reply); err != nil || queued {
		t.Fatalf("a closed thread's reply: %v, %v", queued, err)
	}
	if _, err := s.RecordWorkerSession(ctx, "worker:z", worker(72), thread.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a closed thread's worker: %v", err)
	}
}
