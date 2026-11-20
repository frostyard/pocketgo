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

// CanStopShift says whether the owner may stop a shift: one not ended.
func CanStopShift(sh proto.Shift) bool { return sh.State != proto.ShiftEnded }

// CanResumeShift says whether the owner may go on with a shift, by
// pocketd's rule (protocol rule 55): a paused one, or one that ended for a
// reason other than done or its thread's close, with rounds left. Its
// deadline is pocketd's to check: a shift past it is refused there, and
// says so.
func CanResumeShift(sh proto.Shift) bool {
	resumable := sh.State == proto.ShiftPaused || sh.State == proto.ShiftEnded && sh.Reason != "done" && sh.Reason != "closed"
	return resumable && sh.Round < sh.MaxRounds
}

func shiftKey(action string, id int64) string { return fmt.Sprintf("shift-%s:%d", action, id) }

// StopShift stops thread id's shift for the owner (POST
// /v1/threads/{id}/shift/stop), and ResumeShift goes on with it (POST
// .../resume). Each tried again after a failure keeps its request id, so
// a lost reply is not applied twice.
func (s *Store) StopShift(id int64) { s.changeShift(id, "stop") }

func (s *Store) ResumeShift(id int64) { s.changeShift(id, "resume") }

// ShiftChanging is where the owner's change of thread id's shift stands.
func (s *Store) ShiftChanging(id int64) Pending { return valueOf(s.shifts[id]) }

func (s *Store) changeShift(id int64, action string) {
	p := pending(s.shifts, id)
	if p.Busy {
		return
	}
	key := shiftKey(action, id)
	requestID := s.ids.For(key, action)
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
	})
}
