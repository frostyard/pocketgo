package team

import (
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Only a private agent's session may be renamed (ADR-0058, the owner's
// decision of 2026-11-21): its profile as loaded now, private and no
// supervisor's, and a session the owner started, its standing one
// included. The owner's own sessions, the team's, pocketd's sessions for
// the team's work and a private supervisor's keep their titles, each with
// its reason.
func TestOnlyAPrivateAgentsSessionMayBeRenamed(t *testing.T) {
	chani := profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Home: "/home/chani", Private: true}
	standing := chani
	standing.Standing = true
	vera := profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent}
	odrade := profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor}
	alia := profiles.Profile{ID: "alia", Name: "Alia", Role: profiles.RoleSupervisor, Home: "/home/alia", Private: true}
	for _, ok := range []RenameFacts{
		{Session: 1, Profile: "chani", Now: &chani},
		{Session: 2, Profile: "chani", Now: &standing},
	} {
		if err := CheckRename(ok); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
	for _, tc := range []struct {
		facts RenameFacts
		why   string
	}{
		{RenameFacts{Session: 3}, "session 3 is your own, started from no profile: only a private agent's session may be renamed"},
		{RenameFacts{Session: 4, Profile: "chani", Team: true, Now: &chani}, "session 4 is the team's: pocketd started it for a thread"},
		{RenameFacts{Session: 5, Profile: "chani", NoProfiles: true}, "pocketd runs without profiles, so session 5's profile chani is not private now"},
		{RenameFacts{Session: 6, Profile: "chani"}, "session 6's profile chani is gone or invalid now"},
		{RenameFacts{Session: 7, Profile: "vera", Now: &vera}, "session 7 is vera's, which is on the team"},
		{RenameFacts{Session: 8, Profile: "odrade", Now: &odrade}, "session 8 is odrade's, which is on the team"},
		{RenameFacts{Session: 9, Profile: "alia", Now: &alia}, "session 9 is supervisor alia's: a supervisor's session keeps its title"},
	} {
		err := CheckRename(tc.facts)
		if err == nil || !strings.HasPrefix(err.Error(), tc.why) || !strings.HasSuffix(err.Error(), "only a private agent's session may be renamed") {
			t.Errorf("%+v: %v, want %q", tc.facts, err, tc.why)
		}
	}
}
