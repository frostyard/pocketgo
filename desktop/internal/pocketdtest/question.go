package pocketdtest

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// PutQuestion puts q, waiting, on its session, without an event: the
// session's Question, and its state waiting while it waits (ADR-0047). A
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
// service does (protocol rule 62): a skip alone, or an answer of at most
// proto.MaxAnswer characters once trimmed and the options picked, each
// once, in order, one unless the question takes several. An answer of
// options alone reads as the options' texts. It is idempotent on its
// requestId; a second answer is conflict. Settling it takes it off its
// session, which runs on, with the session's event.
func (s *Server) answerQuestion(w http.ResponseWriter, r *http.Request) {
	var request proto.AnswerQuestion
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId is required")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	id, qid := pathID(r), r.PathValue("qid")
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.questionLocked(qid)
	if !ok || q.SessionID != id {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such question")
		return
	}
	if q.State != "" && q.State != proto.QuestionWaiting {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the question is already "+string(q.State)+" (on "+q.SettledBy+")")
		return
	}
	answer := strings.TrimSpace(request.Answer)
	switch {
	case request.Skipped && (answer != "" || len(request.Option) > 0):
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "a skip carries no answer")
		return
	case !request.Skipped && answer == "" && len(request.Option) == 0:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "answer or option is required")
		return
	case utf8.RuneCountInString(answer) > proto.MaxAnswer:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the answer is too long")
		return
	case len(request.Option) > 1 && !q.Multiple:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the question takes one option")
		return
	}
	var picked []string
	for i, o := range request.Option {
		if o < 0 || o >= len(q.Options) || (i > 0 && o <= request.Option[i-1]) {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad option")
			return
		}
		picked = append(picked, q.Options[o])
	}
	settled := Now
	q.SettledAt, q.SettledBy = &settled, "laptop"
	if request.Skipped {
		q.State, q.Reason = proto.QuestionSkipped, proto.SkippedByOwner
	} else {
		if answer == "" {
			answer = strings.Join(picked, "; ")
		}
		q.State, q.Answer, q.Option = proto.QuestionAnswered, answer, request.Option
	}
	s.questions[q.ID] = q
	if sess := s.sessions[id]; sess != nil {
		if cur := sess.detail.Session.Question; cur != nil && cur.ID == q.ID {
			summary := sess.detail.Session
			summary.Question, summary.State = nil, proto.SessionRunning
			s.emitLocked(proto.Event{Type: proto.EventSession, Session: &summary})
		}
	}
	s.rememberLocked(r, request.RequestID, q)
	writeJSON(w, http.StatusOK, q)
}
