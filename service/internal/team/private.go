package team

import (
	"fmt"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A private agent (`private: true`) works for the owner alone
// (ADR-0042): its sessions' instructions start with PrivateIntro in place
// of the team's preamble and carry no roster and no lessons section, its
// sessions get no team tool, and a team tool an older session kept
// refuses it (CheckTeamCaller).

// PrivateIntro begins a private agent's identity in its sessions'
// instructions, before its profile: it is the owner's agent, not the
// team's, and has no supervisor, team or desk.
func PrivateIntro(name string) string {
	return "You are " + name + ", the owner's personal agent: you work for the owner directly, in sessions the owner starts. " +
		"You have no supervisor, no team and no desk. Your profile follows."
}

// CheckTeamCaller refuses every team tool's call from a session of a
// private profile, p as it is now (nil for a session from no profile):
// one made before its profile became private, or before ADR-0042, may
// still list a team tool.
func CheckTeamCaller(p *profiles.Profile) error {
	if p != nil && p.Private {
		return fmt.Errorf("%s is private, and team tools are not a private agent's: you work for the owner directly, with no supervisor, team or desk, and pocketd keeps no lessons for you", p.ID)
	}
	return nil
}

// CheckLessonsFor refuses to keep or list lessons for agent p: pocketd
// keeps none for a private agent, whose sessions never show them.
func CheckLessonsFor(p profiles.Profile) error {
	if p.Private {
		return fmt.Errorf("%s is private, and pocketd keeps no lessons for a private agent", p.ID)
	}
	return nil
}
