package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// The question tool (ADR-0048): a supervisor's or a private agent's
// session puts a question to the owner inline, and the call waits for the
// answer, in the same run. The rules are team's; the wait is
// approvals.Questions', built as an approval's is.

// ownerKeyPrefix begins the request key of every request a device makes
// (requestKey): a submission of such a key is the owner's own message
// (POST /v1/sessions/{id}/messages). pocketd's own messages, a schedule's,
// a note's, a report's or a supervisor's, never carry one.
const ownerKeyPrefix = "u:"

// ownerMessage says whether a submission of request key is the owner's
// own message.
func ownerMessage(key string) bool { return strings.HasPrefix(key, ownerKeyPrefix) }

// Question puts q to the owner in the calling session and waits for the
// answer, which is the tool's result (team.QuestionResult). A call run
// again finds its question by its task and call (store.QuestionID): it
// waits again for one still waiting, and answers as it did for one
// settled, whatever the rules say by then. A new one is refused, before
// anything is kept, for a session that does not get the tool
// (team.CheckQuestionCaller), in a run the owner's own message did not
// start (team.CheckWatched), and while another question of the session
// waits. It waits until the profile's timeout (team.QuestionTimeout).
// ctx ending ends the wait with ctx's error, and the question stays as it
// is: an abort skips it once its task ended (Questions.RetireEnded), and
// a stop of the service leaves it for the call run again.
func (s *Server) Question(ctx context.Context, call TeamCall, q team.Question) (string, error) {
	id := store.QuestionID(call.Task, call.CallID)
	row, err := s.store.Question(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if row, err = s.newQuestion(ctx, call, q); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case row.SessionID != call.Session:
		return "", fmt.Errorf("question %s is another session's", id)
	case row.State == store.QuestionWaiting:
		// A call that waits again after a restart announces its question
		// again, as an approval's does.
		s.publishQuestion(row.SessionID)
	}
	row, err = s.questions.Await(ctx, row.ID)
	if err != nil {
		return "", err
	}
	return team.QuestionResult(approvals.WireQuestion(row)), nil
}

// newQuestion checks a new question call and keeps its question, waiting.
func (s *Server) newQuestion(ctx context.Context, call TeamCall, q team.Question) (store.QuestionRow, error) {
	checked, err := team.CheckQuestion(q)
	if err != nil {
		return store.QuestionRow{}, err
	}
	caller, role, err := s.sessionCaller(ctx, call.Session)
	if err != nil {
		return store.QuestionRow{}, err
	}
	if err := team.CheckQuestionCaller(caller, role); err != nil {
		return store.QuestionRow{}, err
	}
	key, busy, err := s.engine.RunStarter(ctx, call.Session)
	if err != nil {
		return store.QuestionRow{}, fmt.Errorf("read what started this run: %w", err)
	}
	if err := team.CheckWatched(busy && ownerMessage(key)); err != nil {
		return store.QuestionRow{}, err
	}
	row, _, err := s.questions.Put(ctx, store.QuestionRow{
		SessionID: call.Session, TaskID: call.Task, CallID: call.CallID, Question: checked.Question, Options: checked.Options,
		Multiple: checked.Multiple, Placeholder: checked.Placeholder, Deadline: s.now().Add(team.QuestionTimeout(*caller)),
	})
	if errors.Is(err, store.ErrQuestionWaits) {
		return store.QuestionRow{}, team.ErrQuestionWaits
	}
	return row, err
}

// sessionCaller is session's profile as it is now, nil for none or for a
// profile that is gone or invalid, and what the session works as. A
// profile pocketd cannot read now is an error, and a later call tries
// again.
func (s *Server) sessionCaller(ctx context.Context, session int64) (*profiles.Profile, team.SessionRole, error) {
	row, err := s.store.Session(ctx, session)
	if err != nil {
		return nil, team.SessionRole{}, err
	}
	role, err := s.sessionRole(ctx, session)
	if err != nil {
		return nil, role, err
	}
	if row.Profile == "" || s.profiles == nil {
		return nil, role, nil
	}
	p, err := s.profiles.Find(row.Profile)
	if profiles.Unreadable(err) {
		return nil, role, fmt.Errorf("this session's profile %s: %w", row.Profile, err)
	} else if err != nil {
		return nil, role, nil
	}
	return &p, role, nil
}

