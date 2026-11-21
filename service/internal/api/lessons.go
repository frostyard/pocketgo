package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamMemory runs team_memory for session call.Session (ADR-0015): it
// keeps the lessons of the session's own agent, or a supervisor's add or
// list for another agent. Every change waits for the owner. A lesson
// change and its event go out together under workMu.
func (s *Server) TeamMemory(ctx context.Context, call TeamCall, m team.Memory) (string, error) {
	// A cross-review keeps no lessons, whatever tool list it has (ADR-0019;
	// Codex review of #26, round 1).
	if thread, err := s.store.ThreadOfSession(ctx, call.Session); err == nil && thread.ReviewPull != 0 {
		return "", errors.New("a cross-review keeps no lessons: it files its verdict only")
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	ctx = context.WithoutCancel(ctx)
	if err := team.CheckMemory(m); err != nil {
		return "", err
	}
	// A call that saved or changed a lesson already answers as it did,
	// whatever the profiles say now: its work is done.
	if m.Action != team.MemoryList {
		row, outcome, found, err := s.store.LessonCall(ctx, call.key())
		if err != nil {
			return "", err
		}
		if found {
			if m.Action == team.MemoryAdd {
				return team.SavedText(row.ID, s.agentName(row.Agent)), nil
			}
			return changeText(row.ID, outcome), nil
		}
	}
	// A consult's session files its finding and nothing else (ADR-0020):
	// the role is checked here too, not only by the tools it is offered.
	if _, err := s.store.ConsultOfSession(ctx, call.Session); err == nil {
		return "", errors.New("team_memory is not a consult's: a consult files its finding with team_finding, and nothing else")
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	// A gather's proposal session files proposals and nothing else
	// (ADR-0033), checked here too (Codex review of #57, round 3).
	if _, err := s.store.GatherAskOfSession(ctx, call.Session); err == nil {
		return "", errors.New("team_memory is not a proposal session's: it files proposals with team_propose and reads GitHub with team_github, and nothing else")
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if caller == nil {
		return "", errors.New("team_memory works only in a session started from a profile; a lesson about one repository goes in that repository, by a pull request")
	}
	agent := *caller
	if m.Agent != "" && m.Agent != caller.ID {
		if caller.Role != profiles.RoleSupervisor {
			return "", errors.New("only the supervisor keeps lessons for another agent")
		}
		other, err := s.profiles.Find(m.Agent)
		if err != nil {
			return "", fmt.Errorf("no agent %s on the team: %w", m.Agent, err)
		}
		agent = other
	}
	switch m.Action {
	case team.MemoryList:
		rows, err := s.store.LessonsOf(ctx, agent.ID)
		if err != nil {
			return "", err
		}
		return team.ListText(agent.Name, teamLessons(rows)), nil
	case team.MemoryAdd:
		text, err := team.LessonText(m.Text)
		if err != nil {
			return "", err
		}
		s.workMu.Lock()
		row, replayed, err := s.store.AddLesson(ctx, store.LessonRow{Key: call.key(), Agent: agent.ID, Text: text, Writer: caller.ID, SessionID: call.Session}, team.CheckPending)
		if err == nil && !replayed {
			s.publishLesson(row)
		}
		s.workMu.Unlock()
		if err != nil {
			return "", err
		}
		return team.SavedText(row.ID, agent.Name), nil
	}
	id, err := team.LessonID(m.ID)
	if err != nil {
		return "", err
	}
	text := ""
	if m.Action == team.MemoryUpdate {
		if text, err = team.LessonText(m.Text); err != nil {
			return "", err
		}
	}
	s.workMu.Lock()
	row, outcome, replayed, err := s.store.ChangeLesson(ctx, call.key(), id, agent.ID, caller.ID, call.Session, text, team.CheckPending)
	if err == nil && !replayed && outcome != store.LessonWasGone {
		s.publishLesson(row)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there is no lesson L%d for %s", id, agent.Name)
	} else if err != nil {
		return "", err
	}
	return changeText(row.ID, outcome), nil
}

// changeText is team_memory's answer to an update or remove of lesson id
// that did outcome.
func changeText(id int64, outcome string) string {
	switch outcome {
	case store.LessonChanged:
		return team.ChangedText(id)
	case store.LessonProposed:
		return team.ProposedText(id)
	case store.LessonKept:
		return team.KeptText(id)
	case store.LessonDropped:
		return team.DiscardedText(id)
	case store.LessonForgetProposed:
		return team.ForgetText(id)
	}
	return team.GoneText(id)
}

// teamLessons are rows as team's texts show them.
func teamLessons(rows []store.LessonRow) []team.Lesson {
	out := make([]team.Lesson, len(rows))
	for i, row := range rows {
		out[i] = team.Lesson{ID: row.ID, Text: row.Text, Pending: row.State == store.LessonPending, ChangeText: row.ChangeText, Forget: row.ChangeForget}
	}
	return out
}

// lessonsSection is the section of agent's sessions' instructions: its
// lessons in force now (team.LessonsSection), which depends on nothing
// else, so a profile changed since a session started never changes it.
func (s *Server) lessonsSection(ctx context.Context, agent string) (string, error) {
	rows, err := s.store.ActiveLessons(ctx, agent)
	if err != nil {
		return "", err
	}
	return sectionOf(rows), nil
}

// sectionOf is the lessons section with rows, the lessons in force.
func sectionOf(rows []store.LessonRow) string {
	return team.LessonsSection(teamLessons(rows))
}

// lessonsChanged notes that agent's sessions may not have its lessons in
// force, and has a delivery pass bring them (refreshLessons).
func (s *Server) lessonsChanged(agent string) {
	s.lessonsMu.Lock()
	s.stale[agent] = true
	s.lessonsMu.Unlock()
	s.deliverLater()
}

// lessonsSince brings a session of agent, recorded now, the lessons a
// change made after its instructions read them (had): the change's own
// refresh may have looked before the session was recorded.
func (s *Server) lessonsSince(ctx context.Context, agent, had string) {
	if now, err := s.lessonsSection(ctx, agent); err != nil || now != had {
		s.lessonsChanged(agent)
	}
}

// refreshLessons gives every session of each agent noted stale its
// lessons in force (Engine.SetLessons), and every agent's at start. Only
// a delivery pass runs it, so two refreshes never race; an agent that
// failed is noted again, and done is false.
func (s *Server) refreshLessons(ctx context.Context) (done bool, err error) {
	// One refresh at a time, also when DeliverPending runs a pass beside a
	// scheduled one: a flush waits for the refresh under way, and an older
	// section is never written after a newer one.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.lessonsMu.Lock()
	all := s.allStale
	agents := make([]string, 0, len(s.stale))
	for agent := range s.stale {
		agents = append(agents, agent)
	}
	s.allStale, s.stale = false, map[string]bool{}
	s.lessonsMu.Unlock()
	if all {
		every, err := s.store.SessionProfiles(ctx)
		if err != nil {
			s.lessonsMu.Lock()
			s.allStale = true
			s.lessonsMu.Unlock()
			return false, err
		}
		agents = append(agents, every...)
	}
	var failures []error
	for _, agent := range agents {
		if err := s.refreshAgent(ctx, agent); err != nil {
			failures = append(failures, fmt.Errorf("lessons of %s: %w", agent, err))
			s.lessonsMu.Lock()
			s.stale[agent] = true
			s.lessonsMu.Unlock()
		}
	}
	return len(failures) == 0, errors.Join(failures...)
}

// refreshAgent gives each session of agent its lessons in force.
func (s *Server) refreshAgent(ctx context.Context, agent string) error {
	section, err := s.lessonsSection(ctx, agent)
	if err != nil {
		return err
	}
	ids, err := s.store.SessionsOfProfile(ctx, agent)
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err := s.engine.SetLessons(ctx, id, section); err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

// RefreshAllLessons has the first delivery pass give every session started
// from a profile its agent's lessons in force: pocketd may have stopped
// before it gave them, or a lesson changed in a work database restored
// since. The service calls it once at start, before StartDelivery.
func (s *Server) RefreshAllLessons() {
	s.lessonsMu.Lock()
	s.allStale = true
	s.lessonsMu.Unlock()
}

func (s *Server) publishLesson(row store.LessonRow) {
	lesson := wireLesson(row)
	s.emit(proto.Event{Type: proto.EventLesson, Lesson: &lesson})
}

func wireLesson(row store.LessonRow) proto.Lesson {
	out := proto.Lesson{ID: row.ID, Agent: row.Agent, Text: row.Text, State: proto.LessonState(row.State), Writer: row.Writer,
		SessionID: row.SessionID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.ChangeText != "" || row.ChangeForget {
		out.Change = &proto.LessonChange{Text: row.ChangeText, Forget: row.ChangeForget, Writer: row.ChangeWriter, SessionID: row.ChangeSession}
	}
	return out
}

func (s *Server) lessons(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.LiveLessons(r.Context())
	if err != nil {
		s.internal(w, "list lessons", err)
		return
	}
	out := make([]proto.Lesson, 0, len(rows))
	for _, row := range rows {
		out = append(out, wireLesson(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// decideLesson runs the owner's decision on a lesson (ADR-0015). An
// approval names what the owner saw waiting, and holds only for it. A
// change to the lessons in force reaches the agent's sessions by a
// delivery pass.
func (s *Server) decideLesson(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIDOf(w, r, "lesson")
	if !ok {
		return
	}
	var request proto.DecideLesson
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	switch {
	case request.Decision == proto.LessonApprove && (request.Text == "") == !request.Forget:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "an approval names what it approves: the text shown, or forget")
		return
	case request.Decision == proto.LessonApprove:
	case request.Decision == proto.LessonDiscard, request.Decision == proto.LessonForget:
		if request.Text != "" || request.Forget {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "only an approval names text or forget")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "decision must be approve, discard or forget")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	s.workMu.Lock()
	// The text as sent: an approval names exactly what the owner saw.
	row, replayed, inForce, err := s.store.DecideLesson(ctx, key, id, string(request.Decision), request.Text, request.Forget, team.CheckRoom)
	if err == nil && !replayed {
		s.publishLesson(row)
	}
	s.workMu.Unlock()
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no lesson %d", id))
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error())
		return
	case err != nil:
		s.internal(w, "decide lesson", err)
		return
	}
	if inForce {
		s.lessonsChanged(row.Agent)
	}
	writeJSON(w, http.StatusOK, wireLesson(row))
}
