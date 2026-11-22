package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// questionServer is a fake with session 5 waiting on question q-a, and a
// client of it.
func questionServer(t *testing.T, q proto.Question) (*Server, *proto.Client) {
	t.Helper()
	s := New()
	s.AddSession(proto.Session{ID: 5, State: proto.SessionWaiting})
	q.ID, q.SessionID, q.CallID = "q-a", 5, "c1"
	if q.Deadline.IsZero() {
		q.Deadline = Now.Add(30 * time.Minute)
	}
	s.PutQuestion(q)
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	return s, client
}

// errCode is the code of err, a refusal of the fake's.
func errCode(err error) proto.ErrorCode {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// An answer checks its request id's receipt, settles its question and
// keeps its reply under one lock, as pocketd's transaction does: retries
// of one id at once all get the first reply, never a conflict (Codex
// review of #95, round 2).
func TestAQuestionsAnswerClaimsItsRequestIDAtomically(t *testing.T) {
	s, client := questionServer(t, proto.Question{Question: "Which?", Options: []string{"Go", "Snow"}})
	looked, gap := false, false
	s.afterReplayLookup = func() {
		looked = true
		// Held by the answer, the lock cannot be taken here.
		if s.mu.TryLock() {
			gap = true
			s.mu.Unlock()
		}
	}
	const tries = 16
	var wg sync.WaitGroup
	got := make([]proto.Question, tries)
	errs := make([]error, tries)
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = client.AnswerQuestion(context.Background(), 5, "q-a", proto.AnswerQuestion{RequestID: "r1", Option: []int{1}})
		}()
	}
	wg.Wait()
	if !looked || gap {
		t.Fatalf("the request id's lookup was not under the answer's lock (looked %v, gap %v)", looked, gap)
	}
	for i := range tries {
		if errs[i] != nil || got[i].State != proto.QuestionAnswered || got[i].Answer != "Snow" {
			t.Fatalf("try %d: %+v, %v", i, got[i], errs[i])
		}
	}
}

// A question past its deadline on the fake's clock is skipped for its
// time when an answer comes, with its session's event: the answer is
// conflict with pocketd's words, and its request id stays free (Codex
// review of #95, round 2).
func TestTheFakeSkipsAQuestionPastItsDeadline(t *testing.T) {
	s, client := questionServer(t, proto.Question{Question: "Which?", Deadline: Now})
	seq := s.Seq()
	_, err := client.AnswerQuestion(context.Background(), 5, "q-a", proto.AnswerQuestion{RequestID: "r1", Answer: "Go"})
	if errCode(err) != proto.ErrorCodeConflict || err.Error() != "conflict: the question was skipped already: no answer came in time" {
		t.Fatalf("an answer past the deadline: %v", err)
	}
	q, _ := s.Question("q-a")
	if q.State != proto.QuestionSkipped || q.Reason != proto.SkippedTimeout || q.SettledBy != "pocketd" || q.SettledAt == nil {
		t.Errorf("the question %+v", q)
	}
	d, _ := s.Session(5)
	if d.Session.Question != nil || d.Session.State != proto.SessionRunning || s.Seq() == seq {
		t.Errorf("the session %+v, events %d to %d", d.Session, seq, s.Seq())
	}
	s.mu.Lock()
	_, kept := s.replies["r1"]
	s.mu.Unlock()
	if kept {
		t.Error("the refused answer's request id is kept")
	}
}

// The fake checks an answer's body as pocketd does (team.CheckAnswer):
// options in any order, none twice, stored sorted; a skip with no answer
// at all, white space included; options alone answer with their texts
// (Codex review of #95, round 2).
func TestTheFakeChecksAnswersAsPocketdDoes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		multiple bool
		options  []string
		body     proto.AnswerQuestion
		code     proto.ErrorCode
		answer   string
		option   []int
	}{
		{"options in any order", true, []string{"Mon", "Wed", "Fri"}, proto.AnswerQuestion{Option: []int{2, 0}}, "", "Mon; Fri", []int{0, 2}},
		{"an option twice", true, []string{"Mon", "Wed"}, proto.AnswerQuestion{Option: []int{1, 1}}, proto.ErrorCodeBadRequest, "", nil},
		{"two of one", false, []string{"Mon", "Wed"}, proto.AnswerQuestion{Option: []int{0, 1}}, proto.ErrorCodeBadRequest, "", nil},
		{"an option of none", false, nil, proto.AnswerQuestion{Option: []int{0}}, proto.ErrorCodeBadRequest, "", nil},
		{"no such option", false, []string{"Mon"}, proto.AnswerQuestion{Option: []int{1}}, proto.ErrorCodeBadRequest, "", nil},
		{"a skip with white space", false, nil, proto.AnswerQuestion{Skipped: true, Answer: "  "}, proto.ErrorCodeBadRequest, "", nil},
		{"a skip with an option", false, []string{"Mon"}, proto.AnswerQuestion{Skipped: true, Option: []int{0}}, proto.ErrorCodeBadRequest, "", nil},
		{"nothing", false, nil, proto.AnswerQuestion{Answer: " "}, proto.ErrorCodeBadRequest, "", nil},
		{"words and an option", false, []string{"Mon"}, proto.AnswerQuestion{Answer: " Mon, early ", Option: []int{0}}, "", "Mon, early", []int{0}},
		{"a skip", false, nil, proto.AnswerQuestion{Skipped: true}, "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client := questionServer(t, proto.Question{Question: "When?", Options: tc.options, Multiple: tc.multiple})
			tc.body.RequestID = "r1"
			got, err := client.AnswerQuestion(context.Background(), 5, "q-a", tc.body)
			if tc.code != "" {
				if errCode(err) != tc.code {
					t.Fatalf("%v, want %s", err, tc.code)
				}
				if q, _ := s.Question("q-a"); q.State != proto.QuestionWaiting {
					t.Errorf("a refused body settled the question: %+v", q)
				}
				return
			}
			if err != nil || got.Answer != tc.answer || !slices.Equal(got.Option, tc.option) {
				t.Fatalf("%+v, %v; want %q %v", got, err, tc.answer, tc.option)
			}
		})
	}
}
