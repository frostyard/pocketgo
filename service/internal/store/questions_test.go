package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// A question is kept by its call: the same call finds it, waiting or
// settled, and a session has one waiting at a time, which the database
// holds too (ADR-0048).
func TestAQuestionIsKeptByItsCall(t *testing.T) {
	s, _ := open(t)
	deadline := time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)
	row := QuestionRow{SessionID: 2, TaskID: 7, CallID: "q1", Question: "Which?", Options: []string{"a", "b"}, Multiple: true, Placeholder: "or say", Deadline: deadline}
	first, created, err := s.PutQuestion(ctx, row)
	if err != nil || !created || first.ID != QuestionID(7, "q1") || first.State != QuestionWaiting || !slices.Equal(first.Options, []string{"a", "b"}) ||
		!first.Multiple || first.Placeholder != "or say" || !first.Deadline.Equal(deadline) || first.SettledAt != nil {
		t.Fatalf("PutQuestion = %+v, %v, %v", first, created, err)
	}
	if again, created, err := s.PutQuestion(ctx, QuestionRow{SessionID: 2, TaskID: 7, CallID: "q1", Question: "Other?", Deadline: deadline}); err != nil || created || again.Question != "Which?" {
		t.Fatalf("the same call again: %+v, %v, %v", again, created, err)
	}
	if _, _, err := s.PutQuestion(ctx, QuestionRow{SessionID: 2, TaskID: 7, CallID: "q2", Question: "Second?", Deadline: deadline}); !errors.Is(err, ErrQuestionWaits) {
		t.Fatalf("a second waiting question: %v", err)
	}
	// The database refuses two waiting questions of a session too.
	if _, err := s.db.Exec(`INSERT INTO questions (id, session_id, task_id, call_id, question, state, created_at, deadline) VALUES ('qx', 2, 8, 'x', 'x', 'waiting', 0, 0)`); err == nil {
		t.Fatal("the database kept two waiting questions of a session")
	}
	if _, _, err := s.PutQuestion(ctx, QuestionRow{SessionID: 3, TaskID: 9, CallID: "q1", Question: "Elsewhere?", Deadline: deadline}); err != nil {
		t.Fatalf("another session's question: %v", err)
	}
	waiting, err := s.WaitingQuestions(ctx)
	if err != nil || len(waiting) != 2 || waiting[2].ID != first.ID {
		t.Fatalf("WaitingQuestions = %+v, %v", waiting, err)
	}
	if got, err := s.WaitingQuestion(ctx, 2); err != nil || got.ID != first.ID {
		t.Fatalf("WaitingQuestion = %+v, %v", got, err)
	}
	if _, err := s.WaitingQuestion(ctx, 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session with none: %v", err)
	}
	if _, err := s.Question(ctx, "qnone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no question: %v", err)
	}
}

// The owner's answer settles a question once, under its request key: the
// same key answers with the question as it is, another is conflict, a
// key of another request is refused, and an answer to a question of
// another session finds none. A skip keeps no answer.
func TestTheOwnerAnswersAQuestionOnce(t *testing.T) {
	s, _ := open(t)
	q, _, err := s.PutQuestion(ctx, QuestionRow{SessionID: 2, TaskID: 7, CallID: "q1", Question: "Which?", Options: []string{"a", "b"}, Deadline: time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerQuestion(ctx, "u:d:r1", 3, q.ID, "a", []int{0}, false, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another session's answer: %v", err)
	}
	answered, replayed, err := s.AnswerQuestion(ctx, "u:d:r1", 2, q.ID, "a", []int{0}, false, "laptop")
	if err != nil || replayed || answered.State != QuestionAnswered || answered.Answer != "a" || !slices.Equal(answered.Chosen, []int{0}) ||
		answered.SettledBy != "laptop" || answered.SettledAt == nil {
		t.Fatalf("AnswerQuestion = %+v, %v, %v", answered, replayed, err)
	}
	if again, replayed, err := s.AnswerQuestion(ctx, "u:d:r1", 2, q.ID, "b", []int{1}, false, "laptop"); err != nil || !replayed || again.Answer != "a" {
		t.Fatalf("the same request again: %+v, %v, %v", again, replayed, err)
	}
	if _, _, err := s.AnswerQuestion(ctx, "u:p:r1", 2, q.ID, "b", nil, false, "phone"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second answer: %v", err)
	}
	// The refused answer left its key free.
	if _, existed, err := s.LookupRequest(ctx, "u:p:r1", "question:"+q.ID); err != nil || existed {
		t.Fatalf("a refused answer kept its key: %v %v", existed, err)
	}
	if _, err := s.SkipQuestion(ctx, q.ID, "timeout"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a timeout after the answer: %v", err)
	}
	other, _, err := s.PutQuestion(ctx, QuestionRow{SessionID: 2, TaskID: 8, CallID: "q2", Question: "Go?", Deadline: time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerQuestion(ctx, "u:d:r1", 2, other.ID, "yes", nil, false, "laptop"); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("a key of another request: %v", err)
	}
	skipped, _, err := s.AnswerQuestion(ctx, "u:d:r2", 2, other.ID, "ignored", []int{0}, true, "laptop")
	if err != nil || skipped.State != QuestionSkipped || skipped.Reason != "owner" || skipped.Answer != "" || len(skipped.Chosen) != 0 {
		t.Fatalf("a skip: %+v, %v", skipped, err)
	}
}

// pocketd skips a question that waits, with its reason, once.
func TestPocketdSkipsAWaitingQuestion(t *testing.T) {
	s, _ := open(t)
	q, _, err := s.PutQuestion(ctx, QuestionRow{SessionID: 2, TaskID: 7, CallID: "q1", Question: "Still?", Deadline: time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.SkipQuestion(ctx, q.ID, "timeout")
	if err != nil || row.State != QuestionSkipped || row.Reason != "timeout" || row.SettledBy != "pocketd" || row.SettledAt == nil {
		t.Fatalf("SkipQuestion = %+v, %v", row, err)
	}
	if _, err := s.SkipQuestion(ctx, q.ID, "aborted"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second skip: %v", err)
	}
	if _, err := s.SkipQuestion(ctx, "qnone", "aborted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no question: %v", err)
	}
	// A settled question frees its session for the next.
	if _, created, err := s.PutQuestion(ctx, QuestionRow{SessionID: 2, TaskID: 7, CallID: "q2", Question: "Next?", Deadline: time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)}); err != nil || !created {
		t.Fatalf("the next question: %v %v", created, err)
	}
}