// sessionRole is what session works as: a thread's specialist (a review
// included), a thread's worker, a consult or a gather's proposal session.
func (s *Server) sessionRole(ctx context.Context, session int64) (team.SessionRole, error) {
	var role team.SessionRole
	thread, err := s.store.ThreadOfSession(ctx, session)
	switch {
	case err == nil:
		role.Thread, role.Review = true, thread.ReviewPull != 0
	case !errors.Is(err, store.ErrNotFound):
		return role, err
	}
	if _, err := s.store.ThreadOfWorker(ctx, session); err == nil {
		role.Worker = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return role, err
	}
	if _, err := s.store.ConsultOfSession(ctx, session); err == nil {
		role.Consult = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return role, err
	}
	if _, err := s.store.GatherAskOfSession(ctx, session); err == nil {
		role.Gather = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return role, err
	}
	return role, nil
}

// publishQuestion sends session id's summary after its question changed:
// asked, answered or skipped. Its state may be the same, so it goes out
// always.
func (s *Server) publishQuestion(id int64) { s.publishSession(id, true) }

// RetireQuestions skips, with the reason aborted, each waiting question
// whose task ended: the service runs it at start, before tasks resume, as
// it denies such approvals.
func (s *Server) RetireQuestions(ctx context.Context) error {
	return s.questions.RetireEnded(ctx, 0, s.engine.TaskEnded)
}

// questionsRunEnded skips session id's waiting question once its task
// ended with the run: an abort that a restart cut off, which PiG finishes
// as the tasks resume, leaves no handler and no wait to skip it, and its
// task looked alive at the start's retire. One that fails is tried again
// after retryDelay, the task checked again, until it holds or the server
// stops: no other path may be left to skip it (Codex review of #95,
// round 2).
func (s *Server) questionsRunEnded(id int64) {
	err := s.questions.RetireEnded(s.passCtx, id, s.engine.TaskEnded)
	if err == nil {
		return
	}
	s.log.Warn("skip an ended run's question", "session", id, "err", err)
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	s.retry(func() {
		// A pass of its own, which Stop waits for.
		s.sessionMu.Lock()
		if s.stopped {
			s.sessionMu.Unlock()
			return
		}
		s.passes.Add(1)
		s.sessionMu.Unlock()
		defer s.passes.Done()
		s.questionsRunEnded(id)
	})
}

// answerQuestion is POST /v1/sessions/{id}/question/{qid} (rule 62): the
// owner's answer to a waiting question of the session, or the owner's
// skip. Any device answers, and the first answer stands: the question is
// settled once, and another answer is conflict. A retry with the same
// requestId answers with the question as it is.
func (s *Server) answerQuestion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.AnswerQuestion
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) || !s.knownSession(w, r, id) {
		return
	}
	qid := r.PathValue("qid")
	ctx := context.WithoutCancel(r.Context())
	question, err := s.store.Question(ctx, qid)
	if errors.Is(err, store.ErrNotFound) || err == nil && question.SessionID != id {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("session %d has no question %s", id, qid))
		return
	} else if err != nil {
		s.internal(w, "read the question", err)
		return
	}
	answer, option, err := team.CheckAnswer(question.Options, question.Multiple, request)
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	row, _, err := s.questions.Answer(ctx, key, id, qid, answer, option, request.Skipped, deviceOf(r).Name)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("session %d has no question %s", id, qid))
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, settledText(ctx, s.store, qid))
	case err != nil:
		s.internal(w, "answer the question", err)
	default:
		writeJSON(w, http.StatusOK, approvals.WireQuestion(row))
	}
}

// settledText says why an answer to question id was refused: it was
// answered or skipped already, and by whom.
func settledText(ctx context.Context, s *store.Store, id string) string {
	row, err := s.Question(ctx, id)
	if err != nil {
		return "the question is no longer waiting"
	}
	switch {
	case row.State == store.QuestionWaiting:
		// Refused while it waits: its deadline passed, and the skip that
		// follows is to come.
		return "the question was skipped already: no answer came in time"
	case row.State == store.QuestionAnswered:
		return fmt.Sprintf("the question was answered already, on %s: the first answer stands", row.SettledBy)
	case row.Reason == proto.SkippedByOwner:
		return fmt.Sprintf("the question was skipped already, on %s", row.SettledBy)
	case row.Reason == proto.SkippedTimeout:
		return "the question was skipped already: no answer came in time"
	case row.Reason == proto.SkippedAborted:
		return "the question was skipped already: its run was aborted"
	}
	return "the question is no longer waiting"
}
