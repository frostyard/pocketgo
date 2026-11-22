package pocketdtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// PutQuestion puts q, waiting, on its session, without an event: the
// session's Question, and its state waiting while it waits (ADR-0048). A
// question settled replaces the one it settles there and leaves the
// session running. A session event a test emits with a question puts it
// too.
func (s *Server) PutQuestion(q proto.Question) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q.State == "" {
		q.State = proto.QuestionWaiting
	}
	s.questions[q.ID] = q
	sess := s.sessions[q.SessionID]
	if sess == nil {
		return
	}
	if q.State == proto.QuestionWaiting {
		copied := q
		sess.detail.Session.Question, sess.detail.Session.State = &copied, proto.SessionWaiting
		return
	}
	if cur := sess.detail.Session.Question; cur != nil && cur.ID == q.ID {
		sess.detail.Session.Question, sess.detail.Session.State = nil, proto.SessionRunning
	}
}

// Question is the server's copy of question id, waiting or settled.
func (s *Server) Question(id string) (proto.Question, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.questionLocked(id)
	return q, ok
}

// questionLocked is question id: one put, or one a session event carried.
func (s *Server) questionLocked(id string) (proto.Question, bool) {
	if q, ok := s.questions[id]; ok {
		return q, true
	}
	for _, sess := range s.sessions {
		if q := sess.detail.Session.Question; q != nil && q.ID == id {
			return *q, true
		}
	}
	return proto.Question{}, false
}

// answerQuestion answers or skips a session's question once, as the
// service does (protocol rule 62), checking the request id's receipt,
// changing the question and keeping its reply under one lock, as pocketd's
// transaction does (Codex review of #95, round 2): a retry with the same
// requestId gets the first reply, the question as it settled then; a
// second answer is conflict, with pocketd's words; a question past its
// deadline (on the server's clock, Now) is skipped for its time, with its
// session's event, and the answer is conflict, its requestId left free.
// The body is checked as pocketd checks it (checkAnswer). Settling it
// takes it off its session, which runs on, with the session's event.
func (s *Server) answerQuestion(w http.ResponseWriter, r *http.Request) {
	var request proto.AnswerQuestion
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId is required")
		return
	}
	id, qid := pathID(r), r.PathValue("qid")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayOpLocked(w, r.Method+" "+r.URL.Path, request.RequestID) {
		return
	}
	if s.afterReplayLookup != nil {
		s.afterReplayLookup()
	}
	q, ok := s.questionLocked(qid)
	if !ok || q.SessionID != id {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such question")
		return
	}
	if q.State == proto.QuestionWaiting && !q.Deadline.IsZero() && !Now.Before(q.Deadline) {
		settled := Now
		q.State, q.Reason, q.SettledBy, q.SettledAt = proto.QuestionSkipped, proto.SkippedTimeout, "pocketd", &settled
		s.settleLocked(q)
	}
	if q.State != "" && q.State != proto.QuestionWaiting {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, settledText(q))
		return
	}
	answer, option, err := checkAnswer(q.Options, q.Multiple, request)
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	settled := Now
	q.SettledAt, q.SettledBy = &settled, "laptop"
	if request.Skipped {
		q.State, q.Reason = proto.QuestionSkipped, proto.SkippedByOwner
	} else {
		q.State, q.Answer, q.Option = proto.QuestionAnswered, answer, option
	}
	s.settleLocked(q)
	s.rememberLocked(r, request.RequestID, q)
	writeJSON(w, http.StatusOK, q)
}

// settleLocked keeps q settled and takes it off its session, which runs
// on, with the session's event. s.mu is held.
func (s *Server) settleLocked(q proto.Question) {
	s.questions[q.ID] = q
	if sess := s.sessions[q.SessionID]; sess != nil {
		if cur := sess.detail.Session.Question; cur != nil && cur.ID == q.ID {
			summary := sess.detail.Session
			summary.Question, summary.State = nil, proto.SessionRunning
			s.emitLocked(proto.Event{Type: proto.EventSession, Session: &summary})
		}
	}
}

// settledText is pocketd's word for an answer to q, which settled
// already: by whom, and how.
func settledText(q proto.Question) string {
	switch {
	case q.State == proto.QuestionAnswered:
		return "the question was answered already, on " + q.SettledBy + ": the first answer stands"
	case q.Reason == proto.SkippedByOwner:
		return "the question was skipped already, on " + q.SettledBy
	case q.Reason == proto.SkippedTimeout:
		return "the question was skipped already: no answer came in time"
	case q.Reason == proto.SkippedAborted:
		return "the question was skipped already: its run was aborted"
	}
	return "the question is no longer waiting"
}

// checkAnswer checks an answer's body as pocketd does (team.CheckAnswer):
// a skip with no answer at all, white space included, and no option; else
// an answer of at most proto.MaxAnswer characters once trimmed, and
// options of the question's, in any order, none twice, more than one only
// when it takes several, which go sorted. Options alone answer with their
// texts, joined by "; ".
func checkAnswer(options []string, multiple bool, a proto.AnswerQuestion) (string, []int, error) {
	answer := strings.TrimSpace(a.Answer)
	if a.Skipped {
		if a.Answer != "" || len(a.Option) > 0 {
			return "", nil, errors.New("a skip carries no answer and no option")
		}
		return "", nil, nil
	}
	if n := utf8.RuneCountInString(answer); n > proto.MaxAnswer {
		return "", nil, fmt.Errorf("the answer has %d characters, more than %d", n, proto.MaxAnswer)
	}
	if len(a.Option) > 0 && len(options) == 0 {
		return "", nil, errors.New("the question suggests no option")
	}
	if len(a.Option) > 1 && !multiple {
		return "", nil, errors.New("the question takes one option")
	}
	var option []int
	for _, i := range a.Option {
		if i < 0 || i >= len(options) {
			return "", nil, fmt.Errorf("option %d is not one of the question's %d", i, len(options))
		}
		if slices.Contains(option, i) {
			return "", nil, fmt.Errorf("option %d is picked twice", i)
		}
		option = append(option, i)
	}
	slices.Sort(option)
	if answer == "" {
		if len(option) == 0 {
			return "", nil, fmt.Errorf("an answer is 1 to %d characters, or the options picked", proto.MaxAnswer)
		}
		picked := make([]string, len(option))
		for j, i := range option {
			picked[j] = options[i]
		}
		answer = strings.Join(picked, "; ")
	}
	return answer, option, nil
}
