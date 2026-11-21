package team

import (
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A private agent is the owner's alone (ADR-0042): its preamble names no
// team, and the team's tools and lessons refuse it; a team agent and a
// session from no profile pass.
func TestAPrivateAgentHasNoTeam(t *testing.T) {
	if got := PrivateIntro("Chani"); got != "You are Chani, the owner's personal agent: you work for the owner directly, in sessions the owner starts. "+
		"You have no supervisor, no team and no desk. Your profile follows." {
		t.Errorf("the preamble %q", got)
	}
	chani := profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Home: "/home/chani", Private: true}
	vera := profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent}
	if err := CheckTeamCaller(&chani); err == nil || !strings.HasPrefix(err.Error(), "chani is private, and team tools are not a private agent's") {
		t.Errorf("a private caller: %v", err)
	}
	if err := CheckTeamCaller(&vera); err != nil {
		t.Errorf("a team caller: %v", err)
	}
	if err := CheckTeamCaller(nil); err != nil {
		t.Errorf("a session from no profile: %v", err)
	}
	if err := CheckLessonsFor(chani); err == nil || !strings.Contains(err.Error(), "no lessons for a private agent") {
		t.Errorf("a private agent's lessons: %v", err)
	}
	if err := CheckLessonsFor(vera); err != nil {
		t.Errorf("a team agent's lessons: %v", err)
	}
}
