package team

import (
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Only a private agent's session, working as none of the roles, gets the
// schedule tool; a supervisor's keeps team_schedule. The tool's caller is
// checked again: a private agent's or a supervisor's session, working as
// none of the roles, and no other (ADR-0060).
func TestOnlyAPrivateAgentGetsTheScheduleTool(t *testing.T) {
	supervisor := &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}
	private := &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Private: true}
	privateSupervisor := &profiles.Profile{ID: "leto", Role: profiles.RoleSupervisor, Private: true}
	agent := &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}
	clerk := &profiles.Profile{ID: "clerk", Role: profiles.RoleClerk}
	for name, tc := range map[string]struct {
		p         *profiles.Profile
		role      SessionRole
		gets, may bool
	}{
		"a private agent":               {private, SessionRole{}, true, true},
		"a private supervisor":          {privateSupervisor, SessionRole{}, true, true},
		"the supervisor":                {supervisor, SessionRole{}, false, true},
		"an agent":                      {agent, SessionRole{}, false, false},
		"a clerk":                       {clerk, SessionRole{}, false, false},
		"no profile":                    {nil, SessionRole{}, false, false},
		"a thread's specialist":         {agent, SessionRole{Thread: true}, false, false},
		"a private thread's specialist": {private, SessionRole{Thread: true}, false, false},
		"a supervisor in a thread":      {supervisor, SessionRole{Thread: true}, false, false},
		"a private review":              {private, SessionRole{Thread: true, Review: true}, false, false},
		"a private worker":              {private, SessionRole{Worker: true}, false, false},
		"a worker":                      {nil, SessionRole{Worker: true}, false, false},
		"a private consult":             {private, SessionRole{Consult: true}, false, false},
		"a private gather's session":    {private, SessionRole{Gather: true}, false, false},
		"a private work session":        {private, SessionRole{Work: true}, false, false},
	} {
		if got := MaySchedule(tc.p, tc.role); got != tc.gets {
			t.Errorf("%s: MaySchedule %v, want %v", name, got, tc.gets)
		}
		if err := CheckScheduleCaller(tc.p, tc.role); (err == nil) != tc.may {
			t.Errorf("%s: CheckScheduleCaller %v, want allowed %v", name, err, tc.may)
		}
	}
	if err := CheckScheduleCaller(agent, SessionRole{}); err == nil || !strings.HasPrefix(err.Error(), "schedule is a private agent's and the supervisor's alone") {
		t.Errorf("an agent's refusal: %v", err)
	}
}
