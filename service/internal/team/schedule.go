package team

import (
	"errors"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A private agent's own schedules (ADR-0060): a private agent's session
// keeps its own reminders and repeats with the schedule tool, the
// engine and limits of the supervisor's team_schedule (ADR-0030) under a
// name of its own. It is no team tool, and the one exception ADR-0042
// names to a private agent's having nothing of pocketd's team machinery:
// it sends messages to the calling session alone, and reaches no other
// session, agent or Inbox. A supervisor keeps team_schedule, and gets no
// schedule.

// MaySchedule says whether a session of profile p (nil for none), working
// as role, gets the schedule tool: a private agent's session working as
// none of the roles, a session the owner talks in. A supervisor's session
// keeps its own with team_schedule instead (roleTools), and any other
// session keeps none.
func MaySchedule(p *profiles.Profile, role SessionRole) bool {
	return p != nil && p.Private && role.none()
}

// CheckScheduleCaller refuses a schedule call from a session that may not
// keep its own schedules, p as its profile is now: only a private agent's
// or a supervisor's session, working as none of the roles, may. A session
// made while its profile was private keeps its schedules with it once its
// profile is a supervisor's, which keeps its own anyway; one whose profile
// is a team agent's now, or is gone, and one that works as a thread's
// specialist, a review, a worker, a consult or a gather's agent, is
// refused, whatever tools it lists from before.
func CheckScheduleCaller(p *profiles.Profile, role SessionRole) error {
	if p != nil && (p.Private || p.Role == profiles.RoleSupervisor) && role.none() {
		return nil
	}
	return errors.New("schedule is a private agent's and the supervisor's alone, in a session the owner talks in: this session keeps no schedules")
}
