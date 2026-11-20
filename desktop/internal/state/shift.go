package state

import (
	"context"
	"errors"
	"fmt"

	"github.com/frostyard/pocketgo/proto"
)

// ShiftLine says where a thread's shift is, in one line (ADR-0024).
func ShiftLine(sh proto.Shift) string {
	of := fmt.Sprintf("round %d of %d", sh.Round, sh.MaxRounds)
	switch sh.State {
	case proto.ShiftRunning:
		line := "Shift running: " + of
		if sh.Deadline != nil {
			line += ", until " + sh.Deadline.UTC().Format("Mon 15:04 UTC")
		}
		return line + "."
	case proto.ShiftSummary:
		return "Shift stopped at " + of + ": " + sh.Why + ". Its report is asked for."
	case proto.ShiftPaused:
		return "Shift paused at " + of + ": " + sh.Why + "."
	}
	return "Shift ended at " + of + ": " + sh.Why + "."
}

// shiftChangeable says whether pocketd takes the owner's change of t's
// shift at all (protocol rule 55): t runs one, is not closed, and its
// specialist has a session, so it is past starting. pocketd also waits
// for the brief, which a thread does not carry: it refuses that itself,
// and says so (Codex review of #37).
func shiftChangeable(t proto.Thread) bool {
	return t.Shift != nil && t.State != proto.ThreadClosed && t.State != proto.ThreadStarting && t.Session != 0
}

// CanStopShift says whether the owner may stop t's shift: one not ended.
func CanStopShift(t proto.Thread) bool {
	return shiftChangeable(t) && t.Shift.State != proto.ShiftEnded
}

// CanResumeShift says whether the owner may go on with t's shift, by
// pocketd's rule (protocol rule 55): a paused one, or one that ended for a
// reason other than done, its deadline or its thread's close, with rounds
// left. A shift that ended at its deadline has no time left, whatever its
// rounds (Codex review of #37); a deadline that passed since is pocketd's
// to check, and it refuses the resume, saying so.
func CanResumeShift(t proto.Thread) bool {
	if !shiftChangeable(t) {
		return false
	}
	sh := t.Shift
	resumable := sh.State == proto.ShiftPaused ||
		sh.State == proto.ShiftEnded && sh.Reason != "done" && sh.Reason != "closed" && sh.Reason != "deadline"
	return resumable && sh.Round < sh.MaxRounds
}

// ShiftWaitsForOwner says whether t's shift waits for the owner: the
// owner's message paused it, and only the owner resumes or stops it.
func ShiftWaitsForOwner(t proto.Thread) bool {
	return t.Shift != nil && t.Shift.State == proto.ShiftPaused
}

// shiftTarget is what a stop or resume acts on: the shift as the owner
// saw it. A committed stop or resume always moves it (a stop ends the
// shift, a resume moves its round on), as does a pause, so a request id
// kept for one change of the shift never goes with a later one (Codex
// review of #37).
func shiftTarget(action string, sh proto.Shift) string { return action + "\x00" + shiftSeen(sh) }

// shiftSeen is a shift as the owner saw it: its state, reason and round.
func shiftSeen(sh proto.Shift) string {
	return fmt.Sprintf("%s\x00%s\x00%d", sh.State, sh.Reason, sh.Round)
}

// settleShifts drops the retry state of each stop or resume whose shift
// moved since it was asked for, as an event or a fetch shows it: its
// failure no longer says anything about the shift, as when pocketd stopped
// it and only the reply was lost, and the next stop or resume is a new
// request (Codex review of #37, round 2).
func (s *Store) settleShifts() {
	for id, p := range s.shifts {
		if p.Busy {
			continue
		}
		if th, ok := s.Data.Thread(id); ok && th.Shift != nil && shiftSeen(*th.Shift) == s.shiftAsked[id] {
			continue
		}
		delete(s.shifts, id)
		delete(s.shiftAsked, id)
		s.ids.Done(shiftKey(id))
	}
}

func shiftKey(id int64) string { return fmt.Sprintf("shift:%d", id) }

// StopShift stops thread id's shift for the owner (POST
// /v1/threads/{id}/shift/stop), and ResumeShift goes on with it (POST
// .../resume). Each tried again after a failure, on the shift as it was,
// keeps its request id, so a lost reply is not applied twice; one on a
// shift that moved since, or the other action, is a new request: one key
// a thread, the action and the shift it acts on its target (shiftTarget).
func (s *Store) StopShift(id int64) { s.changeShift(id, "stop") }

func (s *Store) ResumeShift(id int64) { s.changeShift(id, "resume") }

// ShiftChanging is where the owner's change of thread id's shift stands.
func (s *Store) ShiftChanging(id int64) Pending { return valueOf(s.shifts[id]) }

func (s *Store) changeShift(id int64, action string) {
	th, ok := s.Data.Thread(id)
	if !ok || th.Shift == nil {
		return
	}
	p := pending(s.shifts, id)
	if p.Busy {
		return
	}
	key := shiftKey(id)
	requestID := s.ids.For(key, shiftTarget(action, *th.Shift))
	s.shiftAsked[id] = shiftSeen(*th.Shift)
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "%s the shift of T%d", map[string]string{"stop": "Stop", "resume": "Resume"}[action], id)
	f := s.Data.BeginThreadChange()
	var changed proto.Thread
	s.do(fmt.Sprintf("Changing the shift of T%d", id), func(ctx context.Context, c *proto.Client) (err error) {
		if action == "stop" {
			changed, err = c.StopShift(ctx, id, proto.StopShift{RequestID: requestID})
		} else {
			changed, err = c.ResumeShift(ctx, id, proto.ResumeShift{RequestID: requestID})
		}
		return err
	}, func(err error) {
		p.Busy = false
		if err != nil {
			s.Data.ThreadChangeFailed(f)
		}
		var pe *proto.Error
		switch {
		case err == nil:
			s.ids.Done(key)
			delete(s.shifts, id)
			delete(s.shiftAsked, id)
			if s.Data.ThreadChanged(f, changed) {
				s.loadThreads()
			}
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeNotFound || pe.Code == proto.ErrorCodeConflict):
			// pocketd's word is final: the same request again would get
			// it again, and the thread as it is now says why.
			s.ids.Done(key)
			p.Err = pe.Message
			s.loadThreads()
		default:
			p.Err = err.Error()
		}
		// The thread's event may have come before this reply: a shift that
		// moved keeps no retry state.
		s.settleShifts()
	})
}
