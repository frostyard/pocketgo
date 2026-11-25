package team

import (
	"fmt"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The owner renames a private agent's session, and no other (ADR-0058,
// protocol rule 68): the owner's decision of 2026-11-21. CheckRename is
// the rule, read by POST /v1/sessions/{id}/title and by every copy of a
// session pocketd sends, as its renamable.

// RenameFacts are what CheckRename decides a session's rename from: its
// record, and its profile as loaded now.
type RenameFacts struct {
	// Session is the session's id, and Profile the profile it started
	// from, "" for none: the owner's own session.
	Session int64
	Profile string
	// Team says pocketd started the session for the team's work: a
	// thread's specialist, its worker, a review, a consult, a gather's
	// ask or an agent's work session (its created_by is team or gather;
	// ADR-0073).
	Team bool
	// NoProfiles says pocketd runs without profiles. Now is the profile as
	// loaded now, nil when it is gone or invalid.
	NoProfiles bool
	Now        *profiles.Profile
}

// CheckRename refuses to rename a session that is not a private agent's
// now: the owner's own (no profile), one pocketd started for the team's
// work whatever its profile says now, one whose profile is gone or
// invalid, or not private, or a private supervisor's, since the owner
// named supervisor sessions among those that keep their title.
func CheckRename(f RenameFacts) error {
	const only = "only a private agent's session may be renamed"
	switch {
	case f.Profile == "":
		return fmt.Errorf("session %d is your own, started from no profile: %s", f.Session, only)
	case f.Team:
		return fmt.Errorf("session %d is the team's: pocketd started it for a thread, a worker, a review, a consult, a gather or an agent's team_work, and %s", f.Session, only)
	case f.NoProfiles:
		return fmt.Errorf("pocketd runs without profiles, so session %d's profile %s is not private now: %s", f.Session, f.Profile, only)
	case f.Now == nil:
		return fmt.Errorf("session %d's profile %s is gone or invalid now: %s", f.Session, f.Profile, only)
	case !f.Now.Private:
		return fmt.Errorf("session %d is %s's, which is on the team: %s", f.Session, f.Profile, only)
	case f.Now.Role == profiles.RoleSupervisor:
		return fmt.Errorf("session %d is supervisor %s's: a supervisor's session keeps its title, and %s", f.Session, f.Profile, only)
	}
	return nil
}
